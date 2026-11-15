#!/usr/bin/env python3
"""Build the result tables of docs/benchmarks.md from the raw records in docs/data/.

Every number in those tables comes from this script reading the JSON records, never from memory:

    python scripts/results-tables.py            # all sections, as Markdown on stdout
    python scripts/results-tables.py e2 e3      # only some sections

Sections: env, totals, e1, e1b, e2, e2attr, e3, e4, crash, drift.
The runs of 2026-11-14 live in three folders (see docs/benchmarks.md, "Where the data is").
"""
import collections
import glob
import json
import os
import sys

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs", "data")
MAIN = os.path.join(ROOT, "2026-11-14-linux-vm")
CONT = os.path.join(ROOT, "2026-11-14-linux-vm-contention")
SWEEP = os.path.join(ROOT, "2026-11-14-linux-vm-skew-sweep")
E2_DIRS = [os.path.join(MAIN, "e2-strategies"), os.path.join(MAIN, "e2-skew"), os.path.join(MAIN, "e2-skew14"), os.path.join(SWEEP, "e2-sweep")]


def load(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def g(x):
    return "%g" % x


def n(x):
    return "{:,.0f}".format(x)


def rng(values, fmt="%d"):
    lo, hi = fmt % min(values), fmt % max(values)
    return lo if lo == hi else lo + " to " + hi


def table(header, rows):
    out = ["| " + " | ".join(header) + " |", "|" + "|".join("---" for _ in header) + "|"]
    out += ["| " + " | ".join(str(c) for c in r) + " |" for r in rows]
    return "\n".join(out)


def env():
    d = load(os.path.join(MAIN, "e1", "e1-locking-run1.json"))
    e, s, p = d["environment"], d["store_settings"], d["params"]
    rows = [
        ("Machine", "%s (standard provisioning), %d vCPUs, %s MB memory" % (e["machine_type"], e["logical_cpus"], n(e["mem_total_mb"]))),
        ("CPU", e["cpu_model"] + ", CPU steal %g%% when measured" % e["cpu_steal_pct"]),
        ("Kernel / Go", "%s / %s" % (e["kernel"], e["go_version"])),
        ("Core plan", e["pinning"]),
        ("PostgreSQL", "%s; max_connections %s, synchronous_commit %s, fsync %s" % (s["postgres_version"].split(" on ")[0], s["postgres_settings"]["max_connections"], s["postgres_settings"]["synchronous_commit"], s["postgres_settings"]["fsync"])),
        ("Redis", "%s, persistence %s" % (s["redis_version"], s["redis_persistence"])),
        ("API pool / hold time", "%d connections / %s" % (p["db_pool_size"], p["hold_ttl"])),
        ("Payment (simulated)", "%g to %g s, %g%% fail, %g%% exceed a %g s timeout" % (p["payment"]["delay_min_ms"] / 1000, p["payment"]["delay_max_ms"] / 1000, p["payment"]["fail_pct"], p["payment"]["timeout_pct"], p["payment"]["timeout_ms"] / 1000)),
    ]
    return table(["", ""], rows)


def all_records():
    for base in (MAIN, CONT, SWEEP):
        for f in glob.glob(os.path.join(base, "**", "*.json"), recursive=True):
            try:
                d = load(f)
            except Exception:
                continue
            if isinstance(d, dict) and "checker" in d:
                yield f, d


def totals():
    tot = collections.Counter()
    for f, d in all_records():
        strat = d.get("strategy") or d.get("params", {}).get("strategy")
        if not strat or "aborted" in f:
            continue
        key = "control" if strat in ("naive-unprotected", "naive") else ("backstop" if strat == "naive-protected" else "protected")
        ck = d["checker"]
        tot[key + " runs"] += 1
        tot[key + " confirmed"] += ck["confirmed_bookings"]
        tot[key + " double-sold runs"] += ck["double_sold_items"] > 0
        tot[key + " oversold runs"] += ck["oversold"] > 0
        tot[key + " stuck-hold runs"] += ck["stuck_holds"] > 0
    rows = [
        ("locking, optimistic, fast path", n(tot["protected runs"]), n(tot["protected confirmed"]), tot["protected double-sold runs"], tot["protected oversold runs"], tot["protected stuck-hold runs"]),
        ("naive with the database backstop", n(tot["backstop runs"]), n(tot["backstop confirmed"]), tot["backstop double-sold runs"], tot["backstop oversold runs"], tot["backstop stuck-hold runs"]),
        ("naive, no protection (the control)", n(tot["control runs"]), n(tot["control confirmed"]), "**%d**" % tot["control double-sold runs"], "**%d**" % tot["control oversold runs"], tot["control stuck-hold runs"]),
    ]
    return table(["Strategy", "Run records", "Bookings confirmed", "Runs with a double-sold item", "Runs oversold", "Runs with a stuck hold"], rows)


def e1():
    by = collections.defaultdict(list)
    for f in sorted(glob.glob(os.path.join(MAIN, "e1", "e1-*.json"))):
        d = load(f)
        by[d["params"]["strategy"]].append(d)
    rows = []
    for k in ("naive-unprotected", "naive-protected", "locking", "optimistic", "fastpath"):
        rs = by[k]
        cl = [r["client_summary"] for r in rs]
        ck = [r["checker"] for r in rs]
        rows.append((k, len(rs), rng([c["confirmed_bookings"] for c in ck]), sum(c["oversold"] for c in ck), sum(c["double_sold_items"] for c in ck), sum(c["stuck_holds"] for c in ck),
                     rng([x["hold_p50_ms"] for x in cl], "%.1f"), rng([x["hold_p99_ms"] for x in cl], "%.0f")))
    return table(["Strategy", "Runs", "Bookings per run", "Oversold", "Double-sold items", "Stuck holds", "Hold p50 (ms)", "Hold p99 (ms)"], rows)


def e1b():
    by = collections.defaultdict(list)
    for f in sorted(glob.glob(os.path.join(CONT, "final-10items-10000b-1s", "e1-*.json"))):
        d = load(f)
        by[d["params"]["strategy"]].append(d)
    rows = []
    for k in ("naive-unprotected", "naive-protected", "locking", "optimistic", "fastpath"):
        rs = by[k]
        ck = [r["checker"] for r in rs]
        flagged = sum("violated" in r["verdict"] for r in rs)
        rows.append((k, len(rs), ", ".join(str(c["confirmed_bookings"]) for c in ck), ", ".join(str(c["oversold"]) for c in ck), "%d of %d" % (flagged, len(rs))))
    probes = []
    for pr in sorted(glob.glob(os.path.join(CONT, "probe-*"))):
        rs = [load(f) for f in sorted(glob.glob(os.path.join(pr, "e1-*.json")))]
        probes.append((os.path.basename(pr).split("-", 2)[0] + " " + os.path.basename(pr).split("-", 1)[1], "%d of %d" % (sum(r["checker"]["double_sold_items"] > 0 for r in rs), len(rs)), ", ".join(str(r["checker"]["double_sold_items"]) for r in rs)))
    return table(["Strategy", "Runs", "Bookings confirmed per run (capacity 10)", "Oversold per run", "Runs flagged"], rows) + "\n\nProbe (naive, no protection only):\n\n" + table(["Setting", "Runs that oversold", "Double-sold items per run"], probes)


def e2_rows():
    rows = []
    for dd in E2_DIRS:
        for f in sorted(glob.glob(os.path.join(dd, "e2-*-capacity.json"))):
            c = load(f)
            st, sk = c["strategy"], g(c["skew_zipf_exponent"])
            lat = c["latency_runs"]
            p50 = sorted(r["hold_p50_ms"] for r in lat)[len(lat) // 2]
            p99 = sorted(r["hold_p99_ms_judged"] for r in lat)[len(lat) // 2]
            bps = sorted(r["bookings_per_s_arrival_window"] for r in lat)[len(lat) // 2]
            rows.append(dict(strategy=st, skew=float(sk), skew_s=sk, cap=c["confirmed_capacity_buyers_per_s"], lb=c["ladder_exhausted_capacity_is_a_lower_bound"],
                             p50=p50, p99=p99, bps=bps, rate=c["latency_rate_buyers_per_s"], ramp=c["ramp"], dir=dd))
    return sorted(rows, key=lambda r: (r["strategy"], r["skew"]))


def e2():
    rows = e2_rows()
    order = ["locking", "optimistic", "fastpath"]
    skews = sorted({r["skew"] for r in rows})
    cells = {(r["strategy"], r["skew"]): r for r in rows}
    out = []
    for st in order:
        line = [st]
        for sk in skews:
            r = cells[(st, sk)]
            line.append("%s%s<br>p50 %.1f / p99 %.1f ms at %s/s<br>%s bookings/s" % (n(r["cap"]), " (lower bound)" if r["lb"] else "", r["p50"], r["p99"], n(r["rate"]), n(r["bps"])))
        out.append(line)
    return table(["Confirmed capacity (buyers/s), then latency at 70% of it"] + ["skew %s" % g(s) for s in skews], out)


def e2attr():
    out = []
    for r in e2_rows():
        st, sk, cap = r["strategy"], r["skew_s"], r["cap"]

        def rec(rate):
            p = os.path.join(r["dir"], "e2-%s-skew%s-ramp-%d-rep0.json" % (st, sk, round(rate)))
            return load(p) if os.path.exists(p) else None

        def att(d):
            if not d:
                return None
            sm = d["server_metrics"]["strategy"]
            tot = sm.get("db_pool_acquires_total", 0) or 1
            pc = d["part_cpu"]
            return (100 * sm.get("db_pool_acquires_that_waited", 0) / tot, pc["postgres"]["mean_pct_of_pinned"], pc["api"]["mean_pct_of_pinned"], pc["bench"]["mean_pct_of_pinned"])

        ok = [x for x in r["ramp"] if x["sustained"] and abs(x["rate_buyers_per_s"] - cap) < 1]
        bad = [x for x in r["ramp"] if not x["sustained"]]
        a_ok = att(rec(ok[0]["rate_buyers_per_s"])) if ok else None
        a_bad = att(rec(bad[0]["rate_buyers_per_s"])) if bad else None
        f = lambda a: "n/a" if not a else "%d%% / %d%% / %d%% / %d%%" % tuple(round(v) for v in a)
        out.append((st, g(r["skew"]), n(cap), f(a_ok), n(bad[0]["rate_buyers_per_s"]) if bad else "-", f(a_bad)))
    return table(["Strategy", "Skew", "Capacity", "Last sustained step: pool waits / Postgres / API / generator", "First failing step", "First failing step: pool waits / Postgres / API / generator"], out)


def e3():
    cap = load(os.path.join(MAIN, "e3", "e3-locking-skew1.1-capacity.json"))
    head = "Capacity of the small backend, measured in the same session: **%s buyers/s** (latency run at %s/s). The waiting room admitted R = 0.8 x C = **%s buyers/s**.\n\n" % (n(cap["confirmed_capacity_buyers_per_s"]), n(cap["latency_rate_buyers_per_s"]), n(0.8 * cap["confirmed_capacity_buyers_per_s"]))
    rows = []
    for fct in ("1.5", "2", "2.5"):
        for mode in ("no-room", "room"):
            f = os.path.join(MAIN, "e3", "e3-locking-x%s-%s.json" % (fct, mode))
            d = load(f)
            c, sl = d["client_summary"], d["store_load"]
            flag = "" if c["generator_kept_up"] else "generator behind: not used for latency"
            extra = []
            if mode == "room":
                extra.append("%s admitted, %s turned away, %s gave up, %s FIFO inversions" % (n(c["room_admitted"]), c["room_turned_away"], c["room_gave_up"], c["fifo_inversions"]))
            unk = d["reconcile"]["unknown_outcome_booked"] + d["reconcile"]["unknown_outcome_not_booked"]
            if unk:
                extra.append("%d unknown outcomes" % unk)
            rows.append(("%sx (%s/s)" % (fct, n(c["target_rate"])), "room" if mode == "room" else "no room", "%.0f" % c["store_hold_p99_ms"], "%.0f" % c["hold_p99_ms"], "%.0f%% / %.0f%%" % (sl["host_cpu_mean_percent"], sl["host_cpu_max_percent"]),
                         n(c["hold_held"]), n(c["hold_unavailable"]), "; ".join(([flag] if flag else []) + extra) or "none"))
    return head + table(["Offered", "Arm", "Store p99 (ms)", "Buyer p99 (ms)", "Host CPU mean / max", "Holds granted", "Seat taken", "Notes"], rows)


def e4():
    agg = collections.defaultdict(collections.Counter)
    for f in sorted(glob.glob(os.path.join(MAIN, "e4", "e4-*.json"))):
        d = load(f)
        a = agg[(d["plan"]["name"], d["strategy"])]
        a["runs"] += 1
        a["clean"] += d["verdict"] == "all invariants held"
        a["sig"] += d["api_sigkills_observed"]
        a["hook"] += d["api_hook_crash_exits_observed"]
        a["redis"] += d["redis_restarts_done"]
        a["pg"] += d["postgres_restarts_done"]
        a["unk"] += d["reconcile"]["unknown_outcome_booked"] + d["reconcile"]["unknown_outcome_not_booked"]
        a["stuck"] += d["checker"]["stuck_holds"]
        a["dbl"] += d["checker"]["double_sold_items"]
        a["over"] += d["checker"]["oversold"]
    rows = []
    tot = collections.Counter()
    for (plan, strat), a in sorted(agg.items()):
        tot.update(a)
        rows.append((plan, strat, a["runs"], a["clean"], a["sig"], a["hook"], a["redis"], a["pg"], a["unk"], a["dbl"], a["over"], a["stuck"]))
    rows.append(("**total**", "", tot["runs"], tot["clean"], tot["sig"], tot["hook"], tot["redis"], tot["pg"], tot["unk"], tot["dbl"], tot["over"], tot["stuck"]))
    return table(["Plan", "Strategy", "Runs", "Clean", "SIGKILLs", "Crash-hook exits", "Redis restarts", "Postgres restarts", "Unknown outcomes", "Double-sold", "Oversold", "Stuck holds"], rows)


def crash():
    rows = [json.loads(line) for line in open(os.path.join(MAIN, "crash-tests", "crash-results.jsonl"), encoding="utf-8")]
    by = collections.defaultdict(list)
    for r in rows:
        by[(r["row"], r["point"], r["strategy"])].append(r)

    def key(k):
        digits = "".join(ch for ch in k[0] if ch.isdigit())
        return (int(digits) if digits else 0, k[0])

    out = [(k[0], "`%s`" % k[1], k[2], len(v), sum(x["killed"] for x in v), sum(x["passed"] for x in v)) for k, v in sorted(by.items(), key=lambda kv: key(kv[0]))]
    head = "**%d runs, %d real kills (exit status %s), %d passed.**\n\n" % (len(rows), sum(r["killed"] for r in rows), ", ".join(sorted({str(r["exit_code"]) for r in rows})), sum(r["passed"] for r in rows))
    return head + table(["Crash-analysis row", "Hook", "Strategy", "Runs", "Real kills", "Passed"], out)


def drift():
    out = []
    for label, dd in (("E2a (three strategies, skew 1.1)", E2_DIRS[0]), ("fast path skew 1.4 rerun", E2_DIRS[2]), ("locking and optimistic sweep", E2_DIRS[3])):
        f = os.path.join(dd, "e2-baseline-drift.json")
        if os.path.exists(f):
            b = load(f)
            out.append((label, "%+.1f%%" % b["hold_p50_drift_percent"], "%+.1f%%" % b["hold_p99_drift_percent"], "%+.1f%%" % b["bookings_per_s_drift_percent"]))
    out.append(("fast path skews 0 and 0.8", "not measured", "not measured", "that invocation stopped before its closing baseline"))
    return table(["Invocation", "Hold p50", "Hold p99", "Bookings/s"], out)


SECTIONS = dict(env=env, totals=totals, e1=e1, e1b=e1b, e2=e2, e2attr=e2attr, e3=e3, e4=e4, crash=crash, drift=drift)

if __name__ == "__main__":
    want = sys.argv[1:] or list(SECTIONS)
    for name in want:
        if name not in SECTIONS:
            sys.exit("unknown section %r; choose from %s" % (name, ", ".join(SECTIONS)))
        print("<!-- %s -->" % name)
        print(SECTIONS[name]())
        print()
