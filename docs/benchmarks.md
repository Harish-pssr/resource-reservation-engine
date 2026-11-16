# Benchmarks on a Linux VM

This document reports every measurement taken for this project: what was run, how, what came out, and what the numbers do **not** show. All runs took place on one cloud Linux virtual machine on 2026-11-14. Every figure below is generated from the raw records in [`docs/data/`](data) by [`scripts/results-tables.py`](../scripts/results-tables.py); none was typed by hand.

> **Scope: what is real and what is simulated.** The engine, the database, the cache, the load, the process kills (real `SIGKILL`s and real exits at named points), the store restarts and the counting are real. **The payment is simulated** (a delay, a failure rate and a timeout) and **the demand is simulated** (an open-loop generator with skewed item popularity). Everything ran on **one machine**, so the numbers describe this machine, this core split and this configuration, not a production deployment. Capacities are in buyers per second (attempts to book), which is not the same as bookings per second: at high skew most buyers lose.

## At a glance

| Question | Result |
|---|---|
| Can a seat be sold twice? | Across 347 run records of the three protected strategies (locking, optimistic, fast path), including real process kills and store restarts: **0 double-sold items, 0 oversold, 0 stuck holds**. |
| Is the checker able to see a double sale? | Yes. The naive strategy with no database protection **sold up to 86 seats of 10** in a contention test (4 of 10 runs), and once in 5 in the failure experiment. At 1,000 items it did not oversell in 5 runs, so the control needs contention to fire. |
| Does the database alone prevent a double sale? | Yes. The naive code with only the unique index sold **0 seats twice in 15 runs**, though in 4 of 10 contention runs it left other inconsistencies behind (see E1b). |
| How fast, and what limits it? | Confirmed capacity of **3,054 to 18,210 buyers/s** depending on strategy and demand skew, p99 hold latency **6 to 88 ms** at 70% of capacity. In 11 of 12 cases the first thing to run out is the API's connection pool (32), not CPU. |
| Does the strategy matter? | Only under contention. With spread demand all three are identical. At skew 1.4 **locking falls to 4,773 buyers/s while optimistic and fast path reach 18,210.** |
| Does the waiting room help? | At 2x the capacity of a small database it cut the store's p99 from **9.8 s to 0.76 s** (3.7 s to 0.22 s at 1.5x). The wait moves into the queue, in order, with nobody turned away. |
| Survives kills and restarts? | 75 API kills, 15 Postgres restarts and 10 Redis restarts under load, plus 55 real-kill crash tests: all clean. |

## Environment

|  |  |
|---|---|
| Machine | n2-standard-16 (standard provisioning), 16 vCPUs, 64,293 MB memory |
| CPU | Intel(R) Xeon(R) CPU @ 2.80GHz, CPU steal 0% when measured |
| Kernel / Go | 7.0.0-1017-gcp / go1.27.0 |
| Core plan | postgres=0-2,8-10 api=3-4,11-12 bench=5-6,13-14 redis=7 spare=15 |
| PostgreSQL | PostgreSQL 16.16 (Debian 16.16-1.pgdg13+1); max_connections 300, synchronous_commit on, fsync on |
| Redis | 7.4.12, persistence none |
| API pool / hold time | 32 connections / 300s |
| Payment (simulated) | 2 to 10 s, 10% fail, 2% exceed a 15 s timeout |

Each part had its own physical cores, with no two parts sharing a core, which was proven from the running processes (the two threads of one core are CPU *n* and CPU *n*+8 on this machine, so a contiguous split would have shared cores). Background services that would take CPU (automatic updates, monitoring agents) were stopped; the CPU steal reading was 0% on an idle machine. Disk check on the database's disk: `fdatasync` about 390 microseconds per operation. Two profiles were used: the **full** machine above (E1, E2, E4) and a deliberately **small backend** for the surge experiment (E3): PostgreSQL on one core and an API pool of 8. Every result record embeds its machine type, core plan, kernel, CPU steal, profile, pool size, generator cap and commit.

