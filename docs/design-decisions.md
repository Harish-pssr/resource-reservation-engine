# Design decisions

Short public summary. Code and documents refer to these as D1, D2, ...

## D1: Go + Postgres + Redis
Go for cheap concurrency (an open-loop load generator with 10,000+ virtual buyers) and direct control of transactions and locks through pgx. Postgres is the system of record; Redis is only the fast path, behind an interface so it can be killed without affecting correctness.
Cost accepted: final benchmark numbers need a native Linux install, not Docker Desktop.

## D2: One row per item, plus a bookings ledger with a unique backstop
Each sellable unit is a row in `items` (state: free, held, confirmed, with CHECK constraints). Every confirmation also appends a row to `bookings`, which has a partial unique index `UNIQUE (item_id) WHERE status = 'confirmed'`. That index is the hard guarantee: even a buggy confirm cannot record a second booking for one item, whatever the application or cache does.
The naive negative control runs against an *unprotected* schema variant (no unique index, no state guard in its UPDATE), so it can oversell and the independent checker can see it from the database alone. It is also run on the protected schema, where it fails with unique-violation errors instead of overselling.
Cost accepted: two schema variants, one extra table. Counter-style inventory (fungible products) is out of scope.

## D3: Lazy hold expiry, no background sweeper
A hold stores `expires_at`. An item is available if its state is `free`, or if it is `held` and `expires_at < clock_timestamp()`. Nothing sweeps expired holds; a reserve simply takes the item over. Because expiry is derived from a timestamp in the database, a restart of any process or of Redis cannot leave an item stuck.
Guards: time comes from the database clock (`clock_timestamp()`, not the application clock and not `now()`, which is frozen at transaction start); every takeover bumps a `hold_version` fencing counter that confirm must present; one shared availability predicate and a supporting index are used by reserve, the availability view and the checker.
Cost accepted: stale `held` rows remain until taken over, the previous holder's details are overwritten, and the checker's "no stuck hold" rule is defined as "every expired hold can be taken over", verified by a real reserve.

## D4: A late confirm wins if the seat was not taken
A hold is a revocable lease: it protects a buyer from contention, it does not punish slow payment. Confirm is one atomic conditional update (`item_id`, `hold_version` and `state = 'held'` must match; no time check) together with the ledger insert in one transaction. A confirm that arrives after `expires_at` therefore succeeds if nobody took the item over, and is rejected with a distinct outcome if another buyer did (their takeover bumped `hold_version`). Release and payment failure use the same version check, so a stale release cannot free another buyer's hold. Outcomes are counted: confirmed in time, confirmed after expiry, rejected because the item was taken.
Cost accepted: the hold time is a soft limit (an untouched item can be confirmed late), and a buyer who already paid can still lose the race; the payment is simulated, so nothing is refunded and the case is only counted.

## D5: Idempotency from the data, no key table
Retries are made safe by the schema rather than by a separate idempotency-key table. The client supplies a `request_id`, stored on the item row as `hold_request_id`: the same id returns the same hold, a new id is a new attempt. Bookings store `buyer_id` and `hold_version`, so a retried confirm that matches returns the existing booking. Before the payment step a conditional update (guarded by `hold_version`) sets `payment_started_at`; a retry that sees an unexpired marker gets HTTP 409 "in progress", and the marker goes stale after the payment timeout so a crashed attempt can be retried. Release is an idempotent no-op if the hold is no longer the caller's. Redis is never used for deduplication.
Cost accepted: replies to retries are reconstructed rather than replayed verbatim, one extra write per confirm, and buyer identity is a client-supplied id (no authentication).

## D6: Pessimistic locking with NOWAIT and ordered locks
A request names 1 to M specific items and is all-or-nothing. The locking strategy runs `SELECT ... FOR UPDATE NOWAIT` on the items in ascending `item_id` order, inside a short transaction that is never held across the payment step. If a row lock is busy the caller gets `LOCK_BUSY` (retry shortly); if the item is already held or confirmed the caller gets `UNAVAILABLE`. Since no transaction waits on a row lock, lock cycles cannot form; ordered locking is kept anyway, and an off-by-default `wait` mode (with `lock_timeout`) exists to measure queueing and, with deliberately unordered locking, to show that deadlocks do occur without the protection.
Cost accepted: retry traffic on hot items (clients back off with jitter), and one extra code path kept for benchmarking. Requests for "any N units" of a product are out of scope (see limitations; same rows with `SKIP LOCKED`).
