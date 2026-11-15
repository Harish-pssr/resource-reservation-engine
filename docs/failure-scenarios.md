# Failure scenarios

Each scenario below says what goes wrong, what the system does, how to see it yourself, and what evidence backs it. The step-by-step table of every failure point is in [crash-analysis.md](crash-analysis.md) (row numbers below refer to it); the measured results are in [benchmarks.md](benchmarks.md). The one rule behind all of them: **PostgreSQL is the only arbiter**, so no failure of the API, a cache or a payment can produce a second booking of one item.

How to try them: start the system with `scripts/up` and open the **Failures** page (it needs the admin token that `up` prints). Arming a hook makes the server really exit at that point; `up` restarts it.

## 1. The reply to a hold is lost (row 9)

**What happens.** The server commits the hold, then dies before it answers. The buyer sees a broken connection and does not know whether they hold the seats.
**What the system does.** The buyer retries with the **same `request_id`**. The same hold comes back, with the same expiry (a retry never extends it) and the same version. There is no second hold.
**See it.** Failures page, demo "Killed before the hold reply": it holds, kills the server, retries, and shows the same hold version.
**Evidence.** `TestRow9KilledBeforeHoldReply` (a real kill); 75 API kills under load in the failure experiment, 0 double-sold.

## 2. The reply to a confirm is lost (row 17)

**What happens.** The booking commits, then the server dies before replying.
**What the system does.** A retried confirm finds the booking and answers `confirmed` with `replayed: true`. No second booking, no second payment.
**See it.** Failures page, demo "Killed before the booking reply".
**Evidence.** `TestRow17KilledBeforeConfirmReply`; `TestConfirmRetryAfterSuccessIsReplayed`.

## 3. The server dies in the middle of a transaction (rows 8 and 16)

**What happens.** The process exits before the hold or the booking commits.
**What the system does.** PostgreSQL rolls the transaction back, so nothing exists: no half-held multi-item request (the requests are all-or-nothing) and no booking without its item. The client retries as if it were a first request.
**Evidence.** `TestRow8KilledBeforeHoldCommit`, `TestRow16KilledBeforeConfirmCommit`; `TestLostRaceOnLaterItemRollsBackEarlierOnes`.

## 4. The server dies after the payment, before the booking (row 15)

**What happens.** The buyer's payment succeeded, but the confirm never committed.
**What the system does.** The hold persists (it is a soft lease). A retry, once the payment marker goes stale, confirms if the hold is still the buyer's. If another buyer took the item in the meantime, the confirm is refused as `hold_lost`: **paid but lost**, which is counted and, because the payment is simulated, not refunded.
**Not solved.** With a real payment provider the retry could pay a second time unless the provider deduplicates on a key (rows 13 and 15). It cannot cause a double *sale*, only a possible double *charge*; see [limitations.md](limitations.md).
**Evidence.** `TestRow15KilledAfterPayment`; the late-confirm tests; the `paid_but_lost` counter.

## 5. The payment fails or times out (row 14)

**What happens.** The simulated payment returns a failure, or takes longer than its timeout.
**What the system does.** The server releases the hold (on a context that survives a client disconnect), so the seats are free again at once. A timeout is treated as a failure even though its real outcome is unknown.
**Evidence.** `TestPaymentFailureReleasesTheHold`, `TestPaymentTimeoutReleasesTheHold`. Under the realistic profile 10% of payments fail and 2% time out in every benchmark run.

## 6. A slow buyer and a taker meet (row 19)

**What happens.** A hold expires. The slow buyer's confirm and another buyer's takeover arrive at the same instant.
**What the system does.** One conditional write on the row decides. Either the late confirm wins (nobody took the item) or it is refused. Never both; the unique index on bookings is the final backstop.
**Evidence.** `confirm racing a takeover never double sells` (25 rounds); the `expiry-race` plan of the failure experiment (15 runs, 0 violations).

## 7. Redis restarts under load (row 20)

**What happens.** The fast path's cache loses all its state in the middle of a busy period.
**What the system does.** Requests reach the database, which stays correct; the database load rises and then falls as the gate refills from the database's own answers. Nothing is stuck, because claims expire.
**Evidence.** `TestWipingTheGateDuringLoadNeverDoubleSells`; **10 real Redis restarts under load, 0 violations**.

## 8. Redis is down for the waiting room (row 1a)

**What happens.** Buyers cannot be queued.
**What the system does.** The room **fails open**: a buyer gets an open pass and holds go straight to the database. Correct, but no longer protected against a surge.
**Evidence.** `TestRoomFailsOpenWhenRedisIsDown`.

## 9. PostgreSQL restarts under load (rows 11 and 21)

**What happens.** The database restarts for a few seconds while buyers are arriving.
**What the system does.** In-flight transactions roll back, committed state is intact, and requests during the gap fail. The client treats such a result as an **unknown outcome** and retries with the same request id. Afterwards the independent checker and the reconciler decide whether each unknown request booked.
**Evidence.** **15 real restarts under load, 64 requests of unknown outcome, every one classified, 0 violations.**

## 10. The server is killed again and again under load

**What happens.** The API is killed from outside (SIGKILL) and through crash hooks at named points inside hold and confirm, repeatedly, while buyers keep arriving. A supervisor restarts it after every kill.
**What the system does.** Each kill leaves a request of unknown outcome; the reconciler classifies it from the database.
**Evidence.** **75 kills** (30 SIGKILLs and 45 hook exits) across three strategies: 3,621 unknown outcomes, 0 double-sold, 0 oversold, 0 stuck holds.

## 11. Two buyers, one seat

**What happens.** Thousands of buyers ask for a handful of items at the same moment.
**What the system does.** Locking, optimistic and the fast path never let two buyers win the same item. The naive strategy with no database protection does not: it sold up to 86 seats of 10.
**Evidence.** The high-contention oversell test: 0 of 30 protected runs violated, against 4 of 10 for the control. The naive code with only the unique index kept the hard guarantee (0 seats sold twice in 10 runs) while leaving other inconsistencies, which is why the protected strategies exist.

## 12. A surge arrives (waiting room)

**What happens.** Several times more buyers than the database can serve arrive at once.
**What the system does.** With the waiting room, buyers are admitted at a fixed rate in the order they arrived; the rest wait in the queue. The database sees a steady load instead of a flood.
**Evidence.** On a small database at 2x its capacity, the store's p99 was 765 ms with the room and 9,789 ms without; buyers waited about 15 s at p99, nobody was turned away, and there were no order inversions. It was not measured above 2x (see [limitations.md](limitations.md)).

## What is still not covered

A real Redis restart while buyers wait in the waiting room (row 1b), an API instance dying while it is the one running the admission loop (row 2), the disconnect case of row 14, and two live API instances serving the same items (row 22).
