"use strict";
// Resource Reservation Engine console. Plain JavaScript, no framework, no build step.
// Every number shown comes from the API; nothing here decides who gets a seat.

// ---------- helpers ----------
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const fmtN = (n) => (n ?? 0).toLocaleString();
// setHTML repaints an element only if its markup changed. The live refresh runs every second or two, and
// rewriting a section that holds a half-typed input or an open dropdown would wipe it. Returns true if it repainted.
function setHTML(el, html) {
  if (el.dataset.html === html) return false;
  el.dataset.html = html; el.innerHTML = html;
  return true;
}
const uid = () => Math.random().toString(36).slice(2, 10);
const fmtTime = (iso) => new Date(iso).toLocaleTimeString();
const secs = (ms) => Math.max(0, Math.ceil(ms / 1000));

const state = {
  route: "start", live: true, health: null, apiUp: null, prevMetrics: null, rate: null,
  eventId: Number(localStorage.getItem("eventId")) || null, rowSize: Number(localStorage.getItem("rowSize")) || 10,
  buyer: localStorage.getItem("buyer") || "guest-" + uid(),
  token: sessionStorage.getItem("adminToken") || "",
  book: { picked: new Set(), hold: null, busy: false, log: [], status: "" },
};
localStorage.setItem("buyer", state.buyer);

async function api(path, opts = {}) {
  const headers = { "Content-Type": "application/json", ...(opts.headers || {}) };
  if (opts.admin && state.token) headers.Authorization = "Bearer " + state.token;
  try {
    const r = await fetch(path, { method: opts.method || "GET", headers, body: opts.body ? JSON.stringify(opts.body) : undefined });
    let body = null;
    try { body = await r.json(); } catch { /* an empty body */ }
    return { ok: r.ok, status: r.status, body };
  } catch (e) {
    return { ok: false, status: 0, error: String(e) }; // the connection broke: the API is down or was killed
  }
}

function toast(msg, kind) {
  const t = document.createElement("div");
  t.className = "toast " + (kind || "");
  t.textContent = msg;
  $("#toasts").append(t);
  setTimeout(() => t.remove(), 4500);
}

function banner(html, bad) {
  const b = $("#banner");
  b.hidden = !html;
  b.className = "banner" + (bad ? " bad" : "");
  b.innerHTML = html || "";
}

const TIPS = {
  hold: "A temporary reservation of a seat. Nobody else can take it until it is confirmed, released, or it expires and someone takes it over.",
  version: "A counter that goes up every time a seat is held. A late request carrying an old version is recognised and refused, so a stale buyer cannot overwrite a newer one.",
  expiry: "A hold simply stops counting once its time has run out. Nothing sweeps it: a seat whose hold has expired is free for the next buyer.",
  gate: "A fast Redis check in front of Postgres that turns most losing buyers away cheaply. It is only a hint: Postgres still decides.",
  marker: "A mark written before the payment starts, so a retry of the same confirm cannot start a second payment while the first is still running.",
  arbiter: "Postgres alone decides who gets a seat. A unique index in the database refuses a second confirmed booking for any seat, whatever the application or Redis does.",
};
const term = (k, text) => `<span class="term" title="${esc(TIPS[k])}">${esc(text)}</span>`;

// ---------- drawer ----------
function openDrawer(kicker, title, html) {
  $("#drawer-kicker").textContent = kicker;
  $("#drawer-title").textContent = title;
  $("#drawer-body").innerHTML = html;
  $("#drawer").hidden = false;
  $("#drawer-backdrop").hidden = false;
}
function closeDrawer() { $("#drawer").hidden = true; $("#drawer-backdrop").hidden = true; }

// ---------- seat map ----------
// items: [{id, seq, state: free|held|confirmed, holder, version, expires_in_ms}]
function seatClass(it, opts, dense) {
  let cls = "seat " + (it.state === "confirmed" ? "sold" : it.state === "held" ? "held" : "");
  if (it.state === "held" && opts.mine?.has(it.id)) cls = "seat mine";
  if (opts.picked?.has(it.id)) cls = "seat picked";
  return (dense ? cls + " dense" : cls).trim();
}
const seatTip = (it) => `Seat ${it.seq}: ` + (it.state === "held" ? `held by ${it.holder || "?"}, version ${it.version}` : it.state === "confirmed" ? "booked" : "free");

function seatMapHTML(items, opts = {}) {
  const row = opts.rowSize || state.rowSize, dense = items.length > 400, now = performance.now();
  const cells = items.map((it) => {
    const sub = it.state === "held" && !dense ? `<small data-dl="${now + (it.expires_in_ms || 0)}">${secs(it.expires_in_ms || 0)}s</small>` : "";
    return `<button class="${seatClass(it, opts, dense)}" data-id="${it.id}" data-seq="${it.seq}" title="${esc(seatTip(it))}">${dense ? "" : it.seq}${sub}</button>`;
  });
  return `<div class="stage">Front</div><div class="seatmap" style="grid-template-columns: repeat(${row}, auto)">${cells.join("")}</div>`;
}

// renderSeatMap draws the map the first time and then PATCHES it. Rebuilding every seat on each refresh would
// detach the button under the mouse, so a click that lands during a refresh would be lost.
function renderSeatMap(container, items, opts = {}) {
  const row = opts.rowSize || state.rowSize, dense = items.length > 400, key = `${items.length}:${row}:${dense}`;
  const map = container.querySelector(".seatmap");
  if (!map || container.dataset.key !== key) { container.dataset.key = key; container.innerHTML = seatMapHTML(items, opts); return; }
  const now = performance.now();
  items.forEach((it, i) => {
    const b = map.children[i], cls = seatClass(it, opts, dense), tip = seatTip(it);
    if (b.className !== cls) b.className = cls;
    if (b.title !== tip) b.title = tip;
    b.dataset.id = it.id; b.dataset.seq = it.seq;
    let sm = b.querySelector("small");
    if (it.state === "held" && !dense) {
      if (!sm) { sm = document.createElement("small"); b.append(sm); }
      sm.dataset.dl = now + (it.expires_in_ms || 0); sm.textContent = secs(it.expires_in_ms || 0) + "s";
    } else if (sm) sm.remove();
  });
}

// Countdowns tick locally between refreshes: the numbers come from the database clock at the last refresh.
setInterval(() => {
  const now = performance.now();
  $$("[data-dl]").forEach((el) => {
    const left = Number(el.dataset.dl) - now;
    if (el.dataset.dl) el.textContent = secs(left) + "s";
    if (el.classList.contains("countdown")) el.classList.toggle("low", left < 5000);
  });
}, 500);

const legend = (mine) => `<div class="legend"><span><i class="lg-free"></i>Free</span><span><i class="lg-held"></i>Held by someone (seconds left)</span>` +
  (mine ? `<span><i class="lg-mine"></i>Held by you</span><span><i class="lg-picked"></i>Your selection</span>` : "") + `<span><i class="lg-sold"></i>Booked</span></div>`;