## Method

- **Open-loop load.** The generator sends buyers at a fixed rate, regardless of how fast the system answers, so queueing shows. Latency is counted from each buyer's *intended* send time. The generator reports its own schedule lag, and a run where it fell behind is flagged and **not used for latency**.
- **Workload** (fixed before any run; design D10): one event of K items, N virtual buyers, each asking for 1 to 6 items (mean 2.6), demand skewed by a Zipf exponent (1.1 by default; 0, 0.8, 1.1 and 1.4 swept). A buyer holds, then pays (simulated: 2 to 10 s, 10% fail, 2% exceed a 15 s timeout), then confirms. Clients retry only `lock_busy` (3 times, with jitter).
- **Capacity protocol** (D12). A geometric ramp (factor 1.25, 20 s steps), then **confirmation** by majority of up to three 30 s runs, then **latency at 70% of the confirmed capacity** (three runs, medians). A step counts as *sustained* only if the generator kept up, no outcome was unknown, and the hold p99 from the intended send time was under 1,000 ms. A baseline run (locking at 100 buyers/s) opens and closes each invocation to measure drift. The ramp starts at 1,000 buyers/s, because capacity was known to be above it; the start only skips levels.
- **Independent checker and reconciler after every run.** The checker reads only the database: no item with two confirmed bookings, confirmed bookings never above capacity, no stuck hold, no booking on an unconfirmed item. The reconciler compares what each client was told with the database and handles requests of unknown outcome (a kill): the booking may or may not exist, and the database decides.
- **Per-part CPU.** For every run, how much CPU the database, the API, the load generator and Redis really used, so a ceiling can be attributed to a part.
- **Negative control.** The naive strategy on a schema **without** the unique index must oversell; otherwise the checker has not been shown to work.

## Totals across every run

| Strategy | Run records | Bookings confirmed | Runs with a double-sold item | Runs oversold | Runs with a stuck hold |
|---|---|---|---|---|---|
| locking, optimistic, fast path | 347 | 12,006,755 | 0 | 0 | 0 |
| naive with the database backstop | 15 | 3,864 | 0 | 0 | 0 |
| naive, no protection (the control) | 29 | 4,732 | **10** | **9** | 0 |

"Runs oversold" counts runs where confirmed bookings exceeded capacity; a run can have a double-sold item without exceeding capacity. Records of the two aborted attempts (below) are not counted.

## E1: the oversell test

N = 10,000 buyers arriving over 10 s (1,000 buyers/s) for K = 1,000 items, skew 1.1, five runs per strategy, all reported.

| Strategy | Runs | Bookings per run | Oversold | Double-sold items | Stuck holds | Hold p50 (ms) | Hold p99 (ms) |
|---|---|---|---|---|---|---|---|
| naive-unprotected | 5 | 750 to 774 | 0 | 0 | 0 | 1.1 | 3 |
| naive-protected | 5 | 741 to 767 | 0 | 0 | 0 | 1.1 | 2 to 3 |
| locking | 5 | 758 to 788 | 0 | 0 | 0 | 1.2 | 17 to 19 |
| optimistic | 5 | 759 to 774 | 0 | 0 | 0 | 1.1 to 1.2 | 3 |
| fastpath | 5 | 755 to 782 | 0 | 0 | 0 | 1.0 | 3 to 4 |

Every strategy, including the unprotected control, finished clean: **at 1,000 items the control did not oversell in five runs.** On a fast machine two buyers rarely collide on the same item inside the check-then-update window at this arrival rate. Locking's higher p99 (17 to 19 ms against 2 to 4) is its row locks queueing. To make the control fire, contention has to be much higher, which is the next experiment.

## E1b: the same test under much higher contention

