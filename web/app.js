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

// One toast per message: pressing the same thing again restarts the existing one instead of stacking another.
function toast(msg, kind) {
  const box = $("#toasts");
  let t = [...box.children].find((x) => x.textContent === msg);
  if (t) { clearTimeout(t._timer); t.classList.remove("again"); void t.offsetWidth; t.classList.add("again"); }
  else {
    t = document.createElement("div");
    t.className = "toast " + (kind || "");
    t.textContent = msg;
    box.append(t);
    while (box.children.length > 3) box.firstElementChild.remove();
  }
  t._timer = setTimeout(() => t.remove(), 4500);
}

// The admin pages need a token: say so in the side toast and point at the box, instead of a note inside the page.
function needToken(msg) {
  toast(msg || "Enter the admin token in the top bar to use this page", "bad");
  const i = $("#admin-token"); if (!i) return;
  i.focus(); i.classList.add("attn"); setTimeout(() => i.classList.remove("attn"), 1800);
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
  state.eventList = list;
  if (!state.eventId || !list.some((e) => e.id === state.eventId)) state.eventId = list[0]?.id || null;
  return list;
}
function eventSelect(list) {
  if (!list.length) return `<span class="muted">No events yet. <a href="#/events">Create one</a> first.</span>`;
  return `<label class="f">Event<select id="ev-select">${list.map((e) => `<option value="${e.id}" ${e.id === state.eventId ? "selected" : ""}>#${e.id} ${esc(e.name)} (${e.free} free of ${e.capacity})</option>`).join("")}</select></label>`;
}
function bindEventSelect(el, after) {
  const s = $("#ev-select", el);
  if (s) s.onchange = () => { state.eventId = Number(s.value); localStorage.setItem("eventId", state.eventId); state.book.picked.clear(); after(); };
}
function createEventForm() {
  return `<div class="card"><h2>New event</h2><p class="muted small">An event is a set of numbered seats, one row in the database each. The new event becomes the one the other pages show.</p>
    <div class="row"><label class="f">Name<input type="text" id="ce-name" value="Demo show" size="22"></label>
    <label class="f">Seats<input type="number" id="ce-n" value="60" min="1" max="2000" style="width:90px"></label>
    <button class="btn btn-primary" id="ce-go">Create event</button></div></div>`;
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
// Hover (or focus, or tap) a numbered step or a lettered failure marker to read what happens there.
const FLOW_TIPS = {
  s1: ["1 · Join the queue", "When demand is far above what the database can take, a buyer joins the waiting room and gets a ticket. The queue is held in Redis, so it works across several API servers. Joining twice keeps your place."],
  s2: ["2 · Admitted, then hold seats", "At a fixed rate the room admits buyers in the order they joined and gives each a pass. The pass lets them call hold, which asks for one to six specific seats, all or nothing."],
  s3: ["3 · The reserve strategy", "One of several interchangeable strategies decides how contention is handled: row locks, conditional writes, or a Redis gate in front of locking. Every strategy must pass the same behaviour tests."],
  s4: ["4 · Gate claim (fast path only)", "The gate claims the seats in Redis in one atomic step. If someone already holds them it answers 'taken' without touching the database. It is only a hint: when Redis is empty or down, requests simply reach Postgres."],
  s5: ["5 · Postgres decides", "A short transaction checks that every seat is free (or its hold has expired) and writes the hold with a version. A unique index refuses a second confirmed booking for any seat, whatever the application does. This is the only place that decides."],
  s6: ["6 · Pay", "Confirm writes a payment marker, calls the simulated provider (which can be slow, fail or time out), then books the seats. Failure or timeout releases them. A retry during the payment is told it is in progress instead of paying twice."],
  s7: ["7 · Check", "The checker reads only the database and verifies that no seat has two confirmed bookings, bookings never exceed capacity, every expired hold can be taken over, and the ledger agrees with the seats."],
  fA: ["A · Dies after the gate claim", "The Redis claim has a short life and lapses by itself. No hold exists, so the seats may be refused for a few seconds (a lost sale) but never double sold. Tested with a real process kill (row 6 of the crash analysis)."],
  fB: ["B · Dies inside the transaction", "Postgres rolls the transaction back, so a half-held multi-seat request never exists. The retry is a first request. (row 8)"],
  fC: ["C · Dies after the commit, before the reply", "The hold exists but the buyer never heard. The retry carries the same request id and gets the same hold back, not a second one. (row 9)"],
  fD: ["D · Dies during the payment", "A marker stops a second payment for a while. A real provider might have charged the buyer, which the simulated one cannot show: a documented limitation. (rows 12, 13, 15)"],
  fE: ["E · Redis restarts empty", "The gate and the queue are lost. Requests reach Postgres, which stays correct but takes more load; the queue's order is lost and buyers rejoin. (row 20)"],
  fF: ["F · Postgres restarts", "In-flight transactions are lost and requests fail for a moment. Afterwards the checker and the reconciler judge what was booked. (row 21)"],
};

const GLOSSARY = [
  ["Seat (item)", "One reservable unit. It is one row in the database, so it can be held and booked on its own."],
  ["Event", "A set of numbered seats that buyers compete for."],
  ["Hold", "A temporary reservation of one to six seats. Nobody else can take them while it lasts. It is a soft limit: if nobody took the seats, paying a little late still works."],
  ["Expiry", "A hold simply stops counting when its time runs out. Nothing sweeps it, so no crashed process can leave a seat stuck."],
  ["Version", "A counter that goes up each time a seat is held. A late request carrying an old version is recognised and refused."],
  ["Confirm", "Paying for a hold. It writes a marker, takes the (simulated) payment, then books the seats."],
  ["Payment marker", "A mark written before the payment starts, so a retry cannot start a second payment while the first is running."],
  ["Request id", "A unique id the client sends with a hold. Sending the same id again returns the same hold, never a second one."],
  ["Strategy", "How contention is handled: locking (row locks), optimistic (conditional writes) or the fast path (a Redis gate in front of locking)."],
  ["Fast-path gate", "A Redis check in front of Postgres that turns most losing buyers away cheaply. A hint, never the decider."],
  ["Waiting room", "A first-come-first-served queue in Redis that admits buyers at a fixed rate, protecting the database during a surge."],
  ["Pass", "The token the waiting room gives an admitted buyer. Placing a hold needs it."],
  ["System of record", "Postgres. The only place that decides who gets a seat, backed by a unique index that refuses a second booking."],
  ["Checker", "An independent tool that reads only the database and verifies the rules that must always hold."],
  ["Unknown outcome", "A request whose answer never arrived (a crash, a timeout). It may or may not have happened; retrying with the same ids is safe."],
  ["Fail open", "If Redis is down, requests go straight to Postgres instead of being refused: slower, still correct."],
  ["Paid but lost", "A buyer paid, but another buyer had taken the seat after their hold expired. Counted, not refunded (the payment is simulated)."],
  ["Skew", "Demand that is concentrated on a few hot seats instead of spread evenly. It is what creates contention."],
];

views.start = {
  title: "Start",
  desc: "What this project is and how one booking travels through it.",
  render: () => {
    const point = (t, d) => `<div class="point"><h3>${t}</h3><p>${d}</p></div>`;
    const go = (href, t, d) => `<a class="nav-card" href="#/${href}"><b>${t}</b><span>${d}</span></a>`;
    return `
    <div class="card"><div class="card-head"><h2>About this project</h2></div><div class="prose">
      <p class="lead-p">When many buyers go for a few seats at once, the hard part is never selling the same seat twice. This engine <strong>holds a seat for a short time, takes a (simulated) payment, and lets only the database decide who gets it</strong>. It stays correct when a process crashes or Redis loses its data.</p>
      <p class="muted">Built with Go, PostgreSQL, Redis and Docker Compose. The payment and the demand are simulated, so every claim holds "with a simulated payment and simulated demand".</p></div></div>

    <div class="card flow" id="st-flow">
      <div class="card-head head-left"><h2>How a booking flows</h2>
        <label class="switch switch-chip" title="Mark where the system can fail, and how it recovers"><input type="checkbox" id="st-fail"><span class="switch-track"></span><span class="switch-label">Show failure points</span></label></div>
      <p class="muted flow-hint">Hover a number to see what happens at that step. Switch on <strong>Show failure points</strong> to see where it can fail and what is left behind.</p>
      <div class="flow-stage" id="st-stage"><div class="flow-scroll">
        <svg class="flow-svg" viewBox="0 0 1200 500" role="group" aria-label="Diagram: a buyer joins the waiting room, places a hold through the API, a strategy asks Redis and then Postgres, the API takes a simulated payment, and a checker verifies the database.">
          <defs><marker id="flow-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path class="arrowhead" d="M0,0 L10,5 L0,10 z"/></marker></defs>
          <rect class="zone" x="140" y="14" width="865" height="474" rx="14"/><text class="zone-label" x="156" y="34">Your service</text>
          <rect class="zone" x="1020" y="14" width="170" height="250" rx="14"/><text class="zone-label" x="1036" y="34">Outside (simulated)</text>

          <path class="ar" d="M110,230 H168"/><path class="ar" d="M310,230 H368"/><path class="ar" d="M520,230 H588"/><path class="ar" d="M780,230 H848"/>
          <path class="ar" d="M240,292 V410 H358"/><path class="ar" d="M685,320 V410 H562"/>
          <path class="ar" d="M500,150 V45 H1012 V125 H1030"/>
          <path class="ar dashed" d="M430,116 V148"/><path class="ar dashed" d="M910,309 V378"/>

          <g class="fn"><rect x="10" y="200" width="100" height="60" rx="10"/><text x="60" y="224">Buyer</text><text class="t2" x="60" y="242">browser</text></g>

          <g class="fn"><rect class="grp" x="170" y="168" width="140" height="124" rx="12"/><text x="240" y="186">Waiting room</text>
            <rect class="lane" x="182" y="200" width="116" height="36" rx="8"/><text x="240" y="218">queue</text>
            <rect class="lane" x="182" y="244" width="116" height="36" rx="8"/><text x="240" y="262">pass</text></g>

          <g class="fn"><rect class="grp" x="370" y="150" width="150" height="160" rx="12"/><text x="445" y="168">API</text>
            <rect class="lane" x="382" y="182" width="126" height="34" rx="8"/><text x="445" y="199">hold</text>
            <rect class="lane" x="382" y="224" width="126" height="34" rx="8"/><text x="445" y="241">confirm</text>
            <rect class="lane" x="382" y="266" width="126" height="34" rx="8"/><text x="445" y="283">release</text></g>

          <g class="fn"><rect class="grp" x="590" y="140" width="190" height="180" rx="12"/><text x="685" y="158">Strategy</text>
            <rect class="lane" x="602" y="172" width="166" height="40" rx="8"/><text x="685" y="192">Fast-path gate</text>
            <rect class="lane" x="602" y="220" width="166" height="40" rx="8"/><text x="685" y="240">Locking or optimistic</text>
            <rect class="lane dashed" x="602" y="268" width="166" height="40" rx="8"/><text x="685" y="288">Naive (control)</text></g>

          <g class="fn"><path class="cyl" d="M850,182 a60,9 0 0 1 120,0 v118 a60,9 0 0 1 -120,0 z"/><path class="cyl-lip" d="M850,182 a60,9 0 0 0 120,0"/>
            <text x="910" y="222">Postgres</text><text class="t2" x="910" y="242">system of record</text><text class="t2" x="910" y="260">seats and bookings</text></g>

          <g class="fn"><path class="cyl hint" d="M360,378 a100,9 0 0 1 200,0 v62 a100,9 0 0 1 -200,0 z"/><path class="cyl-lip hint" d="M360,378 a100,9 0 0 0 200,0"/>
            <text x="460" y="408">Redis</text><text class="t2" x="460" y="428">queue and gate, a hint</text></g>

          <g class="fn"><rect x="850" y="380" width="120" height="52" rx="10"/><text x="910" y="402">Checker</text><text class="t2" x="910" y="420">reads the database</text></g>
          <g class="fn"><rect class="lane dashed ctl" x="380" y="76" width="100" height="40" rx="8"/><text x="430" y="92">Fault hooks</text><text class="t2" x="430" y="107">off by default</text></g>
          <g class="fn"><rect class="ext" x="1032" y="70" width="146" height="110" rx="12"/><text x="1105" y="96">Payment</text><text class="t2" x="1105" y="118">simulated</text><text class="t2" x="1105" y="146">delay, failure, timeout</text></g>

          <g class="badge" data-tip="s1" tabindex="0" role="button" aria-label="Step 1"><circle cx="139" cy="230" r="11"/><text x="139" y="230">1</text></g>
          <g class="badge" data-tip="s2" tabindex="0" role="button" aria-label="Step 2"><circle cx="339" cy="230" r="11"/><text x="339" y="230">2</text></g>
          <g class="badge" data-tip="s3" tabindex="0" role="button" aria-label="Step 3"><circle cx="554" cy="230" r="11"/><text x="554" y="230">3</text></g>
          <g class="badge" data-tip="s4" tabindex="0" role="button" aria-label="Step 4"><circle cx="685" cy="365" r="11"/><text x="685" y="365">4</text></g>
          <g class="badge" data-tip="s5" tabindex="0" role="button" aria-label="Step 5"><circle cx="814" cy="230" r="11"/><text x="814" y="230">5</text></g>
          <g class="badge" data-tip="s6" tabindex="0" role="button" aria-label="Step 6"><circle cx="756" cy="45" r="11"/><text x="756" y="45">6</text></g>
          <g class="badge" data-tip="s7" tabindex="0" role="button" aria-label="Step 7"><circle cx="910" cy="344" r="11"/><text x="910" y="344">7</text></g>

          <g class="fail-mark" data-tip="fA" tabindex="0" role="button" aria-label="Failure point A"><circle cx="612" cy="410" r="11"/><text x="612" y="410">A</text></g>
          <g class="fail-mark" data-tip="fB" tabindex="0" role="button" aria-label="Failure point B"><circle cx="983" cy="188" r="11"/><text x="983" y="188">B</text></g>
          <g class="fail-mark" data-tip="fC" tabindex="0" role="button" aria-label="Failure point C"><circle cx="532" cy="172" r="11"/><text x="532" y="172">C</text></g>
          <g class="fail-mark" data-tip="fD" tabindex="0" role="button" aria-label="Failure point D"><circle cx="930" cy="45" r="11"/><text x="930" y="45">D</text></g>
          <g class="fail-mark" data-tip="fE" tabindex="0" role="button" aria-label="Failure point E"><circle cx="352" cy="382" r="11"/><text x="352" y="382">E</text></g>
          <g class="fail-mark" data-tip="fF" tabindex="0" role="button" aria-label="Failure point F"><circle cx="838" cy="186" r="11"/><text x="838" y="186">F</text></g>
        </svg></div><div class="flow-tip" id="st-tip" role="tooltip" hidden></div></div>
      <p class="muted small">The failure points map to the rows of <span class="mono">docs/crash-analysis.md</span>; the <a href="#/failures">Failures</a> page can trigger them.</p>
    </div>

    <div class="card"><div class="card-head"><h2>Guarantees</h2></div><div class="point-grid">
      ${point("Never sold twice", "Postgres alone decides, and a unique index refuses a second confirmed booking for any seat, whatever the application, the cache or a crash does.")}
      ${point("Safe retries", "Sending the same request again returns the same hold, or the same booking. A retry during a payment is told it is in progress instead of paying twice.")}
      ${point("Holds that free themselves", "A hold stops counting when its time runs out. There is no cleanup job to die, so a crash cannot leave a seat stuck.")}
      ${point("Survives a lost cache", "Redis holds only hints. If it restarts empty or is down, requests reach Postgres: slower, never wrong.")}
      ${point("A queue that keeps its order", "The waiting room admits buyers first come, first in, at a fixed rate, and it never decides who gets a seat.")}
      ${point("Checked independently", "A separate checker reads only the database and verifies the rules after every experiment, including runs with real crashes.")}
    </div></div>

    <div class="card"><div class="card-head"><h2>Where to look</h2></div><div class="nav-cards">
      ${go("inventory", "Live seat map", "Every seat changing state as traffic arrives, with hold countdowns.")}
      ${go("book", "Book seats", "Hold and pay yourself, and retry a request to see it stay safe.")}
      ${go("surge", "Surge", "Send a crowd at an event and watch the hot seats go first.")}
      ${go("failures", "Failures", "Kill the server at an exact moment and see what is left.")}
      ${go("checker", "Checker", "Prove from the database alone that nothing was sold twice.")}
      ${go("room", "Waiting room", "The queue, its rate, and what it does and does not promise.")}
    </div></div>

    <div class="card" id="st-glossary"><div class="card-head head-left"><h2>Glossary</h2>
      <label class="switch switch-chip" title="Show the words used in this console, with their meanings"><input type="checkbox" id="st-gloss"><span class="switch-track"></span><span class="switch-label">Show glossary</span></label></div>
      <dl class="glossary">${GLOSSARY.map(([t, d]) => `<div><dt>${esc(t)}</dt><dd>${esc(d)}</dd></div>`).join("")}</dl></div>`;
  },
  refresh(el) {
    if (el.dataset.bound) return; // the Start page is static: wire it once
    el.dataset.bound = "1";
    $("#st-fail", el).onchange = (e) => $("#st-flow", el).classList.toggle("show-fail", e.target.checked);
    $("#st-gloss", el).onchange = (e) => $("#st-glossary", el).classList.toggle("show-glossary", e.target.checked);
    const stage = $("#st-stage", el), tip = $("#st-tip", el);
    const showTip = (node) => {
      const info = FLOW_TIPS[node.dataset.tip];
      if (!info) return;
      tip.innerHTML = `<strong>${esc(info[0])}</strong><p>${esc(info[1])}</p>`;
      tip.classList.toggle("fail", node.dataset.tip.startsWith("f"));
      tip.hidden = false;
      const box = stage.getBoundingClientRect(), r = node.getBoundingClientRect(), w = tip.offsetWidth;
      tip.style.left = Math.max(0, Math.min(r.left - box.left + r.width / 2 - w / 2, box.width - w)) + "px";
      tip.style.top = (r.bottom - box.top + 8) + "px";
    };
    const hide = () => { tip.hidden = true; };
    const target = (e) => e.target.closest && e.target.closest("[data-tip]");
    stage.addEventListener("pointerover", (e) => { const n = target(e); if (n) showTip(n); });
    stage.addEventListener("pointerout", (e) => { if (target(e)) hide(); });
    stage.addEventListener("focusin", (e) => { const n = target(e); if (n) showTip(n); });
    stage.addEventListener("focusout", hide);
    stage.addEventListener("click", (e) => { const n = target(e); if (n) showTip(n); });
  },
};

// ===== Overview =====
// Each card answers one plain question with a verdict in words; the counters it came from sit underneath.
views.overview = {
  title: "Overview",
  desc: "Is the engine doing its job right now? Each card asks one question and answers it in words, with the numbers it is based on underneath.",
  render: () => `<div id="ov"></div>`,
  async refresh(el) {
    const [m, ev] = await Promise.all([api("/metrics"), api("/events")]);
    if (!m.ok) { el.innerHTML = `<div class="note warn">The API did not answer.</div>`; return; }
    const c = m.body.api.counters, st = m.body.strategy || {}, out = m.body.outcomes || {};
    const sum = (re) => Object.entries(c).filter(([k]) => re.test(k)).reduce((a, [, v]) => a + v, 0);
    const h = { got: sum(/^requests\{POST \/holds,201\}/), taken: sum(/^requests\{POST \/holds,409\}/), busy: sum(/^requests\{POST \/holds,503\}/), refused: sum(/^requests\{POST \/holds,403\}/) };
    const total = h.got + h.taken + h.busy + h.refused;
    const prev = state.prevMetrics, now = performance.now();
    if (prev) state.rate = { holds: (total - prev.total) / ((now - prev.t) / 1000) };
    state.prevMetrics = { total, t: now };
    const lat = m.body.api.histograms["latency{POST /holds}"];
    const pool = (k) => st["db_pool_" + k] ?? 0;
    const events = ev.body?.events || [];
    const pct = (a, b) => (b ? Math.round((a / b) * 100) : 0);
    const plural = (n, one, many) => (n === 1 ? one : many);

    // --- the checks, each with a verdict
    const overbooked = events.filter((e) => e.confirmed > e.capacity || e.confirmed + e.held + e.free !== e.capacity);
    const waitedPct = pct(pool("acquires_that_waited"), pool("acquires_total"));
    const concerns = [];
    if (overbooked.length) concerns.push(["bad", `Event #${overbooked[0].id} does not add up: more bookings than seats, or seats unaccounted for. Run the Checker.`]);
    if (out.paid_but_lost) concerns.push(["bad", `${fmtN(out.paid_but_lost)} buyer(s) paid but lost the seat. See the Checker and the payments card below.`]);
    if (h.busy) concerns.push(["warn", `${fmtN(h.busy)} hold request(s) were turned away as "busy" because a seat was locked at that moment.`]);
    if (st.gate_errors) concerns.push(["warn", `Redis failed ${fmtN(st.gate_errors)} time(s) and the engine fell back to Postgres alone. Still correct, but slower.`]);
    if (st.room_fail_open) concerns.push(["warn", `The waiting room let ${fmtN(st.room_fail_open)} buyer(s) in without queueing because Redis was unreachable.`]);
    if (pool("acquires_total") >= 200 && waitedPct >= 10) concerns.push(["warn", `${waitedPct}% of database calls had to wait for a free connection: the pool is the limit, not the engine.`]);
    const level = concerns.some((x) => x[0] === "bad") ? "bad" : concerns.length ? "warn" : "ok";
    const headline = { ok: "Everything is working as designed", warn: "Working, with something worth a look", bad: "A correctness check is failing" }[level];
    const idle = total === 0;
    const verdict = `<div class="verdict ${level}"><div class="vd-head"><span class="badge ${level === "ok" ? "ok" : level === "warn" ? "warn" : ""}">${level === "ok" ? "Healthy" : level === "warn" ? "Look closer" : "Problem"}</span><strong>${headline}</strong></div>
      <ul>${concerns.length ? concerns.map((x) => `<li>${esc(x[1])}</li>`).join("") : `<li>No event is over-booked, no request was turned away as busy, and no payment was lost${idle ? ". No traffic has arrived yet: try the Surge page" : ""}.</li>`}</ul></div>`;

    // --- buyers
    const buyers = `<div class="card"><h2>Are buyers getting answers?</h2>
      <p class="answer">${idle ? "No hold requests yet." : `Of ${fmtN(total)} hold ${plural(total, "request", "requests")}, ${fmtN(h.got)} got a seat and ${fmtN(h.taken)} were told the seat was already taken.`}</p>
      <div class="bar tall"><span class="b-conf" style="width:${total ? (h.got / total) * 100 : 0}%" title="got a seat"></span><span class="b-held" style="width:${total ? (h.taken / total) * 100 : 0}%" title="seat taken"></span><span class="b-bad" style="width:${total ? ((h.busy + h.refused) / total) * 100 : 0}%" title="busy or refused"></span></div>
      <div class="chips"><span class="chip"><i class="lg-sold"></i>${fmtN(h.got)} got a seat</span><span class="chip"><i class="lg-held"></i>${fmtN(h.taken)} seat taken</span>${h.busy ? `<span class="chip">${fmtN(h.busy)} busy</span>` : ""}${h.refused ? `<span class="chip">${fmtN(h.refused)} refused (no pass)</span>` : ""}</div>
      <p class="muted small">"Seat taken" is the engine working: it tells the buyer straight away instead of letting two people believe they have the seat.${state.rate && state.rate.holds > 0.05 ? ` Right now about ${state.rate.holds.toFixed(1)} requests per second are arriving.` : ""}</p></div>`;

    // --- correctness
    const booked = events.reduce((a, e) => a + e.confirmed, 0), seats = events.reduce((a, e) => a + e.capacity, 0);
    const correct = `<div class="card"><h2>Is any seat sold twice?</h2>
      <p class="answer ${overbooked.length ? "bad" : events.length ? "good" : ""}">${overbooked.length ? `Something does not add up in event #${overbooked[0].id}.` : events.length ? `No sign of it. Across ${events.length} ${plural(events.length, "event", "events")}, ${fmtN(booked)} of ${fmtN(seats)} seats are booked, no event has more bookings than seats, and every seat is free, held or booked.` : "No events yet."}</p>
      <p class="muted small">This is a quick look at the counts. The Checker page does the full audit straight from the database: one booking per seat, and nothing booked without a payment.</p></div>`;

    // --- speed
    const fast = `<div class="card"><h2>Is it fast?</h2>
      <p class="answer">${lat ? `Half of the hold requests were answered within ${esc(lat.p50_ms_upper_bound)} ms, and 99 in 100 within ${esc(lat.p99_ms_upper_bound)} ms (${fmtN(lat.count)} measured).` : "Nothing measured yet."}</p>
      <p class="muted small">These are upper bounds from fixed buckets inside the server, good for spotting a trend. Benchmark numbers come from the load generator, never from this page.</p></div>`;

    // --- bottleneck
    const limit = `<div class="card"><h2>Is something the bottleneck?</h2>
      <p class="answer">${pool("acquires_total") === 0 ? "No database calls yet." : waitedPct < 10 ? `No. Only ${waitedPct}% of database calls waited for a connection (${fmtN(pool("acquires_that_waited"))} of ${fmtN(pool("acquires_total"))}).` : `Yes, the connection pool: ${waitedPct}% of database calls waited for a free connection.`}</p>
      <div class="kv"><div>Connections in use</div><div>${fmtN(pool("acquired_conns"))} of ${fmtN(pool("max_conns"))}</div><div>Requests in flight</div><div>${fmtN(m.body.inflight)} now, peak ${fmtN(m.body.max_inflight)}</div>${st.inner_lock_busy !== undefined ? `<div>Seats found locked</div><div>${fmtN(st.inner_lock_busy)}</div>` : ""}</div></div>`;

    // --- fast path, in words
    const gate = st.gate_passed !== undefined ? `<div class="card"><h2>What the Redis fast path did</h2>
      <p class="answer">${fmtN(st.gate_rejected)} ${plural(st.gate_rejected, "request was", "requests were")} stopped at Redis without touching Postgres; ${fmtN(st.gate_passed)} went on to Postgres.</p>
      <div class="kv"><div>Stopped at Redis</div><div>${fmtN(st.gate_rejected)}</div><div>Passed to Postgres</div><div>${fmtN(st.gate_passed)}</div><div>Redis learned a seat is gone</div><div>${fmtN(st.gate_learned)}</div><div>Redis failed</div><div>${fmtN(st.gate_errors)}</div></div>
      <p class="muted small">Redis only ever says "taken, don't bother Postgres". It never decides a seat is yours; Postgres does.</p></div>` : "";

    // --- payments
    const confirmed = (out.confirmed_in_time || 0) + (out.confirmed_after_expiry || 0);
    const pay = `<div class="card"><h2>What happened to payments?</h2>
      <p class="answer">${out.payment_attempts ? `${fmtN(confirmed)} ${plural(confirmed, "booking", "bookings")} confirmed from ${fmtN(out.payment_attempts)} payment ${plural(out.payment_attempts, "attempt", "attempts")}.` : "No payments yet."}</p>
      <div class="kv"><div>Confirmed in time</div><div>${fmtN(out.confirmed_in_time)}</div><div>Confirmed after the hold ran out</div><div>${fmtN(out.confirmed_after_expiry)}</div><div>Payment failed or timed out</div><div>${fmtN((out.payment_failed || 0) + (out.payment_timed_out || 0))}</div><div>Paid but lost the seat</div><div>${fmtN(out.paid_but_lost)}</div></div>
      <p class="muted small">"After the hold ran out" means the buyer was slow but nobody else took the seat, so the booking still stands.</p></div>`;

    const cell = (cls, n) => `<td><span class="chip"><i class="${cls}"></i>${fmtN(n)}</span></td>`;
    const rows = events.map((e) => `<tr class="click" data-ev="${e.id}"><td>#${e.id} ${esc(e.name)}</td><td>${e.capacity}</td>${cell("lg-sold", e.confirmed)}${cell("lg-held", e.held)}${cell("lg-free", e.free)}</tr>`).join("");
    el.innerHTML = `${verdict}
      <div class="grid g2">${buyers}${correct}${fast}${limit}${gate}${pay}</div>
      <div class="card" style="margin-top:14px"><h2>Events</h2><p class="muted small" style="margin-top:0">Click an event to open its seat map.</p><div class="table-wrap"><table><thead><tr><th>Event</th><th>Seats</th><th>Booked</th><th>Held</th><th>Free</th></tr></thead><tbody>${rows || `<tr><td class="muted" colspan="5">No events yet</td></tr>`}</tbody></table></div></div>`;
    $$("tr[data-ev]", el).forEach((tr) => (tr.onclick = () => { state.eventId = Number(tr.dataset.ev); localStorage.setItem("eventId", state.eventId); location.hash = "#/inventory"; }));
  },
};

// ===== Live seat map =====
views.inventory = {
  title: "Live seat map",
  desc: "Every seat of an event, updating as traffic arrives. Hold countdowns are computed from the database clock. A hold that has run out shows as free, because that is how the engine treats it.",
  every: 1000,
  render: () => `<div class="note neutral">Read this page alongside the Failures page. It shows each seat's state as the server is crashed and restarted, so expiring holds and landing bookings can be observed while a fault is in progress.</div><div class="card"><div class="row" id="inv-head"></div><div id="inv-sum" class="chips"></div>${legend(false)}<div id="inv-map"></div></div>`,
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
    $("#inv-sum", el).innerHTML = `<span class="chip"><i class="lg-free"></i>${n("free")} free</span><span class="chip"><i class="lg-held"></i>${n("held")} held</span><span class="chip"><i class="lg-sold"></i>${n("confirmed")} booked</span>` + (r.body.truncated ? `<span class="chip">showing the first ${items.length}</span>` : "");
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
  render: () => `<div class="card"><div class="row" id="bk-head"></div><div id="bk-sum" class="chips"></div><div id="bk-body" class="purchase-grid"></div></div>`,
  async refresh(el) {
    const list = await loadEvents();
    if (setHTML($("#bk-head", el), eventSelect(list))) bindEventSelect(el, () => views.bookings.refresh(el));
    if (!state.eventId) return;
    const r = await api(`/events/${state.eventId}/bookings?limit=200`);
    const rows = r.body?.bookings || [];
    // seats bought in one request share the hold request id; rows without one stay on their own
    const groups = [], byReq = new Map();
    for (const b of rows) {
      let g = b.request ? byReq.get(b.request) : null;
      if (!g) { g = { request: b.request, items: [] }; groups.push(g); if (b.request) byReq.set(b.request, g); }
      g.items.push(b);
    }
    const ago = (t) => { const s = Math.max(0, Math.round((Date.now() - new Date(t)) / 1000)); return s < 5 ? "just now" : s < 60 ? s + " s ago" : s < 3600 ? Math.round(s / 60) + " min ago" : fmtTime(t); };
    const idRange = (g) => { const v = g.items.map((x) => x.id).sort((a, b) => a - b); return v.length > 1 && v.every((x, i) => i === 0 || x === v[i - 1] + 1) ? `#${v[0]} to #${v[v.length - 1]}` : v.map((x) => "#" + x).join(", "); };
    const seatsOf = (g) => g.items.map((x) => x.seat).sort((a, b) => a - b);
    $("#bk-sum", el).innerHTML = rows.length ? `<span class="chip">${groups.length} ${groups.length === 1 ? "purchase" : "purchases"}</span><span class="chip">${rows.length} ${rows.length === 1 ? "seat" : "seats"} booked</span>${rows.length >= 200 ? `<span class="chip">showing the latest 200 seats</span>` : ""}` : "";
    $("#bk-body", el).innerHTML = groups.map((g, i) => `<button type="button" class="purchase" data-g="${i}">
        <div class="pc-head"><div class="pc-who"><span class="pc-label">Booked by</span><strong title="${esc(g.items[0].buyer)}">${esc(g.items[0].buyer)}</strong></div><div class="pc-count"><b>${g.items.length}</b><span>${g.items.length === 1 ? "seat" : "seats"}</span></div></div>
        <div class="pc-body"><span class="pc-label">Seats</span><div class="tk-seats">${seatsOf(g).map((n) => `<span class="seat-pill">${n}</span>`).join("")}</div></div>
        <div class="pc-foot"><span>Booking ${idRange(g)}</span><span>${ago(g.items[0].at)}</span></div></button>`).join("") || `<p class="muted">No bookings yet.</p>`;
    $$(".purchase", el).forEach((tk) => (tk.onclick = () => {
      const g = groups[Number(tk.dataset.g)];
      openDrawer("Purchase", `${g.items.length} ${g.items.length === 1 ? "seat" : "seats"} by ${g.items[0].buyer}`, `<div class="kv"><div>Buyer</div><div>${esc(g.items[0].buyer)}</div><div>Booked at</div><div>${new Date(g.items[0].at).toLocaleString()}</div><div>Seats</div><div>${seatsOf(g).join(", ")}</div></div>
        <h3 style="margin:14px 0 6px;font-size:14px">Ledger rows</h3><div class="table-wrap"><table><thead><tr><th>Booking</th><th>Seat</th><th>Hold version</th></tr></thead><tbody>${g.items.slice().sort((a, b) => a.id - b.id).map((x) => `<tr><td class="mono">#${x.id}</td><td>${x.seat}</td><td>${x.hold_version}</td></tr>`).join("")}</tbody></table></div>
        <p class="muted small">The ledger keeps one row per seat, which is what lets the database refuse a second booking of the same seat. The purchase view groups the rows that were bought together.</p>`);
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
    const cfgRows = Object.entries(cfg).map(([k, v]) => `<div class="cfg"><span class="muted small">${esc(k.replace(/_/g, " "))}</span><strong class="mono">${esc(v)}</strong></div>`).join("");
    el.innerHTML = `<div class="split2">
      <div class="fill-col">
        <div class="card"><h2>Stores</h2><div class="kv"><div>Postgres (system of record)</div><div>${pill(b.postgres)}</div><div>Redis (gate and queue, a hint)</div><div>${pill(b.redis)}</div></div>
          <p class="muted small">Redis down is a degraded state, not an outage: requests fail open to Postgres, which stays correct.</p></div>
        <div class="card"><h2>Connection pool</h2><div class="kv"><div>Size</div><div>${fmtN(st.db_pool_max_conns)}</div><div>In use</div><div>${fmtN(st.db_pool_acquired_conns)}</div><div>Waited for a connection</div><div>${fmtN(st.db_pool_acquires_that_waited)} of ${fmtN(st.db_pool_acquires_total)}</div></div></div>
      </div>
      <div class="card fill-card"><h2>Safety switches</h2><div class="switch-list">
        <div class="sw-row"><div class="sw-top"><strong>Fault injection</strong>${b.faults_enabled ? `<span class="badge warn">on</span>` : `<span class="badge ok">off</span>`}</div>
          <div class="muted small">${b.faults_enabled ? "This server can be made to crash on request. Fine for crash tests, never for a real deployment." : "Crash hooks cannot be triggered."} Benchmarks refuse to run while it is on.</div></div>
        <div class="sw-row"><div class="sw-top"><strong>Admin token</strong>${b.admin_token_configured ? `<span class="badge ok">set</span>` : `<span class="badge">not set</span>`}</div>
          <div class="muted small">${b.admin_token_configured ? "The Failures and Checker pages need it." : "Without it the admin actions are refused."}</div></div>
        <div class="sw-row"><div class="sw-top"><strong>Waiting room</strong>${b.waiting_room ? `<span class="badge ok">on</span>` : `<span class="badge">off</span>`}</div>
          <div class="muted small">${b.waiting_room ? "Buyers need a pass before they can place a hold." : "Anyone can place a hold directly."}</div></div></div></div>
    </div>
    <div class="card"><h2>How this server was started</h2><div class="cfg-grid">${cfgRows}</div></div>`;
  },
};

// ===== Create event =====
views.events = {
  title: "Create event",
  desc: "An event is the thing buyers compete for: a numbered set of seats. Create one here, then book it by hand, send a crowd at it, or crash the server while it sells.",
  render: () => `${createEventForm()}<div class="card"><h2>Existing events</h2><div id="ev-list"></div></div>`,
  async refresh(el) {
    const list = await loadEvents();
    bindCreateEvent(el, () => views.events.refresh(el));
    const pick = (e) => `data-ev="${e.id}"`;
    setHTML($("#ev-list", el), list.length ? `<div class="table-wrap"><table><thead><tr><th>Event</th><th>Seats</th><th>Booked</th><th>Held</th><th>Free</th><th></th></tr></thead><tbody>${list.map((e) => `<tr ${pick(e)}><td>#${e.id} ${esc(e.name)}${e.id === state.eventId ? ` <span class="badge ok">selected</span>` : ""}</td><td>${e.capacity}</td><td>${e.confirmed}</td><td>${e.held}</td><td>${e.free}</td><td class="ev-actions"><button class="btn btn-ghost" data-act="book">Book seats</button><button class="btn btn-ghost" data-act="map">Seat map</button></td></tr>`).join("")}</tbody></table></div>` : `<p class="muted">No events yet. Create the first one above.</p>`);
    $$("#ev-list [data-act]", el).forEach((b) => (b.onclick = () => {
      state.eventId = Number(b.closest("tr").dataset.ev); localStorage.setItem("eventId", state.eventId); state.book.picked.clear();
      location.hash = b.dataset.act === "book" ? "#/book" : "#/inventory";
    }));
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
  desc: "Pick up to six seats, proceed to reserve them, then pay. This is the same two-step flow a real buyer goes through: select, then pay. Behind the button, proceeding places a hold on the seats.",
  every: 1000,
  render: () => `<div class="split stretch"><div class="card"><div class="row" id="bk-ev"></div>${legend(true)}<div id="bk-map"></div></div>
    <div><div class="card ticket" id="bk-panel"></div><div id="bk-ticket"></div></div></div>`,
  async refresh(el) {
    if (state.book.busy) return; // do not redraw under an action in progress
    const list = await loadEvents();
    if (setHTML($("#bk-ev", el), eventSelect(list))) bindEventSelect(el, () => views.book.refresh(el));
    if (!state.eventId) { $("#bk-map", el).innerHTML = `<p class="muted"><a href="#/events">Create an event</a> to start.</p>`; renderBookPanel(el, []); return; }
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
// A preview of the ticket this purchase will become: it fills with the seats as they are picked.
function renderTicketPreview(el, items) {
  const b = state.book, ev = (state.eventList || []).find((e) => e.id === state.eventId);
  const seq = (id) => items.find((i) => i.id === id)?.seq ?? id;
  const seats = b.hold ? b.hold.items.map((i) => i.seq) : [...b.picked].map(seq);
  const sample = !seats.length, shown = (sample ? [12, 13] : seats.slice().sort((x, y) => x - y));
  const foot = b.hold ? "Reserved: pay to confirm" : sample ? "Pick seats to fill in your ticket" : "Not reserved yet";
  setHTML($("#bk-ticket", el), `<div class="preview-title muted small">Your ticket${sample ? " (example)" : ""}</div>
    <div class="purchase preview${sample ? " sample" : ""}"><div class="pc-head"><div class="pc-who"><span class="pc-label">Event</span><strong>${esc(ev ? ev.name : "Event")}</strong></div><div class="pc-count"><b>${shown.length}</b><span>${shown.length === 1 ? "seat" : "seats"}</span></div></div>
      <div class="pc-body"><span class="pc-label">Seats</span><div class="tk-seats">${shown.map((n) => `<span class="seat-pill">${n}</span>`).join("")}</div><span class="pc-label" style="margin-top:4px">Booked by</span><strong class="pc-buyer">${esc(state.buyer)}</strong></div>
      <div class="pc-foot"><span>${foot}</span><span>${ev ? "#" + ev.id : ""}</span></div></div>`);
}
function renderBookPanel(el, items) {
  renderTicketPreview(el, items);
  const b = state.book, panel = $("#bk-panel", el), seq = (id) => items.find((i) => i.id === id)?.seq ?? id;
  const log = b.log.slice(-4).map((l) => `<div class="note ${l.kind}">${l.text}</div>`).join("");
  if (b.hold) {
    const dl = b.hold.deadline;
    setHTML(panel, `<h2>Seats reserved for you</h2><div class="chips">${b.hold.items.map((i) => `<span class="chip acc">Seat ${i.seq}</span>`).join("")}</div>
      <div class="countdown" data-dl="${dl}">${secs(dl - performance.now())}s</div><div class="muted small">left to pay (this is your ${term("hold", "hold")}: a soft limit, so if nobody takes your seats, paying late still works)</div>
      <div class="kv" style="margin-top:8px"><div>${term("version", "Version")}</div><div>${b.hold.items.map((i) => i.version).join(", ")}</div><div>Request id</div><div class="mono">${esc(b.hold.requestId)}</div></div>
      <div class="btn-row"><button class="btn btn-primary" id="bk-pay" ${b.busy ? "disabled" : ""}>Pay (simulated)</button><button class="btn" id="bk-rel" ${b.busy ? "disabled" : ""}>Release</button><button class="btn btn-ghost" id="bk-retry" title="Sends exactly the same hold request again, as a client would after a lost reply. You get the same hold back, not a second one.">Retry the hold request</button></div>
      <div class="muted small">${esc(b.status)}</div>${log}`);
    $("#bk-pay", panel).onclick = () => payHold(el); $("#bk-rel", panel).onclick = () => releaseHold(el); $("#bk-retry", panel).onclick = () => retryHold(el);
    return;
  }
  const n = b.picked.size;
  if (!setHTML(panel, `<h2>Your selection</h2>${n ? `<div class="chips">${[...b.picked].map((id) => `<span class="chip acc">Seat ${seq(id)}</span>`).join("")}</div>` : `<p class="muted">Click free seats on the map (up to six).</p>`}
    <label class="f">Buyer id<input type="text" id="bk-buyer" value="${esc(state.buyer)}" size="20" title="Who you are. Two browsers with different ids compete for the same seats."></label>
    <div class="btn-row"><button class="btn btn-primary" id="bk-hold" ${n && !b.busy ? "" : "disabled"}title="Reserves the seats for you for a short time while you pay (the engine places a hold)">Proceed${n ? ` with ${n} seat${n === 1 ? "" : "s"}` : ""}</button><button class="btn btn-ghost" id="bk-clear" ${n ? "" : "disabled"}>Clear</button></div>
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
    b.picked.clear(); note("good", `${ids.length} seat${ids.length === 1 ? "" : "s"} reserved for you. Pay before the countdown ends, or someone else may take them.`);
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
    <div class="card"><h2>Outcome</h2><div id="sg-out"></div><p class="muted small">When it ends, open the <a href="#/inventory">seat map</a>, then run the <a href="#/checker">checker</a> to confirm nothing was sold twice.</p></div></div>`,
  async refresh(el) {
    if (el.dataset.running) return;
    paintSurge(el);
    const list = await loadEvents();
    if (setHTML($("#sg-ev", el), eventSelect(list))) bindEventSelect(el, () => views.surge.refresh(el));
    $("#sg-go", el).onclick = () => runSurge(el);
  },
};
// The outcome card is always filled in: zeros before a run, live numbers during one, the last result after it.
function paintSurge(el) {
  const r = state.surgeRun, t = r?.tally || { held: 0, unavailable: 0, busy: 0, confirmed: 0, lost: 0, failed: 0, errors: 0, turnedAway: 0 };
  const tile = (cls, label, v, hint) => `<div class="o-tile ${cls}" title="${hint}"><span class="o-n">${v}</span><span class="o-l">${label}</span></div>`;
  const line = !r ? "No crowd sent yet. Choose how many buyers and press Start: the numbers below fill in as they arrive."
    : r.running ? `${r.done} of ${r.n} buyers finished so far.`
    : `${t.confirmed} of ${r.n} buyers booked their seats and ${t.unavailable} were told the seats were already taken.`;
  setHTML($("#sg-out", el), `<p class="answer">${line}</p><div class="o-grid">
    ${tile("good", "Bookings confirmed", t.confirmed, "Held the seats, paid, and the booking was written")}
    ${tile("", "Holds granted", t.held, "Got the seats for the hold time (includes the bookings above)")}
    ${tile("", "Seat taken by others", t.unavailable, "The engine answered straight away that a seat was gone")}
    ${tile("warn", "Lock busy", t.busy, "A seat was locked at that instant; the buyer retried and gave up")}
    ${tile("warn", "Turned away by the room", t.turnedAway, "The waiting room was full")}
    ${tile("bad", "Paid but lost", t.lost, "Paid after the hold ran out and another buyer had the seat")}
    ${tile("warn", "Payment failed or timed out", t.failed, "The simulated payment failed, so the seats were released")}
    ${tile("bad", "Errors", t.errors, "Anything unexpected")}</div>`);
}
async function runSurge(el) {
  if (!state.eventId) { location.hash = "#/events"; return toast("Create an event first", "bad"); }
  const n = Number($("#sg-n", el).value), over = Number($("#sg-t", el).value) * 1000, skew = Number($("#sg-s", el).value);
  const items = (await api(`/events/${state.eventId}/items`)).body.items.map((i) => i.id);
  const pick = zipfPicker(items.length, skew), sizes = [1, 2, 2, 3, 4, 6, 2, 2, 1, 3];
  const tally = { held: 0, unavailable: 0, busy: 0, confirmed: 0, lost: 0, failed: 0, errors: 0, turnedAway: 0 };
  let done = 0; el.dataset.running = "1";
  state.surgeRun = { tally, n, done: 0, running: true };
  const show = () => {
    state.surgeRun.done = done;
    $("#sg-bar", el).style.width = (done / n * 100) + "%"; $("#sg-sum", el).textContent = `${done} of ${n} buyers finished`;
    paintSurge(el);
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
  await Promise.all(jobs); delete el.dataset.running; state.surgeRun.running = false; paintSurge(el); toast("The crowd has finished", "ok");
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
      <p class="muted small">They need the server to restart by itself (<span class="mono">scripts/up</span> starts it that way).</p><div id="fl-log"></div></div>
    <div class="card" id="fl-armed"></div></div><div class="card" id="fl-table"></div>`,
  async refresh(el) {
    const h = await api("/healthz");
    const f = state.token ? await api("/admin/faults", { admin: true }) : { status: 0, body: null };
    const b = h.body || {};
    let gate = "";
    if (!b.faults_enabled) gate = `<div class="note warn">Fault injection is off on this server. Start it with <span class="mono">FAULTS_ENABLED=true ADMIN_TOKEN=&lt;token&gt;</span>, or simply use <span class="mono">scripts/up</span>, which turns it on and restarts the server after a crash.</div>`;
    else if (!state.token) { if (!state.tokenAsked) { state.tokenAsked = true; needToken(); } }
    else if (f.status === 401 && state.tokenRefusedFor !== state.token) { state.tokenRefusedFor = state.token; needToken("The admin token was refused: check it in the top bar"); }
    setHTML($("#fl-gate", el), gate);
    const armed = f.body?.faults || [], known = f.body?.known || {};
    setHTML($("#fl-armed", el), `<h2>Armed right now</h2>${armed.length ? `<table><thead><tr><th>Point</th><th>Action</th><th>Left</th></tr></thead><tbody>${armed.map((a) => `<tr><td class="mono">${esc(a.point)}</td><td>${esc(a.action)}</td><td>${a.remaining ?? "∞"}</td></tr>`).join("")}</tbody></table>` : `<p class="muted">Nothing armed.</p>`}
      <div class="btn-row"><button class="btn btn-danger" id="fl-clear">Clear all</button></div>`);
    if (setHTML($("#fl-table", el), `<h2>Every failure point</h2><div class="table-wrap"><table><thead><tr><th>Point</th><th>Row</th><th>Where</th><th>Arm it</th></tr></thead><tbody>${Object.keys(known).sort().map((p) => `<tr><td class="mono">${esc(p)}</td><td>${esc(FAULT_INFO[p]?.[0] || "")}</td><td>${esc(FAULT_INFO[p]?.[1] || "")}</td><td>${(known[p] || []).map((a) => `<button class="btn" data-arm="${esc(p)}" data-action="${esc(a)}">${esc(a)} once</button>`).join(" ")}</td></tr>`).join("") || `<tr><td class="muted" colspan="4">Unlocks once faults are enabled and the token is entered.</td></tr>`}</tbody></table></div>
        <p class="muted small">"Row" is the row of <span class="mono">docs/crash-analysis.md</span>. A crash makes the whole process exit: the API pill turns red until it is restarted.</p>`)) {
      $$("[data-arm]", el).forEach((bn) => (bn.onclick = async () => {
        if (!state.token) return needToken();
        const action = bn.dataset.action, body = { action, remaining: 1 }; if (action === "delay") body.params = { delay_ms: 1500 };
        const r = await api("/admin/faults/" + bn.dataset.arm, { method: "PUT", admin: true, body });
        toast(r.status === 204 ? `Armed ${bn.dataset.arm}: ${action} once` : (r.body?.error || "Could not arm"), r.status === 204 ? "ok" : "bad"); views.failures.refresh(el);
      }));
    }
    const clear = $("#fl-clear", el);
    if (clear) clear.onclick = async () => { if (!state.token) return needToken(); await api("/admin/faults", { method: "DELETE", admin: true }); toast("Cleared", "ok"); views.failures.refresh(el); };
    $("#fl-d9", el).onclick = () => demo(el, "hold.before_reply");
    $("#fl-d17", el).onclick = () => demo(el, "confirm.before_reply");
  },
};
async function demo(el, point) {
  if (!state.token) return needToken();
  const log = $("#fl-log", el);
  const say = (kind, t) => { log.insertAdjacentHTML("beforeend", `<div class="note ${kind || "neutral"}">${t}</div>`); };
  log.innerHTML = "";
  try {
    const list = await loadEvents();
    if (!state.eventId) { say("warn", "Create an event first (Create event page)."); return; }
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
    if (arm.status === 401 || arm.status === 403) { needToken(arm.status === 401 ? "The admin token was refused: check it in the top bar" : undefined); return; }
    if (arm.status !== 204) { say("warn", esc(arm.body?.error || "Could not arm the crash: is fault injection enabled and the token entered?")); return; }
    say("", `${refs ? 2 : 1}. Armed a crash at <span class="mono">${point}</span>.`);
    const first = refs ? await api("/holds/confirm", { method: "POST", body: { buyer_id: buyer, items: refs } }) : await hold();
    say(first.status === 0 ? "good" : "warn", `${refs ? 3 : 2}. The request ${first.status === 0 ? "got no answer: the connection broke, because the server process really exited." : "was answered (" + first.status + "): the crash did not fire, so run it again."}`);
    if (first.status !== 0) return;
    say("", "Waiting for the server to come back…");
    let up = false; for (let i = 0; i < 60 && !up; i++) { await sleep(500); up = (await api("/healthz")).status > 0; }
    if (!up) { say("bad", "The server did not come back within 30 s. Run scripts/up again: it starts the server, and restarts it after a crash by itself."); return; }
    say("", "The server is back. The client retries the very same request:");
    if (point === "hold.before_reply") {
      const again = await hold(), v = again.body?.items?.[0]?.hold_version;
      say(again.status === 201 && v === (free.version ?? 0) + 1 ? "good" : "bad", `The retry returned status ${again.status} with hold <b>version ${v}</b>. The hold had already committed before the crash, so the retry gets the same hold back instead of creating a second one. Seat ${free.seq} now shows as held in the <a href="#/inventory">seat map</a>.`);
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
      if (r.status === 401 || r.status === 403) { out.innerHTML = ""; needToken(r.status === 401 && state.token ? "The admin token was refused: check it in the top bar" : undefined); return; }
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
  state.route = route; state.tokenAsked = false; state.tokenRefusedFor = null; const v = views[route];
  $("#page-title").textContent = v.title; $("#page-desc").textContent = v.desc; document.title = v.title + " · Resource Reservation Engine";
  $$("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.route === route));
  const el = $("#views"); el.innerHTML = v.render(); closeDrawer();
  if (v.refresh) v.refresh(el);
}
async function checkApi() {
  const h = await api("/healthz");
  const was = state.apiUp; state.apiUp = h.status > 0; state.health = h.body || state.health;
  const pill = $("#api-pill"), text = $("#api-pill-text");
  if (!state.apiUp) { pill.className = "pill pill-bad"; text.textContent = "API not answering"; banner("The API is not answering. If you armed a crash, it exited on purpose: run <span class='mono'>scripts/up</span> again (it normally restarts the server by itself).", true); }
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
function applyTheme() { const t = localStorage.getItem("theme") || "auto"; if (t === "auto") delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = t; $("#theme").textContent = ICON[t]; $("#theme-label").textContent = "Theme: " + t; $("#theme").title = "Theme: " + t + " (click to change)"; }

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