// ---------- events ----------
async function loadEvents() {
  const r = await api("/events");
  const list = r.body?.events || [];
  if (!state.eventId || !list.some((e) => e.id === state.eventId)) state.eventId = list[0]?.id || null;
  return list;
}
function eventSelect(list) {
  if (!list.length) return `<span class="muted">No events yet. Create one below.</span>`;
  return `<label class="f">Event<select id="ev-select">${list.map((e) => `<option value="${e.id}" ${e.id === state.eventId ? "selected" : ""}>#${e.id} ${esc(e.name)} (${e.free} free of ${e.capacity})</option>`).join("")}</select></label>`;
}
function bindEventSelect(el, after) {
  const s = $("#ev-select", el);
  if (s) s.onchange = () => { state.eventId = Number(s.value); localStorage.setItem("eventId", state.eventId); state.book.picked.clear(); after(); };
}
function createEventForm() {
  return `<div class="card"><h2>Create an event</h2><p class="muted small">An event is a set of numbered seats, one row in the database each.</p>
    <div class="row"><label class="f">Name<input type="text" id="ce-name" value="Demo show" size="18"></label>
    <label class="f">Seats<input type="number" id="ce-n" value="60" min="1" max="2000" style="width:90px"></label>
    <button class="btn btn-primary" id="ce-go">Create</button></div></div>`;
}
function bindCreateEvent(el, after) {
  const go = $("#ce-go", el);
  if (!go) return;
  go.onclick = async () => {
    const r = await api("/events", { method: "POST", body: { name: $("#ce-name", el).value || "Event", items: Number($("#ce-n", el).value) } });
    if (!r.ok) return toast(r.body?.error || "Could not create the event", "bad");
    state.eventId = r.body.event_id; localStorage.setItem("eventId", state.eventId);
    toast(`Event #${r.body.event_id} created with ${r.body.item_ids.length} seats`, "ok");
    after();
  };
}

// ---------- views ----------
const views = {};

// ===== Start =====
views.start = {
  title: "What is this?",
  desc: "A reservation engine for scarce items: many buyers, few seats. It holds a seat for a short time, takes a (simulated) payment, and never sells the same seat twice, even when processes crash.",
  render: () => `
    <div class="card"><h2>The idea in one minute</h2>
      <ol class="steps">
        <li>A buyer <b>joins the waiting room</b> when demand is far above what the database can take. It lets buyers in at a fixed rate, in the order they arrived.</li>
        <li>The buyer <b>holds</b> one to six seats. A ${term("hold", "hold")} is temporary: nobody else can take those seats while it lasts.</li>
        <li>The buyer <b>pays</b> (simulated). Success books the seats; failure or timeout releases them.</li>
        <li>A hold that nobody confirms just <b>expires</b>. No background job frees it, so no crashed process can leave a seat stuck.</li>
        <li>Only <b>${term("arbiter", "Postgres")}</b> decides who gets a seat. Redis (the fast ${term("gate", "gate")} and the waiting room) is a hint: if it is empty, stale or down, the answer is still correct, only slower.</li>
      </ol>
    </div>
    <div class="card diagram"><h2>How a booking flows, and where it can fail</h2>
      <svg viewBox="0 0 960 330" role="img" aria-label="Architecture diagram">
        <defs><marker id="ah" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M0 0L10 5L0 10z" fill="currentColor" style="color:var(--muted)"/></marker></defs>
        <rect class="box" x="20" y="130" width="110" height="60" rx="10"/><text x="75" y="156" text-anchor="middle">Buyer</text><text x="75" y="173" text-anchor="middle" class="muted">browser / load</text>
        <rect class="box" x="170" y="130" width="140" height="60" rx="10"/><text x="240" y="156" text-anchor="middle">Waiting room</text><text x="240" y="173" text-anchor="middle">FIFO queue, passes</text>
        <rect class="box" x="350" y="130" width="110" height="60" rx="10"/><text x="405" y="156" text-anchor="middle">API</text><text x="405" y="173" text-anchor="middle">hold, confirm</text>
        <rect class="box" x="500" y="130" width="150" height="60" rx="10"/><text x="575" y="156" text-anchor="middle">Strategy</text><text x="575" y="173" text-anchor="middle">lock / optimistic / gate</text>
        <rect class="box store" x="700" y="130" width="160" height="60" rx="10"/><text x="780" y="156" text-anchor="middle">Postgres</text><text x="780" y="173" text-anchor="middle">system of record</text>
        <rect class="box store" x="270" y="250" width="200" height="50" rx="10"/><text x="370" y="272" text-anchor="middle">Redis</text><text x="370" y="288" text-anchor="middle">queue + gate (a hint)</text>
        <rect class="box" x="490" y="30" width="170" height="50" rx="10"/><text x="575" y="52" text-anchor="middle">Payment</text><text x="575" y="68" text-anchor="middle">simulated</text>
        <path class="arrow" d="M130 160H168" marker-end="url(#ah)"/><path class="arrow" d="M310 160H348" marker-end="url(#ah)"/>
        <path class="arrow" d="M460 160H498" marker-end="url(#ah)"/><path class="arrow" d="M650 160H698" marker-end="url(#ah)"/>
        <path class="arrow" d="M405 130V80H488" marker-end="url(#ah)"/><path class="arrow" d="M240 190V250H268" marker-end="url(#ah)"/><path class="arrow" d="M575 190V225H470V250" marker-end="url(#ah)"/>
        <g class="badge-c"><circle cx="150" cy="144" r="10"/><text x="150" y="144">1</text></g>
        <g class="badge-c"><circle cx="329" cy="144" r="10"/><text x="329" y="144">2</text></g>
        <g class="badge-c"><circle cx="479" cy="144" r="10"/><text x="479" y="144">3</text></g>
        <g class="badge-c"><circle cx="675" cy="144" r="10"/><text x="675" y="144">4</text></g>
        <g class="badge-c"><circle cx="440" cy="58" r="10"/><text x="440" y="58">5</text></g>
        <g class="fail"><circle cx="560" cy="205" r="10"/><text x="560" y="205">A</text><title>Process dies after the gate claim, before the database: the claim lapses by itself.</title></g>
        <g class="fail"><circle cx="755" cy="112" r="10"/><text x="755" y="112">B</text><title>Process dies inside the transaction: Postgres rolls it back, nothing is left.</title></g>
        <g class="fail"><circle cx="405" cy="112" r="10"/><text x="405" y="112">C</text><title>Process dies after the commit, before the reply: the retry returns the same hold.</title></g>
        <g class="fail"><circle cx="672" cy="40" r="10"/><text x="672" y="40">D</text><title>Process dies during the payment: a marker stops a second payment until it goes stale.</title></g>
        <g class="fail"><circle cx="330" cy="236" r="10"/><text x="330" y="236">E</text><title>Redis restarts empty: requests reach Postgres, which stays correct.</title></g>
        <g class="fail"><circle cx="840" cy="112" r="10"/><text x="840" y="112">F</text><title>Postgres restarts: in-flight transactions are lost; the checker judges the result.</title></g>
      </svg>
      <p class="muted small">Numbers are the normal flow (1 join, 2 admitted, 3 hold, 4 the database decides, 5 payment). Letters are failure points: hover one. They map to the rows of <span class="mono">docs/crash-analysis.md</span>; the <a href="#/failures">Failures</a> page can trigger them.</p>
    </div>
    <div class="grid g2">
      <div class="card"><h2>Where to look</h2>
        <p><b>Live seat map</b> shows every seat changing state as real traffic arrives, with hold countdowns.</p>
        <p><b>Book seats</b> lets you hold and pay yourself. <b>Surge</b> sends a crowd. <b>Failures</b> kills the API at an exact moment. <b>Checker</b> proves, from the database alone, that nothing was sold twice.</p></div>
      <div class="card"><h2>Words</h2>
        <p>${term("hold", "Hold")} · ${term("version", "Version")} · ${term("expiry", "Expiry")} · ${term("gate", "Gate")} · ${term("marker", "Payment marker")} · ${term("arbiter", "Arbiter")}</p>
        <p class="muted small">Hover a word for its meaning. Times in the seat map come from the database clock, not from your browser.</p></div>
    </div>`,
};