Run after E1 because the control must be shown to oversell. A small probe of three settings (naive, no protection only, three runs each, all kept) found the setting where it fired every time: **10 items, 10,000 buyers arriving within 1 s.** That setting was chosen *after* seeing the probe, and it is a deliberately extreme one. The final matrix then ran all five strategies ten times.

| Strategy | Runs | Bookings confirmed per run (capacity 10) | Oversold per run | Runs flagged |
|---|---|---|---|---|
| naive-unprotected | 10 | 6, 86, 10, 10, 65, 82, 10, 24, 10, 5 | 0, 76, 0, 0, 55, 72, 0, 14, 0, 0 | 4 of 10 |
| naive-protected | 10 | 10, 10, 8, 6, 7, 7, 6, 10, 10, 10 | 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 | 4 of 10 |
| locking | 10 | 10, 10, 6, 9, 10, 10, 10, 10, 10, 10 | 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 | 0 of 10 |
| optimistic | 10 | 5, 10, 10, 8, 8, 10, 9, 10, 10, 10 | 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 | 0 of 10 |
| fastpath | 10 | 7, 5, 10, 10, 10, 10, 9, 8, 8, 10 | 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 | 0 of 10 |

Probe (naive, no protection only):

| Setting | Runs that oversold | Double-sold items per run |
|---|---|---|
| probe a-50items-4000b-5s | 0 of 3 | 0, 0, 0 |
| probe b-20items-10000b-2s-skew1.4 | 2 of 3 | 3, 2, 0 |
| probe c-10items-10000b-1s | 3 of 3 | 2, 7, 7 |

- **The control oversold in 4 of 10 runs, up to 86 bookings for 10 items.** Locking, optimistic and fast path never exceeded 10, in any run.
- **The unique index alone is the hard guarantee.** `naive-protected` is the same naive code on the schema with the index. It had **0 oversold and 0 double-sold items in all 10 runs**. But 4 of the 10 runs were flagged for other inconsistencies: bookings on items not marked confirmed (1, 7 and 3 in three runs) and bookings the clients were never told about (1, 4, 5 and 1). The backstop keeps "a seat is never sold twice", not full consistency, which is what the protected strategies add.
- Over the whole program the control had a double-sold item in 10 of its 29 runs: 0 of 5 at 1,000 items, 5 of 9 in the probe, 4 of 10 in the final matrix, 1 of 5 in the failure experiment.

## E2: capacity and latency under rising contention

Three strategies at four skews: **12 protocols**, each with its own ladder, confirmation and latency runs; none is a lower bound (the ladder ended in a failing step every time). Run in four invocations the same day (the same ladder and settings; see the drift table).

| Confirmed capacity (buyers/s), then latency at 70% of it | skew 0 | skew 0.8 | skew 1.1 | skew 1.4 |
|---|---|---|---|---|
| locking | 3,054<br>p50 2.4 / p99 6.8 ms at 2,138/s<br>1,252 bookings/s | 4,773<br>p50 2.1 / p99 39.6 ms at 3,341/s<br>1,267 bookings/s | 5,966<br>p50 1.8 / p99 58.7 ms at 4,176/s<br>600 bookings/s | 4,773<br>p50 1.8 / p99 88.4 ms at 3,341/s<br>104 bookings/s |
| optimistic | 3,054<br>p50 2.6 / p99 14.2 ms at 2,138/s<br>1,257 bookings/s | 4,773<br>p50 1.9 / p99 40.1 ms at 3,341/s<br>1,266 bookings/s | 9,323<br>p50 1.4 / p99 38.3 ms at 6,526/s<br>900 bookings/s | 18,210<br>p50 1.5 / p99 81.3 ms at 12,747/s<br>277 bookings/s |
| fastpath | 3,054<br>p50 2.9 / p99 6.3 ms at 2,138/s<br>1,254 bookings/s | 4,773<br>p50 1.8 / p99 9.5 ms at 3,341/s<br>1,266 bookings/s | 11,654<br>p50 1.4 / p99 9.4 ms at 8,158/s<br>1,102 bookings/s | 18,210<br>p50 1.5 / p99 7.3 ms at 12,747/s<br>274 bookings/s |

