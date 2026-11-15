# Crash analysis: what happens when each step fails

This document follows one booking through the system and asks, at every step: **what if the process dies (or the dependency fails) right here?** For each point it states what is left behind, how the system recovers, whether a double sale or a duplicate effect can result, and how the claim is tested. It is deliberately honest about what is **not** tested yet.

The ideas that make recovery work:

- **Postgres is the system of record, and the only arbiter** (D2). A hold or a booking exists once its transaction commits. Redis (the fast-path gate and the waiting room) is a hint: it may be empty, stale or down, and correctness never depends on it (D8, D9).
- **Time comes from the database clock** (D3), and a hold carries a version that a stale caller cannot reuse (D3, D4). Nothing relies on a background process to free an item, so no process can die and leave one stuck.
- **Retries are safe by construction** (D5): the same request id returns the same hold, a retried confirm is recognised, and a marker stops a second payment from starting while the first is in flight.

How to read the *Tested* column. **test** = an automated test that runs against real Postgres and Redis (named). **simulated** = the condition is reproduced without a real crash (for example, a leaked Redis claim is created directly, or a retry is sent by hand). **real kill** = a real process kill or store restart under load; **no real kill has been run yet** (slice 5), so every such cell currently says *planned*. **not tested** = reasoned from the code, never provoked.

## The flow, step by step

```text
 buyer -> (1) join queue -> (2) poll, admitted -> (3) POST /holds -> (4) pass check -> (5) gate claim
       -> (6) reserve: read or lock -> (7) write the hold -> (8) commit -> (9) gate extend -> (10) reply
       -> (11) POST /holds/confirm -> (12) payment marker -> (13) payment -> (14) confirm: write booking, commit
       -> (15) gate sold marker -> (16) reply

 failure path:  (17) payment fails or times out -> release        (18) a hold simply expires on the clock
```