// ===== Overview =====
views.overview = {
  title: "Overview",
  desc: "A live picture of the engine: how traffic is being answered, how busy the database pool is, and what each strategy counted.",
  render: () => `<div id="ov"></div>`,
  async refresh(el) {
    const [m, ev] = await Promise.all([api("/metrics"), api("/events")]);
    if (!m.ok) { el.innerHTML = `<div class="note warn">The API did not answer.</div>`; return; }
    const c = m.body.api.counters, st = m.body.strategy || {}, out = m.body.outcomes || {};
    const sum = (re) => Object.entries(c).filter(([k]) => re.test(k)).reduce((a, [, v]) => a + v, 0);
    const holds = { held: sum(/^requests\{POST \/holds,201\}/), unavailable: sum(/^requests\{POST \/holds,409\}/), busy: sum(/^requests\{POST \/holds,503\}/), refused: sum(/^requests\{POST \/holds,403\}/) };
    const total = holds.held + holds.unavailable + holds.busy + holds.refused;
    const prev = state.prevMetrics, now = performance.now();
    if (prev) state.rate = { holds: (total - prev.total) / ((now - prev.t) / 1000) };
    state.prevMetrics = { total, t: now };
    const lat = m.body.api.histograms["latency{POST /holds}"];
    const strat = Object.entries(st).filter(([k]) => !k.startsWith("db_pool") && !k.startsWith("room_")).map(([k, v]) => `<tr><td class="mono">${esc(k)}</td><td>${fmtN(v)}</td></tr>`).join("");
    const pool = (k) => st["db_pool_" + k] ?? 0;
    el.innerHTML = `
      <div class="grid g4">
        <div class="tile"><div class="k">Hold requests answered</div><div class="v">${fmtN(total)}</div><div class="s">${state.rate ? state.rate.holds.toFixed(1) + " per second right now" : "measuring…"}</div></div>
        <div class="tile"><div class="k">Holds granted</div><div class="v">${fmtN(holds.held)}</div><div class="s">${fmtN(holds.unavailable)} unavailable · ${fmtN(holds.busy)} lock busy</div></div>
        <div class="tile"><div class="k">Bookings confirmed</div><div class="v">${fmtN((out.confirmed_in_time || 0) + (out.confirmed_after_expiry || 0))}</div><div class="s">${fmtN(out.confirmed_after_expiry || 0)} after the hold expired · ${fmtN(out.paid_but_lost || 0)} paid but lost</div></div>
        <div class="tile"><div class="k">Requests in flight</div><div class="v">${fmtN(m.body.inflight)}</div><div class="s">peak ${fmtN(m.body.max_inflight)}</div></div>
      </div>
      <div class="grid g2">
        <div class="card"><h2>Hold latency (approximate)</h2>
          <div class="kv"><div>Median, at most</div><div>${lat ? esc(lat.p50_ms_upper_bound) + " ms" : "–"}</div><div>99th percentile, at most</div><div>${lat ? esc(lat.p99_ms_upper_bound) + " ms" : "–"}</div><div>Requests measured</div><div>${fmtN(lat?.count)}</div></div>
          <p class="muted small">These come from fixed buckets inside the server, so they are upper bounds, good for seeing a trend. The benchmark numbers are taken from the load generator instead.</p></div>
        <div class="card"><h2>Database connections</h2>
          <div class="kv"><div>Pool size</div><div>${fmtN(pool("max_conns"))}</div><div>In use / idle</div><div>${fmtN(pool("acquired_conns"))} / ${fmtN(pool("idle_conns"))}</div><div>Requests that had to wait for a connection</div><div>${fmtN(pool("acquires_that_waited"))} of ${fmtN(pool("acquires_total"))}</div></div>
          <p class="muted small">A growing "had to wait" count means the pool, not the engine, is the limit.</p></div>
      </div>
      <div class="grid g2">
        <div class="card"><h2>This strategy counted</h2><div class="table-wrap"><table><tbody>${strat || `<tr><td class="muted">nothing yet</td></tr>`}</tbody></table></div></div>
        <div class="card"><h2>Events</h2><div class="table-wrap"><table><thead><tr><th>Event</th><th>Seats</th><th>State</th></tr></thead><tbody>${(ev.body?.events || []).map((e) => `<tr class="click" data-ev="${e.id}"><td>#${e.id} ${esc(e.name)}</td><td>${e.capacity}</td><td><div class="bar" title="${e.confirmed} booked, ${e.held} held, ${e.free} free"><span class="b-conf" style="width:${e.confirmed / e.capacity * 100}%"></span><span class="b-held" style="width:${e.held / e.capacity * 100}%"></span><span class="b-free" style="width:${e.free / e.capacity * 100}%"></span></div></td></tr>`).join("") || `<tr><td class="muted" colspan="3">No events yet</td></tr>`}</tbody></table></div></div>
      </div>`;
    $$("tr[data-ev]", el).forEach((tr) => (tr.onclick = () => { state.eventId = Number(tr.dataset.ev); localStorage.setItem("eventId", state.eventId); location.hash = "#/inventory"; }));
  },
};

// ===== Live seat map =====
views.inventory = {
  title: "Live seat map",
  desc: "Every seat of an event, updating as traffic arrives. Hold countdowns are computed from the database clock. A hold that has run out shows as free, because that is how the engine treats it.",
  every: 1000,
  render: () => `<div class="card"><div class="row" id="inv-head"></div><div id="inv-sum" class="chips"></div>${legend(false)}<div id="inv-map"></div></div>`,
  async refresh(el) {
    const list = await loadEvents();
    if (setHTML($("#inv-head", el), eventSelect(list) + `<label class="f">Seats per row<select id="rowsize">${[8, 10, 12, 20, 40].map((n) => `<option ${n === state.rowSize ? "selected" : ""}>${n}</option>`).join("")}</select></label>`)) {
      bindEventSelect(el, () => views.inventory.refresh(el));
      $("#rowsize", el).onchange = (e) => { state.rowSize = Number(e.target.value); localStorage.setItem("rowSize", state.rowSize); views.inventory.refresh(el); };
    }
    if (!state.eventId) { $("#inv-map", el).innerHTML = `<p class="muted">Create an event on the Book page to see it here.</p>`; return; }
    const r = await api(`/events/${state.eventId}/items`);
    if (!r.ok) { $("#inv-map", el).innerHTML = `<div class="note warn">Could not read the event.</div>`; return; }
    const items = r.body.items, n = (s) => items.filter((i) => i.state === s).length;
    $("#inv-sum", el).innerHTML = `<span class="chip">${n("free")} free</span><span class="chip">${n("held")} held</span><span class="chip">${n("confirmed")} booked</span>` + (r.body.truncated ? `<span class="chip">showing the first ${items.length}</span>` : "");
    renderSeatMap($("#inv-map", el), items);
    $("#inv-map", el).onclick = (e) => {
      const b = e.target.closest(".seat"); if (!b) return;
      const it = items.find((x) => x.id === Number(b.dataset.id));
      openDrawer("Seat", "Seat " + it.seq, `<div class="kv"><div>State</div><div><span class="badge ${it.state === "free" ? "ok" : it.state === "held" ? "warn" : ""}">${esc(it.state)}</span></div>
        <div>Held by</div><div>${esc(it.holder || "–")}</div><div>${term("version", "Version")}</div><div>${it.version}</div>
        <div>Time left</div><div>${it.expires_in_ms != null ? secs(it.expires_in_ms) + " s (database clock)" : "–"}</div><div>Row id</div><div class="mono">${it.id}</div></div>
        <p class="muted small">Version ${it.version} means this seat has been held ${it.version} time(s) since it was created.</p>`);
    };
  },
};

// ===== Bookings =====
views.bookings = {
  title: "Bookings",
  desc: "The most recent confirmed bookings of an event, straight from the ledger. The ledger has a unique index per seat, so no seat can appear twice.",
  render: () => `<div class="card"><div class="row" id="bk-head"></div><div class="table-wrap"><table><thead><tr><th>Booking</th><th>Seat</th><th>Buyer</th><th>Version</th><th>Booked at</th></tr></thead><tbody id="bk-body"></tbody></table></div></div>`,
  async refresh(el) {
    const list = await loadEvents();
    if (setHTML($("#bk-head", el), eventSelect(list))) bindEventSelect(el, () => views.bookings.refresh(el));
    if (!state.eventId) return;
    const r = await api(`/events/${state.eventId}/bookings?limit=100`);
    const rows = r.body?.bookings || [];
    $("#bk-body", el).innerHTML = rows.map((b) => `<tr class="click" data-i="${b.id}"><td class="mono">#${b.id}</td><td>${b.seat}</td><td>${esc(b.buyer)}</td><td>${b.hold_version}</td><td>${fmtTime(b.at)}</td></tr>`).join("") || `<tr><td class="muted" colspan="5">No bookings yet</td></tr>`;
    $$("tr[data-i]", el).forEach((tr) => (tr.onclick = () => {
      const b = rows.find((x) => x.id === Number(tr.dataset.i));
      openDrawer("Booking", "#" + b.id, `<div class="kv"><div>Seat</div><div>${b.seat}</div><div>Buyer</div><div>${esc(b.buyer)}</div><div>Hold version</div><div>${b.hold_version}</div><div>Booked at</div><div>${new Date(b.at).toLocaleString()}</div></div>
        <p class="muted small">The booking remembers the buyer and the hold version, so a retried confirm is recognised instead of booking twice.</p>`);
    }));
  },
};