- **Skew 0 and 0.8: identical.** Demand is spread, most requests succeed, and the shared connection pool is the limit for all three.
- **Skew 1.1: they separate** (fast path 11,654, optimistic 9,323, locking 5,966).
- **Skew 1.4: locking is the only strategy whose capacity falls** (5,966 to 4,773): its row locks hold connections for longer on the hot items, so the pool starves sooner. Optimistic and fast path both confirm 18,210 buyers/s.
- **At high skew most buyers lose.** Bookings per second at 70% load fall to 104 (locking) and about 275 (the others) at skew 1.4: the capacity figure is mostly fast rejections of buyers who wanted a seat that is gone.
- Latency at 70% of capacity is a median of 1.4 to 2.9 ms and a p99 of 6 to 88 ms across the twelve protocols.

### What limited each protocol

At the last sustained step and at the first failing step, how many of the API's database connection requests had to wait for the pool, and how busy each part was (percent of the cores it was pinned to):

| Strategy | Skew | Capacity | Last sustained step: pool waits / Postgres / API / generator | First failing step | First failing step: pool waits / Postgres / API / generator |
|---|---|---|---|---|---|
| fastpath | 0 | 3,054 | 5% / 41% / 34% / 17% | 3,818 | 74% / 51% / 39% / 24% |
| fastpath | 0.8 | 4,773 | 8% / 41% / 36% / 19% | 5,966 | 85% / 48% / 40% / 28% |
| fastpath | 1.1 | 11,654 | 26% / 36% / 40% / 28% | 14,568 | 89% / 41% / 44% / 37% |
| fastpath | 1.4 | 18,210 | 3% / 11% / 35% / 30% | 22,763 | 7% / 11% / 39% / 48% |
| locking | 0 | 3,054 | 8% / 43% / 31% / 17% | 3,818 | 77% / 53% / 37% / 25% |
| locking | 0.8 | 4,773 | 17% / 48% / 35% / 20% | 5,966 | 93% / 54% / 38% / 28% |
| locking | 1.1 | 5,966 | 16% / 42% / 35% / 20% | 7,458 | 98% / 52% / 41% / 33% |
| locking | 1.4 | 4,773 | 10% / 32% / 31% / 18% | 5,966 | 100% / 49% / 40% / 32% |
| optimistic | 0 | 3,054 | 4% / 41% / 32% / 17% | 4,773 | 92% / 53% / 38% / 26% |
| optimistic | 0.8 | 4,773 | 13% / 44% / 34% / 20% | 5,966 | 93% / 52% / 39% / 29% |
| optimistic | 1.1 | 9,323 | 13% / 42% / 36% / 25% | 11,654 | 95% / 49% / 41% / 36% |
| optimistic | 1.4 | 18,210 | 16% / 39% / 38% / 33% | 22,763 | 98% / 42% / 42% / 47% |

For **11 of the 12 protocols**, 74% to 100% of connection requests waited for the pool at the first failing step (3% to 26% at the last sustained step), while PostgreSQL used 41% to 54% of its cores, the API 37% to 44% and the generator 24% to 47%. **The ceiling is the API's connection pool of 32, which is a configuration choice, so these capacities would move with the pool size.** The reported failure reason is always "the generator did not keep up", but that is a symptom: requests pile up behind the pool and the generator's schedule lags.

**One exception: fast path at skew 1.4 (18,210).** Only 7% of connection requests waited at the failing step, PostgreSQL was at 11% and the API at 39%, but the generator was at 48% on average with peaks near its limit. That ceiling looks like the **load generator**, not the engine, so 18,210 is a confirmed rate of this system including its generator, and the engine may do more.

### Drift

The baseline run (locking at 100 buyers/s) opens and closes each invocation. How much it moved is the run-to-run variability to keep in mind, and it was 10% or less:

