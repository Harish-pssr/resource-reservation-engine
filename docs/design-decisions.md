# Design decisions

Short public summary. Code and documents refer to these as D1, D2, ...

## D1: Go + Postgres + Redis
Go for cheap concurrency (an open-loop load generator with 10,000+ virtual buyers) and direct control of transactions and locks through pgx. Postgres is the system of record; Redis is only the fast path, behind an interface so it can be killed without affecting correctness.
Cost accepted: final benchmark numbers need a native Linux install, not Docker Desktop.

## D2: One row per item, plus a bookings ledger with a unique backstop
Each sellable unit is a row in `items` (state: free, held, confirmed, with CHECK constraints). Every confirmation also appends a row to `bookings`, which has a partial unique index `UNIQUE (item_id) WHERE status = 'confirmed'`. That index is the hard guarantee: even a buggy confirm cannot record a second booking for one item, whatever the application or cache does.
The naive negative control runs against an *unprotected* schema variant (no unique index, no state guard in its UPDATE), so it can oversell and the independent checker can see it from the database alone. It is also run on the protected schema, where it fails with unique-violation errors instead of overselling.
Cost accepted: two schema variants, one extra table. Counter-style inventory (fungible products) is out of scope.