| # | Failure at this point | What is left behind | Recovery | Double sale or duplicate? | Tested |
|---|---|---|---|---|---|
| 1a | Redis is down when a buyer joins the queue | Nothing | The room **fails open**: the buyer gets an `open` pass and holds go straight to the database. Correct, but unprotected against a surge | No | test `TestRoomFailsOpenWhenRedisIsDown` |
| 1b | Redis loses the queue (restart) while buyers wait | Buyers hold tickets that no longer exist | A poll answers `not_queued`; the client joins again at the back. Order is lost, nothing else | No | simulated: `TestPollOfAnUnknownBuyerIsNotQueued` (queue wiped). The client's rejoin path is **not tested**; real Redis restart: *planned* |
| 2 | The API instance that runs the admission loop dies | The queue stays in Redis | Other instances keep admitting, because the rate budget lives in Redis. With a single instance, admission pauses until it restarts; nobody is lost | No | the global budget: `TestAdmissionRateIsGlobalAcrossCallers`. An instance dying: **not tested** |
| 3 | The pass is missing, forged, someone else's, or expired | Nothing | `POST /holds` answers 403 `not_admitted`; the buyer rejoins the queue | No | tests `TestHoldNeedsAnAdmissionPass`, `TestPassValidationAndExpiry` |
| 4 | The API dies before it does anything with the request | Nothing | The client retries | No | not tested (nothing to recover) |
| 5 | The gate claim fails (Redis error) | Nothing | **Fail open**: the request goes to the database, which decides | No | test `TestFailsOpenWhenRedisIsDown` |
| 6 | The API dies **after the gate claim, before the database reserve** | A Redis claim with a short pending TTL (2 s); no hold | The claim lapses by itself; the item may be falsely refused for at most that long (a lost sale, never a double sale) | No | simulated: `TestLeakedClaimLapsesAfterThePendingTTL` (the claim is created directly); real kill: `TestRow6KilledAfterGateClaim` |
| 7 | The reserve loses a race (row locked, or an optimistic write finds the row changed) | Nothing; the claim is dropped | `lock_busy` (retry) or `unavailable` (final); no server-side retry loop | No | tests `TestLostRaceThatTakesTheItemIsUnavailable`, `TestSpuriousConflictIsLockBusy`, the suite case `concurrent holds: exactly one wins` and the control experiment |
| 8 | The API dies **mid-transaction, before commit** (or the write fails) | Nothing: Postgres rolls the transaction back, so no half-held multi-item request | The client retries; the retry is a first request. The Redis claim lapses | No | test `TestLostRaceOnLaterItemRollsBackEarlierOnes` (a failed multi-item write leaves nothing); real kill: `TestRow8KilledBeforeHoldCommit` |
| 9 | The API dies **after the commit, before the reply** (the client never learns it worked) | The hold exists, in full | The client retries with the **same request id**: the same hold comes back, with the same expiry (a retry never extends it) | No | simulated: `hold retry with the same request id is idempotent` (a retry sent by hand); real kill: `TestRow9KilledBeforeHoldReply` |
| 10 | The API dies **after the commit, before the gate is extended** | The hold in Postgres; in Redis only a 2 s claim | The claim lapses; later buyers pass the empty gate, the database refuses them, and the gate refills from that answer | No | test `TestEmptiedGateIsSafeAndRefillsFromTheDatabase` (gate wiped); real kill: `TestRow10KilledBeforeGateExtend` |
| 11 | Postgres is **down or restarting** during a reserve | Nothing committed; in-flight transactions roll back | The API returns an error; the client treats the outcome as unknown and retries with the same request id. When Postgres returns, work resumes | No | real restarts under load: `bench e4` plan `postgres-restart` (15 restarts, 0 violations, see [benchmarks.md](benchmarks.md)) |
| 12 | The API dies **after the payment marker commits, before the payment starts** | The marker (`payment_started_at`) | Retries get 409 `payment_in_progress` until the marker is older than the payment timeout plus slack; then a retry may start the payment | No | test `payment marker goes stale after the timeout`; real kill: `TestRow12KilledAfterPaymentMarker` |
| 13 | The API dies **during the payment** | The marker; the hold; the real payment's outcome is unknown | Same as 12. **In a real system the payment may have been taken**, and a retry after the marker goes stale would pay again unless the provider deduplicates on a key (for example the hold id). The simulated provider has no state, so this is **a limitation, not solved** (see [limitations.md](limitations.md)) | A possible double charge with a real provider | simulated provider only; real kill: `TestRow13KilledDuringPayment` |
| 14 | The payment **fails or times out** | A hold the buyer no longer wants | The server releases it, on a context that survives a client disconnect. A timeout is treated as a failure although its real outcome is unknown | No | tests `TestPaymentFailureReleasesTheHold`, `TestPaymentTimeoutReleasesTheHold`; the disconnect case: **not tested** |
| 15 | The API dies **after the payment succeeded, before the confirm commits** | Paid, not booked; the hold persists (a hold is a soft lease, D4) | A retry (after the marker goes stale) confirms if the hold is still theirs. If someone took the item meanwhile the confirm is rejected: **paid but lost**, counted and not refunded (the payment is simulated) | No double sale | late-confirm tests (`late confirm on an untouched item wins`, `late confirm after takeover is rejected`) and the `paid_but_lost` counter; real kill: `TestRow15KilledAfterPayment` |
| 16 | The confirm transaction **fails** (database error) | Nothing booked; the hold intact | The buyer retries | No | real kill: `TestRow16KilledBeforeConfirmCommit` |
| 17 | The API dies **after the confirm commits, before the reply** | The booking exists | A retried confirm finds it and answers `confirmed, replayed`; no second booking | No | simulated: `confirm retry is idempotent`, `TestConfirmRetryAfterSuccessIsReplayed`; real kill: `TestRow17KilledBeforeConfirmReply` |
| 18 | The gate's sold marker fails to write | Nothing | Harmless: the next request reaches the database, which answers `unavailable`, and the gate refills | No | not tested separately; the failed-open counter is tested |
| 19 | A confirm and a **takeover of the expired hold** happen at the same instant | One writer wins on the row | The conditional write decides: either the late confirm wins (nobody took the item) or it is rejected. Never both; the unique index is the final backstop | No | test `confirm racing a takeover never double sells` (25 rounds); under load: `bench e4` plan `expiry-race` (15 runs, 0 violations) |
| 20 | **Redis restarts under load** (fast path) | An empty gate | Requests reach the database, which stays correct; the database load rises and then falls as the gate refills. Reported, not hidden | No | simulated: wiping the gate, `TestWipingTheGateDuringLoadNeverDoubleSells`, and a gate that protects nothing, `TestBlindGateInFrontOfLockingNeverDoubleSells` (with a negative control); real restarts under load: `bench e4` plan `redis-restart` (10 restarts of the fast path's Redis, 0 violations) |
| 21 | **Postgres restarts briefly under load** | In-flight transactions rolled back; committed state intact | Errors during the restart are unknown outcomes; afterwards the checker and the reconciler decide whether each one booked | No | `bench e4` plan `postgres-restart`: 15 restarts, 64 requests of unknown outcome, every one classified by the reconciler, 0 violations |
| 22 | **Two API instances** serve the same items | None | The row is the single point of decision, so instances cannot disagree | No | tests with concurrent buyers; two real instances: *planned* |
| 23 | **Clocks differ** between instances | None | Not an issue by design: every expiry decision uses the database clock | No | not tested (by construction) |

## Reading the table in one sentence each

- **Before a commit** nothing exists, so the client simply retries.
- **After a commit** the hold or booking exists, and every later failure is a retry of an idempotent step.
- **The only place a duplicate effect can arise** is "the payment happened but the record of it was lost" (rows 13 and 15). It cannot produce a double sale, because Postgres alone arbitrates, but with a real payment provider it could produce a double charge.
- **No step holds a lock across a network call**: the reserve transaction is short, and the payment runs outside any transaction.

## What is tested for real, and what is not

**Real process kills now cover the API instance** (rows 6, 8, 9, 10, 12, 13, 15, 16 and 17). Each real-kill test named in the table builds the real API binary, arms the row's hook with a crash, drives the request, and checks that the process really exited with status 137 and the client saw a broken connection. It then restarts the server (the one-shot fault stays consumed in the database, so it does not crash again), retries as a client would, and checks what the database holds and what the client is told, finishing with the independent checker. The scenarios are repeated (`scripts/crash-tests`), and two of them are also run against the naive strategy as negative controls: there the retry of a hold, and the retry of a confirm, that had already committed are answered wrongly (and in the confirm case a second payment is started), which shows the tests can detect the missing protection. The server's own evidence is in `docs/data` once the final runs are taken in the final environment.

**Under load, with real kills and restarts (`bench e4`, 55 runs on a Linux VM, [benchmarks.md](benchmarks.md)):** 75 API kills (30 SIGKILLs from outside and 45 crash-hook exits at named points), 15 Postgres restarts, 10 Redis restarts of the fast path's cache, and the expiry-versus-late-confirm race (rows 6 to 21 under load). Every run was judged by the independent checker and the reconciler: 0 double-sold, 0 oversold, 0 stuck holds in all 50 runs of the protected strategies. The same plans also ran a negative control (the naive strategy with no database protection), which oversold in 1 of 5 runs.

**Still simulated, or not yet run for real:** a real Redis restart while the waiting room holds a queue (row 1b: the room was not enabled in those runs, and the client's rejoin path is not tested), an API instance dying while it is the one running the admission loop (row 2), the disconnect case of row 14, and two live API instances (row 22).

See [limitations.md](limitations.md) for what the system does not handle and how it could be handled in production.