| Invocation | Hold p50 | Hold p99 | Bookings/s |
|---|---|---|---|
| E2a (three strategies, skew 1.1) | -6.7% | -9.7% | +3.0% |
| fast path skew 1.4 rerun | -4.1% | -6.2% | +7.9% |
| locking and optimistic sweep | -5.1% | +0.0% | -1.4% |
| fast path skews 0 and 0.8 | not measured | not measured | that invocation stopped before its closing baseline |

## E3: surge protection with a waiting room

On the full machine the engine's capacity is far above what one load generator can offer several multiples of, so this experiment uses the **small backend**: PostgreSQL on one core and an API pool of 8, with the strategy **locking** (the service's default). Its capacity was measured in the same session, and the waiting room admitted buyers at R = 0.8 x C. Each arm lasts 10 s. The original plan was 5x, 10x and higher; one machine's generator saturates above about 2x of this backend, so 1.5x, 2x and 2.5x were run.

Capacity of the small backend, measured in the same session: **2,835 buyers/s** (latency run at 1,984/s). The waiting room admitted R = 0.8 x C = **2,268 buyers/s**.

| Offered | Arm | Store p99 (ms) | Buyer p99 (ms) | Host CPU mean / max | Holds granted | Seat taken | Notes |
|---|---|---|---|---|---|---|---|
| 1.5x (4,253/s) | no room | 3700 | 3700 | 19% / 40% | 6,494 | 35,973 | none |
| 1.5x (4,253/s) | room | 223 | 8637 | 29% / 65% | 6,573 | 35,925 | 42,530 admitted, 0 turned away, 0 gave up, 0 FIFO inversions |
| 2x (5,670/s) | no room | 9789 | 9789 | 26% / 62% | 7,549 | 49,037 | 4 unknown outcomes |
| 2x (5,670/s) | room | 765 | 14803 | 36% / 70% | 7,720 | 48,903 | 56,700 admitted, 0 turned away, 0 gave up, 0 FIFO inversions |
| 2.5x (7,088/s) | no room | 14610 | 14636 | 30% / 78% | 8,654 | 61,007 | generator behind: not used for latency; 170 unknown outcomes |
| 2.5x (7,088/s) | room | 6483 | 23736 | 45% / 81% | 8,835 | 61,073 | generator behind: not used for latency; 70,117 admitted, 0 turned away, 0 gave up, 0 FIFO inversions; 1 unknown outcomes |

- **The room protects the store.** Store-facing p99 is 223 ms against 3,700 ms at 1.5x, and 765 ms against 9,789 ms at 2x.
- **The wait moves to the buyer.** With the room, buyers wait in the queue: p99 about 8.6 s at 1.5x and 14.8 s at 2x. Everyone was eventually admitted (0 turned away, 0 gave up).
- **It does not raise bookings.** Holds granted are about the same with and without the room (the database is the limit): it protects latency, not throughput.
- **Fairness.** 0 FIFO inversions among queued buyers in every room arm. What is guaranteed is first come, first in for buyers who keep waiting; it does not cover buyers who leave and rejoin, or one person using many buyer ids.
- **The room costs CPU.** Host CPU is higher with the room (queue polling and Redis): 29% against 19% at 1.5x, 36% against 26% at 2x.
- **2.5x is not usable.** In both arms the generator itself saturated, so those two runs are flagged and not used for latency. Valid evidence stops at 2x, on a small database.

## E4: failures under load

Each run: 3,000 buyers over 20 s (40 s for the store-restart plans) on 1,000 items, with a supervisor that restarts the API after every kill. Plans: kill the API from outside (SIGKILL) and through named crash hooks inside hold and confirm; hold expiry racing a late confirm; restart Postgres; restart the fast path's Redis; and the negative control.