// ===== Waiting room =====
views.room = {
  title: "Waiting room",
  desc: "Buyers queue in the order they arrive and are let in at a fixed rate. The queue lives in Redis, so it works across several API instances. It only limits how fast buyers reach the database; Postgres still decides who gets a seat.",
  render: () => `<div id="rm-stats"></div><div class="grid g2">
    <div class="card"><h2>What it guarantees</h2><ul>
      <li>Admission order equals join order for buyers who keep waiting (first come, first in).</li>
      <li>Joining twice keeps your original place.</li>
      <li>At most the configured rate is admitted per second, however many API instances run.</li>
      <li>It can never cause a double sale: the database still decides.</li></ul>
      <h3>What it does not guarantee</h3><ul><li>Arrival order at the network, or fairness for buyers who leave and rejoin.</li><li>Protection against one person using many buyer ids.</li><li>That an admitted buyer gets a seat: seats are scarce.</li></ul></div>
    <div class="card"><h2>Try the queue</h2><p class="muted small">Join as a buyer and watch your position fall.</p>
      <div class="row"><label class="f">Buyer id<input type="text" id="rm-buyer" value="${esc(state.buyer)}" size="18"></label><button class="btn btn-primary" id="rm-join">Join the queue</button></div><div id="rm-status" class="note" hidden></div></div></div>`,
  async refresh(el) {
    const [h, m] = await Promise.all([api("/healthz"), api("/metrics")]);
    const on = h.body?.waiting_room, st = m.body?.strategy || {};
    const ctr = (k) => fmtN(st["room_" + k]);
    setHTML($("#rm-stats", el), `${on ? "" : `<div class="note warn">The waiting room is off on this server. Start it with <span class="mono">ROOM_RATE=&lt;buyers per second&gt;</span>.</div>`}
      <div class="grid g4">
        <div class="tile"><div class="k">Waiting now</div><div class="v">${ctr("queue_length")}</div><div class="s">queue depth</div></div>
        <div class="tile"><div class="k">Admitted</div><div class="v">${ctr("admitted")}</div><div class="s">${ctr("joins")} joins, ${ctr("retry_full")} turned away (full)</div></div>
        <div class="tile"><div class="k">Passes checked</div><div class="v">${ctr("pass_checks")}</div><div class="s">${ctr("pass_rejected")} refused</div></div>
        <div class="tile"><div class="k">Let through (Redis down)</div><div class="v">${fmtN(st.room_fail_open)}</div><div class="s">fail-open count</div></div>
      </div>`);
    const join = $("#rm-join", el);
    join.disabled = !on;
    join.onclick = async () => {
      const buyer = $("#rm-buyer", el).value.trim() || state.buyer, box = $("#rm-status", el); box.hidden = false;
      const token = await passFor(buyer, (t) => (box.textContent = t));
      box.textContent = token ? `Admitted. You hold a pass (${token.slice(0, 8)}…). It lets you place holds for a few minutes.` : "Not admitted.";
    };
  },
};

// Join the waiting room and wait to be admitted. Returns the pass token, "" if not admitted.
async function passFor(buyer, say) {
  say = say || (() => {});
  let r = await api("/queue/join", { method: "POST", body: { buyer_id: buyer } }), rejoins = 0;
  for (let i = 0; i < 600; i++) {
    const b = r.body || {};
    if (r.status === 429) { say("The waiting room is full: try again shortly."); return ""; }
    if (r.ok && b.state === "admitted") return b.token;
    if (r.status === 404 && rejoins++ < 3) { r = await api("/queue/join", { method: "POST", body: { buyer_id: buyer } }); continue; }
    if (r.ok && b.state === "queued") say(`You are number ${b.position} in line (ticket ${b.ticket}). Checking again in ${Math.round((b.retry_after_ms || 500) / 100) / 10}s.`);
    else if (!r.ok) { say("The waiting room did not answer."); return ""; }
    await sleep(Math.min(Math.max(b.retry_after_ms || 500, 150), 2000));
    r = await api("/queue/" + encodeURIComponent(buyer));
  }
  return "";
}

// ===== Health =====
views.health = {
  title: "Health",
  desc: "Whether the stores answer, how the server was started, and which safety switches are on.",
  render: () => `<div id="hl"></div>`,
  async refresh(el) {
    const [h, m] = await Promise.all([api("/healthz"), api("/metrics")]);
    if (!h.body) { el.innerHTML = `<div class="note warn">The API is not answering.</div>`; return; }
    const b = h.body, cfg = b.config || {}, st = m.body?.strategy || {};
    const pill = (s) => (s === undefined ? `<span class="badge">not used</span>` : s === "ok" ? `<span class="badge ok">ok</span>` : `<span class="badge bad">${esc(s)}</span>`);
    el.innerHTML = `<div class="grid g2">
      <div class="card"><h2>Stores</h2><div class="kv"><div>Postgres (system of record)</div><div>${pill(b.postgres)}</div><div>Redis (gate and queue, a hint)</div><div>${pill(b.redis)}</div></div>
        <p class="muted small">Redis down is a degraded state, not an outage: requests fail open to Postgres, which stays correct.</p></div>
      <div class="card"><h2>How this server was started</h2><div class="kv">${Object.entries(cfg).map(([k, v]) => `<div>${esc(k.replace(/_/g, " "))}</div><div class="mono">${esc(v)}</div>`).join("")}</div></div>
      <div class="card"><h2>Safety switches</h2><div class="kv"><div>Fault injection</div><div>${b.faults_enabled ? `<span class="badge warn">ENABLED: this server can be made to crash</span>` : `<span class="badge ok">off</span>`}</div>
        <div>Admin token configured</div><div>${b.admin_token_configured ? `<span class="badge ok">yes</span>` : `<span class="badge">no</span>`}</div><div>Waiting room</div><div>${b.waiting_room ? "on" : "off"}</div></div>
        <p class="muted small">Benchmarks refuse to run while fault injection is enabled.</p></div>
      <div class="card"><h2>Connection pool</h2><div class="kv"><div>Size</div><div>${fmtN(st.db_pool_max_conns)}</div><div>In use</div><div>${fmtN(st.db_pool_acquired_conns)}</div><div>Waited for a connection</div><div>${fmtN(st.db_pool_acquires_that_waited)} of ${fmtN(st.db_pool_acquires_total)}</div></div></div></div>`;
  },
};

