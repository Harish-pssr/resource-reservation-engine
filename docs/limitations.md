# Limitations

What this project does not do, what its measurements do not show, and how each gap could be closed. Read it next to [benchmarks.md](benchmarks.md) and [crash-analysis.md](crash-analysis.md).

## What the measurements do not show

- **One machine, one session.** All runs were taken on one 16-vCPU cloud VM on one day, with the database, the cache, the API and the load generator on the same machine (each on its own cores). The numbers describe this machine and this core split, not a production deployment. Variability between invocations of the baseline was within 10%, but the capacities were each measured once.
- **A simulated payment.** The payment is a delay, a failure rate and a timeout. A real provider has real latency, real outcomes after a timeout, and real duplicate-charge risk (see below). Every claim holds "with a simulated payment".
- **Simulated demand.** An open-loop generator with a Zipf-skewed item popularity and a fixed distribution of request sizes (1 to 6 items). Real demand has bursts, retries by humans, bots and correlated behaviour that this does not model.
- **Capacity is a connection-pool ceiling.** In 11 of 12 capacity protocols the first thing to run out was the API's database pool of 32 connections, not hardware. Different pool sizes would move every capacity figure. The fast path's 18,210 buyers/s at skew 1.4 looks limited by the load generator instead, so it is a floor for the engine, not its limit.
- **Buyers per second are attempts.** At high skew most buyers lose, so bookings per second are far lower (about 275 at the highest skew).
- **The waiting room was shown only to 2x.** It was measured on a deliberately small database (one core, a pool of 8) at 1.5x and 2x its capacity. Above about 2x one load generator became the limit, so 5x, 10x and beyond were not measured. It was measured with the locking strategy only.
- **No scaling experiment.** Throughput as API instances go from 1 to 4 was not run, and two live API instances were never exercised together (crash-analysis row 22).
- **The negative control needs contention to fire.** The naive strategy did not oversell at 1,000 items; it oversold in 4 of 10 runs at 10 items (up to 86 sold of 10) and once in five runs of the failure experiment. The contention setting was chosen after a probe.
- **Two protocols have no closing baseline.** Fast path at skews 0 and 0.8 ran in an invocation that stopped before its closing baseline, so their drift was not measured.
- **No long outages.** Postgres and Redis were restarted briefly under load; nothing was tested about disk loss or an outage of minutes.
- **The failure tests found no defect in the engine.** That is a result, not proof that none exists: the failure space is larger than the 75 kills, 25 store restarts and 55 crash tests run here.

## What the engine does not handle

- **A real payment can be taken twice.** If the API dies during or just after a payment, a retry after the payment marker goes stale pays again unless the provider deduplicates on a key (for example the hold id). The simulated provider keeps no state, so this is documented, not solved (crash-analysis rows 13 and 15). It cannot cause a double *sale*, only a possible double *charge*.
- **A payment timeout has an unknown real outcome.** The engine treats a timeout as a failure and releases the hold. With a real provider the charge may have succeeded. A production system would need an asynchronous reconciliation with the provider.
- **"Paid but lost" is counted, not refunded.** If a buyer pays after their hold expired and another buyer took the item meanwhile, the confirm is refused and the case is only counted, because the payment is simulated.
- **There is no cancellation or refund flow** after a booking is confirmed, and no waitlist for a sold-out event.
- **Buyer identity is unauthenticated.** A buyer is a client-chosen string, so one person can use many ids and the waiting room cannot stop it. A production system needs real accounts and rate limits per identity.
- **The waiting room's polling cost grows with the crowd.** Every waiting buyer polls, so the load on the API (and on Redis) grows with the number of people waiting, not with the admission rate. At very large crowds the polling itself becomes the bottleneck; a production system would push the queue to an edge service or use server-sent updates. Order is also lost if Redis restarts (buyers rejoin at the back).
- **A single Redis instance.** The waiting room's scripts build per-buyer keys inside the script, so they need one Redis instance (a cluster would need hash tags). Redis is never needed for correctness, only for speed and admission control; if it is down the engine fails open to Postgres and is correct but unprotected.
- **The fast path can wrongly refuse a free item** for up to its pending lifetime (2 s) after a crash between the gate claim and the database write: a lost sale, never a double sale. It is tested and counted.
- **Expired holds are not cleaned up.** By design nothing sweeps them (an item is taken over when next requested), so a stale `held` row remains until then. Views compute the effective state on the database clock.
- **The hold time is soft.** A late confirm wins if nobody took the item, so a hold does not strictly limit how long a buyer can take.
- **Requests are for named items.** "Any N seats" of a product (fungible inventory) is out of scope; the same rows with `SKIP LOCKED` would be the way.
- **Events have an upper size** (4,000,000 items) and one row per item, which suits seat maps and ticket inventory, not counters of millions of identical units.
- **The console is a demonstration.** It has an admin token for the failure controls and the checker, but no user accounts, and the development compose file publishes the database and Redis ports on all interfaces. Fault injection is off by default and must never be enabled on a shared or public deployment.
- **Clock handling is by construction, not by test.** Every expiry decision uses the database clock, so clock differences between API servers cannot matter; this was not tested separately (crash-analysis row 23).

## How a production system would close the gaps

A payment provider with idempotency keys and an asynchronous outcome check; per-identity authentication and rate limits; a queue at the edge with push updates instead of polling; a Redis deployment with replication for the room (still treated as a hint); a bounded set of pooled connections sized from a measured capacity; and a scaling experiment across several API instances against one database.