| Plan | Strategy | Runs | Clean | SIGKILLs | Crash-hook exits | Redis restarts | Postgres restarts | Unknown outcomes | Double-sold | Oversold | Stuck holds |
|---|---|---|---|---|---|---|---|---|---|---|---|
| api-kills | fastpath | 5 | 5 | 10 | 15 | 0 | 0 | 1171 | 0 | 0 | 0 |
| api-kills | locking | 5 | 5 | 10 | 15 | 0 | 0 | 1239 | 0 | 0 | 0 |
| api-kills | optimistic | 5 | 5 | 10 | 15 | 0 | 0 | 1211 | 0 | 0 | 0 |
| control | naive-unprotected | 5 | 4 | 0 | 0 | 0 | 0 | 0 | 1 | 1 | 0 |
| expiry-race | fastpath | 5 | 5 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| expiry-race | locking | 5 | 5 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| expiry-race | optimistic | 5 | 5 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| postgres-restart | fastpath | 5 | 5 | 0 | 0 | 0 | 5 | 17 | 0 | 0 | 0 |
| postgres-restart | locking | 5 | 5 | 0 | 0 | 0 | 5 | 21 | 0 | 0 | 0 |
| postgres-restart | optimistic | 5 | 5 | 0 | 0 | 0 | 5 | 26 | 0 | 0 | 0 |
| redis-restart | fastpath | 5 | 5 | 0 | 0 | 10 | 0 | 0 | 0 | 0 | 0 |
| **total** |  | 55 | 54 | 30 | 45 | 10 | 15 | 3685 | 1 | 1 | 0 |

- **50 runs of the protected strategies: 0 double-sold, 0 oversold, 0 stuck holds.** 75 API kills (30 SIGKILLs and 45 exits at named points), 15 Postgres restarts and 10 Redis restarts happened under load, and every armed crash hook fired.
- **Unknown outcomes are expected and resolved.** A killed API or a restarting database leaves requests whose result the client never saw (3,621 in the API-kill runs, 64 in the Postgres restarts). The reconciler classified every one by reading the database: the booking exists or it does not.
- **The control oversold in 1 of its 5 runs.** The failure tests found no defect in the engine; the defects found during this program were in the benchmark harness (below).

## Real-kill crash tests

The nine crash rows of the [crash analysis](crash-analysis.md) (plus two negative controls) each ran five times against the real API binary, which a hook makes exit with status 137 at an exact point. A skipped run counts as a failure.

**55 runs, 55 real kills (exit status 137), 55 passed.**

| Crash-analysis row | Hook | Strategy | Runs | Real kills | Passed |
|---|---|---|---|---|---|
| 6 | `hold.after_gate_claim` | fastpath | 5 | 5 | 5 |
| 8 | `hold.before_commit` | locking | 5 | 5 | 5 |
| 9 | `hold.before_reply` | locking | 5 | 5 | 5 |
| 9-control | `hold.before_reply` | naive | 5 | 5 | 5 |
| 10 | `hold.before_gate_extend` | fastpath | 5 | 5 | 5 |
| 12 | `confirm.after_marker` | locking | 5 | 5 | 5 |
| 13 | `confirm.during_payment` | locking | 5 | 5 | 5 |
| 15 | `confirm.after_payment` | locking | 5 | 5 | 5 |
| 16 | `confirm.before_commit` | locking | 5 | 5 | 5 |
| 17 | `confirm.before_reply` | locking | 5 | 5 | 5 |
| 17-control | `confirm.before_reply` | naive | 5 | 5 | 5 |

## What went wrong and what it taught

Writing these down matters as much as the results:

- **A harness defect that made the waiting room look broken.** In a rehearsal, 45,936 of 56,700 joins were answered "already admitted" and the room did not throttle at all. The cause was in the benchmark, not the room: the reset between runs wiped the cache's keys but not the waiting room's, and 128,304 passes from the previous arm were still in Redis (buyer ids repeat across arms). The reset now clears both and a test fails without it. Every conclusion about the room before the fix was discarded. Lesson: a result that looks like "the protection does not work" can be a measurement hygiene failure; read the counters before concluding.
- **A measurement artefact.** Some per-part CPU maxima showed impossible values (a generator above 600% of its cores) because a starved sampler received ticks back to back. Short windows are now merged into the next one. The means were never affected.
- **Limits of the harness itself.** An event of more than 500,000 items was refused by the API (raised, then raised again to 4,000,000 when one protocol aborted), and the generator's cap of 20,000 buyers in flight skipped most of a surge. Both became recorded settings.
- **Two attempts were stopped and kept.** One protocol (fast path, skew 1.4) aborted because the event limit was too small, and was rerun; an early surge run reported its capacity as a lower bound (the ramp's default top) and was stopped. Both are kept under `e2-skew-aborted-skew1.4` and `e3-aborted-lowerbound-fastpath` and are not used.
- **The surge experiment used the service's default strategy, not the best one.** Fast path's capacity on the small backend is more than a single generator can offer even 1.5x of.
- **Two protocols have no closing baseline.** The first invocation of the fast path skew sweep stopped before its closing baseline, so its drift was never measured for skews 0 and 0.8. Their capacities equal those of the other strategies at the same skew, measured later in a properly bracketed invocation, which is consistent but not a substitute.

## What the data does and does not show

- **It shows** that a seat was never sold twice across 347 run records of three strategies, including 75 API kills, 25 store restarts and 55 real-kill crash tests; that the checker can see a double sale; that strategy choice matters under hot-item contention and not otherwise; that the first ceiling on this machine is a configuration limit (the pool); and that a waiting room cuts store-facing latency under overload at the cost of queueing time.
- **It does not show** production capacity (one machine, a simulated payment, simulated demand, the load generator on the same machine), behaviour with a real payment provider, behaviour under surges of 5x and above, scaling across several API instances (not run), survival of disk loss or long outages, or that the capacities hold with a different pool size.
- **Variability** between invocations was within 10% on the baseline run, and 18,210 for fast path at skew 1.4 is probably generator-limited.

More in [limitations.md](limitations.md).

## Where the data is

- `docs/data/2026-11-14-linux-vm/`: `e1/` (25 runs), `e2-strategies/` (three protocols at skew 1.1), `e2-skew/` (fast path at skews 0 and 0.8), `e2-skew14/` (fast path at skew 1.4), `e3/` (the surge), `e4/` (55 runs), `crash-tests/`, and the two aborted attempts.
- `docs/data/2026-11-14-linux-vm-contention/`: the probe and the final high-contention matrix (E1b).
- `docs/data/2026-11-14-linux-vm-skew-sweep/`: locking and optimistic at skews 0, 0.8 and 1.4.
- Each run has a JSON record (its parameters, environment, client summary, server metrics, per-part CPU, checker and reconciler results). The per-buyer logs next to them (`*.buyers.ndjson.gz`, about 1 GB) are large and are not committed; every file was verified against the machine by SHA-256.
- The runs came from commits `334340a` (E1, the first E2 invocations) and `cfbf836` (everything after); they differ only in the event size limit and in recording the commit id. Every record names its own commit.

## Reproducing it

```bash
scripts/vm/setup.sh                                  # one time, on a fresh Ubuntu 24.04 machine
scripts/vm/run-pinned.sh e1 -runs 5 -out docs/data/<date>-linux-vm/e1
scripts/vm/run-pinned.sh e2 -strategies locking,optimistic,fastpath -skews 1.1 -start 1000 -max-rate 24000 -out ...
BACKEND=small scripts/vm/run-pinned.sh e3 -strategy locking -factors 1.5,2,2.5 -duration 10s -max-rate 8000 -out ...
scripts/vm/run-pinned.sh e4 -start 1000 -max-rate 24000 -out ...
scripts/crash-tests.sh
python scripts/results-tables.py                     # rebuild every table above from the records
```

See [deployment.md](deployment.md) for the machine, the core plan and the checks that run before a measurement.