// ===== Book =====
const OUTCOME_TEXT = {
  unavailable: ["bad", "Someone else holds or has booked one of those seats. Nothing was held for you, not even the free ones: a request is all or nothing."],
  lock_busy: ["warn", "Another buyer was reserving the same seat at that very instant. Try again: it will most likely say the seat is taken."],
  not_admitted: ["warn", "The waiting room needs to admit you first."],
  hold_lost: ["bad", "Your hold is no longer yours: another buyer took the seat after your hold ran out. If you had paid, that payment is counted as paid-but-lost (simulated, nothing is refunded)."],
  payment_failed: ["warn", "The payment failed, so the seats were released."],
  payment_timeout: ["warn", "The payment timed out, so the seats were released. (In real life the outcome of a timeout is unknown, which is a documented limitation.)"],
  payment_in_progress: ["warn", "A payment for this hold is already running. Wait for it instead of paying twice."],
};
views.book = {
  title: "Book seats",
  desc: "Pick up to six seats, hold them, then pay. This is the same two-step flow a real buyer goes through: select, then pay.",
  every: 1000,
  render: () => `<div class="split"><div class="card"><div class="row" id="bk-ev"></div>${legend(true)}<div id="bk-map"></div></div>
    <div><div class="card ticket" id="bk-panel"></div>${createEventForm()}</div></div>`,
  async refresh(el) {
    if (state.book.busy) return; // do not redraw under an action in progress
    const list = await loadEvents();
    if (setHTML($("#bk-ev", el), eventSelect(list))) bindEventSelect(el, () => views.book.refresh(el));
    bindCreateEvent(el, () => views.book.refresh(el));
    if (!state.eventId) { $("#bk-map", el).innerHTML = `<p class="muted">Create an event to start.</p>`; renderBookPanel(el, []); return; }
    const r = await api(`/events/${state.eventId}/items`);
    if (!r.ok) return;
    const items = r.body.items, mine = new Set((state.book.hold?.items || []).map((i) => i.id));
    renderSeatMap($("#bk-map", el), items, { picked: state.book.picked, mine });
    $("#bk-map", el).onclick = (e) => {
      const b = e.target.closest(".seat"); if (!b || state.book.hold) return;
      const id = Number(b.dataset.id), it = items.find((x) => x.id === id);
      if (it.state !== "free") return toast(`Seat ${it.seq} is not free`, "bad");
      const p = state.book.picked;
      if (p.has(id)) p.delete(id); else if (p.size >= 6) return toast("At most six seats per booking", "bad"); else p.add(id);
      views.book.refresh(el);
    };
    renderBookPanel(el, items);
  },
};
function renderBookPanel(el, items) {
  const b = state.book, panel = $("#bk-panel", el), seq = (id) => items.find((i) => i.id === id)?.seq ?? id;
  const log = b.log.slice(-4).map((l) => `<div class="note ${l.kind}">${l.text}</div>`).join("");
  if (b.hold) {
    const dl = b.hold.deadline;
    setHTML(panel, `<h2>Your hold</h2><div class="chips">${b.hold.items.map((i) => `<span class="chip">Seat ${i.seq}</span>`).join("")}</div>
      <div class="countdown" data-dl="${dl}">${secs(dl - performance.now())}s</div><div class="muted small">left on your ${term("hold", "hold")} (a soft limit: if nobody takes your seats, paying late still works)</div>
      <div class="kv" style="margin-top:8px"><div>${term("version", "Version")}</div><div>${b.hold.items.map((i) => i.version).join(", ")}</div><div>Request id</div><div class="mono">${esc(b.hold.requestId)}</div></div>
      <div class="btn-row"><button class="btn btn-primary" id="bk-pay" ${b.busy ? "disabled" : ""}>Pay (simulated)</button><button class="btn" id="bk-rel" ${b.busy ? "disabled" : ""}>Release</button><button class="btn btn-ghost" id="bk-retry" title="Sends exactly the same hold request again, as a client would after a lost reply. You get the same hold back, not a second one.">Retry the hold request</button></div>
      <div class="muted small">${esc(b.status)}</div>${log}`);
    $("#bk-pay", panel).onclick = () => payHold(el); $("#bk-rel", panel).onclick = () => releaseHold(el); $("#bk-retry", panel).onclick = () => retryHold(el);
    return;
  }
  const n = b.picked.size;
  if (!setHTML(panel, `<h2>Your selection</h2>${n ? `<div class="chips">${[...b.picked].map((id) => `<span class="chip">Seat ${seq(id)}</span>`).join("")}</div>` : `<p class="muted">Click free seats on the map (up to six).</p>`}
    <label class="f">Buyer id<input type="text" id="bk-buyer" value="${esc(state.buyer)}" size="20" title="Who you are. Two browsers with different ids compete for the same seats."></label>
    <div class="btn-row"><button class="btn btn-primary" id="bk-hold" ${n && !b.busy ? "" : "disabled"}>Hold ${n || ""} seat${n === 1 ? "" : "s"}</button><button class="btn btn-ghost" id="bk-clear" ${n ? "" : "disabled"}>Clear</button></div>
    <div class="muted small">${esc(b.status)}</div>${log}`)) return; // nothing changed: keep what the buyer is typing
  $("#bk-buyer", panel).onchange = (e) => { state.buyer = e.target.value.trim() || state.buyer; localStorage.setItem("buyer", state.buyer); };
  $("#bk-hold", panel).onclick = () => holdSelected(el, items); $("#bk-clear", panel).onclick = () => { b.picked.clear(); views.book.refresh(el); };
}
function note(kind, text) { state.book.log.push({ kind, text }); }
async function holdSelected(el, items) {
  const b = state.book; b.busy = true; b.status = "";
  const ids = [...b.picked], requestId = "req-" + uid();
  let token = "";
  if (state.health?.waiting_room) { token = await passFor(state.buyer, (t) => { b.status = t; renderBookPanel(el, items); }); if (!token) { b.busy = false; note("warn", "You were not admitted by the waiting room."); return views.book.refresh(el); } }
  b.status = ""; // the queue message has done its job
  const r = await api("/holds", { method: "POST", headers: token ? { "X-Admission-Token": token } : {}, body: { buyer_id: state.buyer, request_id: requestId, item_ids: ids } });
  b.busy = false;
  if (r.status === 201) {
    b.hold = { requestId, ids, items: r.body.items.map((i) => ({ id: i.item_id, version: i.hold_version, seq: items.find((x) => x.id === i.item_id)?.seq ?? i.item_id })), deadline: performance.now() + 60000, token };
    const ttl = await api(`/events/${state.eventId}/items`); const mine = ttl.body?.items?.find((x) => x.id === ids[0]);
    if (mine?.expires_in_ms) b.hold.deadline = performance.now() + mine.expires_in_ms;
    b.picked.clear(); note("good", `Held ${ids.length} seat(s). Pay before the countdown ends, or someone else may take them.`);
  } else if (r.status === 0) note("bad", "The connection broke before an answer came back. The hold may or may not exist: use <b>Retry the hold request</b> to find out safely.");
  else { const [kind, text] = OUTCOME_TEXT[r.body?.outcome] || ["bad", esc(r.body?.error || "Unexpected answer " + r.status)]; note(kind, text); b.picked.clear(); }
  views.book.refresh(el);
}
async function payHold(el) {
  const b = state.book; b.busy = true; b.status = "Paying… (the simulated payment takes a moment)"; renderBookPanel(el, []);
  const r = await api("/holds/confirm", { method: "POST", body: { buyer_id: state.buyer, items: b.hold.items.map((i) => ({ item_id: i.id, hold_version: i.version })) } });
  b.busy = false; b.status = "";
  if (r.status === 200) { note("good", r.body.replayed ? "Already booked: this was a retry, and no second booking or payment was made." : r.body.after_expiry ? "Booked. Your hold had expired, but nobody took the seats, so your payment still won." : "Booked. Enjoy the show."); b.hold = null; toast("Booking confirmed", "ok"); }
  else if (r.status === 0) note("bad", "The connection broke during the payment. Nothing is lost: press Pay again. If the server says a payment is in progress, wait a few seconds.");
  else { const [kind, text] = OUTCOME_TEXT[r.body?.outcome] || ["bad", "Unexpected answer " + r.status]; note(kind, text); if (["hold_lost", "payment_failed", "payment_timeout"].includes(r.body?.outcome)) b.hold = null; }
  views.book.refresh(el);
}
async function releaseHold(el) {
  const b = state.book, r = await api("/holds/release", { method: "POST", body: { buyer_id: state.buyer, items: b.hold.items.map((i) => ({ item_id: i.id, hold_version: i.version })) } });
  note(r.ok ? "good" : "bad", r.ok ? (r.body.outcome === "released" ? "Released: the seats are free again." : "Nothing to release: the hold was already gone.") : "Release failed");
  b.hold = null; views.book.refresh(el);
}
async function retryHold(el) {
  const b = state.book, r = await api("/holds", { method: "POST", headers: b.hold.token ? { "X-Admission-Token": b.hold.token } : {}, body: { buyer_id: state.buyer, request_id: b.hold.requestId, item_ids: b.hold.ids } });
  if (r.status === 201) note("good", `Same request sent again: you got the SAME hold back (versions ${r.body.items.map((i) => i.hold_version).join(", ")}). A retry never creates a second hold.`);
  else note("bad", "The retry was answered with " + (r.body?.outcome || r.status));
  views.book.refresh(el);
}

