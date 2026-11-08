# Design decisions

Short public summary. Code and documents refer to these as D1, D2, ...

## D1: Go + Postgres + Redis
Go for cheap concurrency (an open-loop load generator with 10,000+ virtual buyers) and direct control of transactions and locks through pgx. Postgres is the system of record; Redis is only the fast path, behind an interface so it can be killed without affecting correctness.
Cost accepted: final benchmark numbers need a native Linux install, not Docker Desktop.
