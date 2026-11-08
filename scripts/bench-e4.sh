#!/usr/bin/env bash
# E4, failures under load: real API kills, Redis and Postgres restarts, an expiry race. Needs scripts/up.sh.
# Extra arguments go to `bench e4`, e.g. scripts/bench-e4.sh -runs 5 -out docs/data/<date>-linux-vm
# On a server, pass -restart-redis-cmd and -restart-postgres-cmd (the defaults restart the Docker containers).
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build.sh
exec bin/bench e4 "$@"