// ===== Surge =====
// A crowd in the browser: each buyer holds a contiguous block (hot seats first) and pays. Browsers limit parallel
// connections, so this is a demonstration, not a benchmark: the benchmarks use a load generator.
function zipfPicker(n, s) {
  const cum = []; let t = 0;
  for (let r = 1; r <= n; r++) { t += Math.pow(r, -s); cum.push(t); }
  return (size) => { const fits = n - size + 1, u = Math.random() * cum[fits - 1]; let lo = 0, hi = fits - 1; while (lo < hi) { const mid = (lo + hi) >> 1; if (cum[mid] >= u) hi = mid; else lo = mid + 1; } return Array.from({ length: size }, (_, i) => lo + i); };
}
views.surge = {
  title: "Surge",
  desc: "Send a crowd at an event and watch the seat map. Buyers want blocks of one to six seats, and demand is skewed: a few hot seats get most of the requests. This runs in your browser, so it is a demonstration, not a benchmark.",
  render: () => `<div class="grid g2"><div class="card"><h2>Send a crowd</h2><div class="row" id="sg-ev"></div>
    <div class="row" style="margin-top:10px"><label class="f">Buyers<input type="number" id="sg-n" value="120" min="10" max="400" style="width:90px"></label>
    <label class="f">Over (seconds)<input type="number" id="sg-t" value="8" min="2" max="60" style="width:90px"></label>
    <label class="f" title="0 spreads demand evenly. Higher values concentrate it on a few hot seats.">Skew<input type="number" id="sg-s" value="1.1" step="0.1" min="0" max="2" style="width:80px"></label></div>
    <div class="btn-row"><button class="btn btn-primary" id="sg-go">Start</button></div><div class="progress" style="margin-top:12px"><span id="sg-bar" style="width:0"></span></div><div id="sg-sum" class="muted small" style="margin-top:6px"></div></div>
    <div class="card"><h2>Outcome</h2><div id="sg-out" class="kv"></div><p class="muted small">When it ends, open the <a href="#/inventory">seat map</a>, then run the <a href="#/checker">checker</a> to confirm nothing was sold twice.</p></div></div>${createEventForm()}`,
  async refresh(el) {
    if (el.dataset.running) return;
    const list = await loadEvents();
    if (setHTML($("#sg-ev", el), eventSelect(list))) bindEventSelect(el, () => views.surge.refresh(el));
    bindCreateEvent(el, () => views.surge.refresh(el));
    $("#sg-go", el).onclick = () => runSurge(el);
  },
};
async function runSurge(el) {
  if (!state.eventId) return toast("Create an event first", "bad");
  const n = Number($("#sg-n", el).value), over = Number($("#sg-t", el).value) * 1000, skew = Number($("#sg-s", el).value);
  const items = (await api(`/events/${state.eventId}/items`)).body.items.map((i) => i.id);
  const pick = zipfPicker(items.length, skew), sizes = [1, 2, 2, 3, 4, 6, 2, 2, 1, 3];
  const tally = { held: 0, unavailable: 0, busy: 0, confirmed: 0, lost: 0, failed: 0, errors: 0, turnedAway: 0 };
  let done = 0; el.dataset.running = "1";
  const show = () => {
    $("#sg-bar", el).style.width = (done / n * 100) + "%"; $("#sg-sum", el).textContent = `${done} of ${n} buyers finished`;
    $("#sg-out", el).innerHTML = Object.entries({ "Holds granted": tally.held, "Seats taken by others": tally.unavailable, "Lock busy": tally.busy, "Bookings confirmed": tally.confirmed, "Paid but lost": tally.lost, "Payment failed or timed out": tally.failed, "Turned away by the room": tally.turnedAway, "Errors": tally.errors }).map(([k, v]) => `<div>${k}</div><div>${v}</div>`).join("");
  };
  const buyer = async (i) => {
    const id = `surge-${uid()}-${i}`, ids = pick(sizes[i % sizes.length]).map((k) => items[k]);
    try {
      let token = "";
      if (state.health?.waiting_room) { token = await passFor(id); if (!token) { tally.turnedAway++; return; } }
      let r, attempts = 0;
      do { r = await api("/holds", { method: "POST", headers: token ? { "X-Admission-Token": token } : {}, body: { buyer_id: id, request_id: id + "-h", item_ids: ids } }); if (r.status !== 503) break; await sleep(20 * 2 ** attempts + Math.random() * 20); } while (++attempts < 4);
      if (r.status === 503) { tally.busy++; return; }
      if (r.status === 409) { tally.unavailable++; return; }
      if (r.status !== 201) { tally.errors++; return; }
      tally.held++;
      const c = await api("/holds/confirm", { method: "POST", body: { buyer_id: id, items: r.body.items.map((x) => ({ item_id: x.item_id, hold_version: x.hold_version })) } });
      if (c.status === 200) tally.confirmed++; else if (c.body?.outcome === "hold_lost") tally.lost++; else if (c.status === 402) tally.failed++; else tally.errors++;
    } finally { done++; show(); }
  };
  show();
  const jobs = Array.from({ length: n }, (_, i) => sleep(i * over / n).then(() => buyer(i)));
  await Promise.all(jobs); delete el.dataset.running; toast("The crowd has finished", "ok");
}

