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