// ===== Failures =====
const FAULT_INFO = {
  "hold.after_gate_claim": ["6", "after the Redis claim, before the database (fast path only)"], "hold.before_commit": ["8", "inside the reserve transaction, before the commit"],
  "hold.before_reply": ["9", "after the hold committed, before the reply"], "hold.before_gate_extend": ["10", "after the commit, before the claim is extended (fast path only)"],
  "confirm.after_marker": ["12", "after the payment marker is written, before the payment"], "confirm.during_payment": ["13", "in the middle of the payment"],
  "confirm.after_payment": ["15", "after the payment succeeded, before the booking"], "confirm.before_commit": ["16", "inside the confirm transaction, before the commit"],
  "confirm.before_reply": ["17", "after the booking committed, before the reply"], "gate.sold_marker": ["18", "writing the sold marker at the gate fails"],
  "payment.delay": ["12-15", "slows the payment down"], "confirm.skip_payment_marker": ["control", "switches the payment marker OFF (the negative control)"],
};
views.failures = {
  title: "Failures",
  desc: "Make the server fail at an exact moment and see what is left behind. A crash is a real process exit. These controls need the server started with FAULTS_ENABLED=true and the admin token.",
  render: () => `<div id="fl-gate"></div><div class="grid g2"><div class="card"><h2>Guided demos</h2>
      <p>Each demo does the whole thing for you: it arms a crash, drives a request, watches the server die, waits for it to come back, retries like a client would, and explains what it sees.</p>
      <div class="btn-row"><button class="btn btn-primary" id="fl-d9">Killed before the hold reply</button><button class="btn btn-primary" id="fl-d17">Killed before the booking reply</button></div>
      <p class="muted small">They need the server to restart by itself (run it with <span class="mono">scripts/dev-api</span>).</p><div id="fl-log"></div></div>
    <div class="card" id="fl-armed"></div></div><div class="card" id="fl-table"></div>`,
  async refresh(el) {
    const h = await api("/healthz");
    const f = state.token ? await api("/admin/faults", { admin: true }) : { status: 0, body: null };
    const b = h.body || {};
    let gate = "";
    if (!b.faults_enabled) gate = `<div class="note warn">Fault injection is off on this server. Start it with <span class="mono">FAULTS_ENABLED=true ADMIN_TOKEN=&lt;token&gt;</span>, ideally under <span class="mono">scripts/dev-api</span> so it restarts after a crash.</div>`;
    else if (!state.token) gate = `<div class="note warn">Enter the admin token in the top bar.</div>`;
    else if (f.status === 401) gate = `<div class="note warn">The admin token was refused.</div>`;
    setHTML($("#fl-gate", el), gate);
    const armed = f.body?.faults || [], known = f.body?.known || {};
    setHTML($("#fl-armed", el), `<h2>Armed right now</h2>${armed.length ? `<table><thead><tr><th>Point</th><th>Action</th><th>Left</th></tr></thead><tbody>${armed.map((a) => `<tr><td class="mono">${esc(a.point)}</td><td>${esc(a.action)}</td><td>${a.remaining ?? "∞"}</td></tr>`).join("")}</tbody></table>` : `<p class="muted">Nothing armed.</p>`}
      <div class="btn-row"><button class="btn btn-danger" id="fl-clear">Clear all</button></div>`);
    if (setHTML($("#fl-table", el), `<h2>Every failure point</h2><div class="table-wrap"><table><thead><tr><th>Point</th><th>Row</th><th>Where</th><th>Arm it</th></tr></thead><tbody>${Object.keys(known).sort().map((p) => `<tr><td class="mono">${esc(p)}</td><td>${esc(FAULT_INFO[p]?.[0] || "")}</td><td>${esc(FAULT_INFO[p]?.[1] || "")}</td><td>${(known[p] || []).map((a) => `<button class="btn" data-arm="${esc(p)}" data-action="${esc(a)}">${esc(a)} once</button>`).join(" ")}</td></tr>`).join("") || `<tr><td class="muted" colspan="4">Unlocks once faults are enabled and the token is entered.</td></tr>`}</tbody></table></div>
        <p class="muted small">"Row" is the row of <span class="mono">docs/crash-analysis.md</span>. A crash makes the whole process exit: the API pill turns red until it is restarted.</p>`)) {
      $$("[data-arm]", el).forEach((bn) => (bn.onclick = async () => {
        const action = bn.dataset.action, body = { action, remaining: 1 }; if (action === "delay") body.params = { delay_ms: 1500 };
        const r = await api("/admin/faults/" + bn.dataset.arm, { method: "PUT", admin: true, body });
        toast(r.status === 204 ? `Armed ${bn.dataset.arm}: ${action} once` : (r.body?.error || "Could not arm"), r.status === 204 ? "ok" : "bad"); views.failures.refresh(el);
      }));
    }
    const clear = $("#fl-clear", el);
    if (clear) clear.onclick = async () => { await api("/admin/faults", { method: "DELETE", admin: true }); toast("Cleared", "ok"); views.failures.refresh(el); };
    $("#fl-d9", el).onclick = () => demo(el, "hold.before_reply");
    $("#fl-d17", el).onclick = () => demo(el, "confirm.before_reply");
  },
};
async function demo(el, point) {
  const log = $("#fl-log", el);
  const say = (kind, t) => { log.insertAdjacentHTML("beforeend", `<div class="note ${kind}">${t}</div>`); };
  log.innerHTML = "";
  try {
    const list = await loadEvents();
    if (!state.eventId) { say("warn", "Create an event first (Book seats page)."); return; }
    const items = (await api(`/events/${state.eventId}/items`)).body.items, free = items.find((i) => i.state === "free");
    if (!free) { say("warn", "No free seat left in this event: create a new one."); return; }
    const buyer = "demo-" + uid(), rid = "demo-req-" + uid();
    let token = "";
    if (state.health?.waiting_room) {
      say("", "The waiting room is on, so the demo buyer joins the queue first…");
      token = await passFor(buyer);
      if (!token) { say("warn", "The demo buyer was not admitted by the waiting room."); return; }
    }
    const hold = () => api("/holds", { method: "POST", headers: token ? { "X-Admission-Token": token } : {}, body: { buyer_id: buyer, request_id: rid, item_ids: [free.id] } });
    let refs = null;
    if (point === "confirm.before_reply") {
      const h = await hold(); if (h.status !== 201) { say("bad", "Could not take a seat to start the demo."); return; }
      refs = h.body.items.map((i) => ({ item_id: i.item_id, hold_version: i.hold_version })); say("", `1. ${esc(buyer)} holds seat ${free.seq} (version ${refs[0].hold_version}).`);
    }
    const arm = await api("/admin/faults/" + point, { method: "PUT", admin: true, body: { action: "crash", remaining: 1 } });
    if (arm.status !== 204) { say("warn", esc(arm.body?.error || "Could not arm the crash: is fault injection enabled and the token entered?")); return; }
    say("", `${refs ? 2 : 1}. Armed a crash at <span class="mono">${point}</span>.`);
    const first = refs ? await api("/holds/confirm", { method: "POST", body: { buyer_id: buyer, items: refs } }) : await hold();
    say(first.status === 0 ? "good" : "warn", `${refs ? 3 : 2}. The request ${first.status === 0 ? "got no answer: the connection broke, because the server process really exited." : "was answered (" + first.status + "): the crash did not fire, so run it again."}`);
    if (first.status !== 0) return;
    say("", "Waiting for the server to come back…");
    let up = false; for (let i = 0; i < 60 && !up; i++) { await sleep(500); up = (await api("/healthz")).status > 0; }
    if (!up) { say("bad", "The server did not come back within 30 s. Start it again by hand (or run it under scripts/dev-api)."); return; }
    say("", "The server is back. The client retries the very same request:");
    if (point === "hold.before_reply") {
      const again = await hold(), v = again.body?.items?.[0]?.hold_version;
      say(again.status === 201 && v === 1 ? "good" : "bad", `The retry returned status ${again.status} with hold <b>version ${v}</b>. The hold had already committed before the crash, so the retry gets the same hold back instead of creating a second one. Seat ${free.seq} now shows as held in the <a href="#/inventory">seat map</a>.`);
    } else {
      const again = await api("/holds/confirm", { method: "POST", body: { buyer_id: buyer, items: refs } });
      say(again.status === 200 && again.body?.replayed ? "good" : "bad", `The retry returned status ${again.status}${again.body?.replayed ? ", marked <b>replayed</b>" : ""}. The booking had already committed before the crash, so the retry is recognised: no second booking, and no second payment. Seat ${free.seq} is booked once.`);
    }
  } finally { /* the log stays: the next refresh no longer rewrites it */ }
}

// ===== Checker =====
views.checker = {
  title: "Checker",
  desc: "An independent tool that reads only the database and checks the rules that must always hold. It does not use the booking code, so a bug there cannot hide from it.",
  render: () => `<div class="card"><p>Run it after a surge, a crash demo, or any time. It never changes your data.</p><div class="btn-row"><button class="btn btn-primary" id="ck-run">Run the checker</button></div></div><div id="ck-out"></div>`,
  refresh(el) {
    $("#ck-run", el).onclick = async () => {
      const r = await api("/admin/check", { admin: true }), out = $("#ck-out", el);
      if (r.status === 401 || r.status === 403) { out.innerHTML = `<div class="note warn">${esc(r.body?.error || "Not allowed")}. Enter the admin token in the top bar.</div>`; return; }
      if (!r.ok) { out.innerHTML = `<div class="note warn">The checker could not run.</div>`; return; }
      const rep = r.body.report, row = (ok, name, why, val) => `<tr><td><span class="badge ${ok ? "ok" : "bad"}">${ok ? "PASS" : "FAIL"}</span></td><td>${name}</td><td class="mono">${val}</td><td class="muted">${why}</td></tr>`;
      out.innerHTML = `<div class="note ${r.body.ok ? "good" : "bad"}"><b>${r.body.ok ? "Every rule holds." : "A rule is broken."}</b> Checked at ${fmtTime(r.body.checked_at)}: ${fmtN(rep.confirmed_bookings)} confirmed bookings against a total capacity of ${fmtN(rep.total_capacity)}.</div>
        <div class="card"><div class="table-wrap"><table><thead><tr><th></th><th>Rule</th><th>Found</th><th>Meaning</th></tr></thead><tbody>
        ${row(rep.double_sold_items === 0, "No seat has two confirmed bookings", "seats sold twice", rep.double_sold_items, "")}
        ${row(rep.extra_bookings === 0, "No booking beyond the first on any seat", "extra bookings", rep.extra_bookings, "")}
        ${row(rep.oversold === 0, "Bookings never exceed capacity", "bookings beyond capacity", rep.oversold, "")}
        ${row(rep.stuck_holds === 0, "Every expired hold can be taken over", "holds that cannot be", rep.stuck_holds, "")}
        ${row(rep.confirmed_items_without_booking === 0, "Every confirmed seat has a booking", "confirmed seats with no booking", rep.confirmed_items_without_booking, "")}
        ${row(rep.bookings_on_unconfirmed_items === 0, "Every booking is on a confirmed seat", "bookings on unconfirmed seats", rep.bookings_on_unconfirmed_items, "")}</tbody></table></div></div>`;
    };
  },
};

// ---------- routing, polling, chrome ----------
const ROUTES = Object.keys(views);
function parseHash() { const r = (location.hash.replace(/^#\//, "") || "start").split("?")[0]; return ROUTES.includes(r) ? r : "start"; }
function show(route) {
  state.route = route; const v = views[route];
  $("#page-title").textContent = v.title; $("#page-desc").textContent = v.desc; document.title = v.title + " · Resource Reservation Engine";
  $$("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.route === route));
  const el = $("#views"); el.innerHTML = v.render(); closeDrawer();
  if (v.refresh) v.refresh(el);
}
async function checkApi() {
  const h = await api("/healthz");
  const was = state.apiUp; state.apiUp = h.status > 0; state.health = h.body || state.health;
  const pill = $("#api-pill"), text = $("#api-pill-text");
  if (!state.apiUp) { pill.className = "pill pill-bad"; text.textContent = "API not answering"; banner("The API is not answering. If you armed a crash, it exited on purpose: restart it (<span class='mono'>scripts/dev-api</span> does that automatically).", true); }
  else {
    const pg = h.body?.postgres === "ok";
    pill.className = "pill " + (pg ? "pill-ok" : "pill-bad"); text.textContent = pg ? "API and Postgres healthy" : "Postgres is not answering";
    if (was === false) { toast("The API is back", "ok"); }
    banner(h.body?.faults_enabled ? "Fault injection is <b>enabled</b> on this server: it can be made to crash on request." : "", false);
    const rp = $("#redis-pill"); rp.hidden = h.body?.redis === undefined;
    if (!rp.hidden) { const ok = h.body.redis === "ok"; rp.className = "pill " + (ok ? "pill-ok" : "pill-warn"); $("#redis-pill-text").textContent = ok ? "Redis healthy" : "Redis down: failing open"; }
  }
}
const THEMES = ["auto", "light", "dark"], ICON = { auto: "◐", light: "☀", dark: "☾" };
function applyTheme() { const t = localStorage.getItem("theme") || "auto"; if (t === "auto") delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t; $("#theme").textContent = ICON[t]; $("#theme-label").textContent = "Theme: " + t; }

window.addEventListener("hashchange", () => show(parseHash()));
$("#drawer-close").onclick = closeDrawer; $("#drawer-backdrop").onclick = closeDrawer;
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeDrawer(); });
$("#live").onchange = (e) => (state.live = e.target.checked);
$("#admin-token").value = state.token;
$("#admin-token").onchange = (e) => { state.token = e.target.value; sessionStorage.setItem("adminToken", state.token); };
$("#theme").onclick = () => { const t = localStorage.getItem("theme") || "auto"; localStorage.setItem("theme", THEMES[(THEMES.indexOf(t) + 1) % 3]); applyTheme(); };
// The refresh loop skips a tick while a modal drawer is open, so a detail view is not redrawn under the reader.
(function loop() {
  const tick = async () => {
    if (state.live && views[state.route].refresh && $("#drawer").hidden) { try { await views[state.route].refresh($("#views")); } catch (e) { /* a page refresh must never break the loop */ } }
    setTimeout(tick, views[state.route].every || 1500);
  };
  setTimeout(tick, 1500);
})();
setInterval(checkApi, 3000);
applyTheme(); checkApi().then(() => show(parseHash()));
