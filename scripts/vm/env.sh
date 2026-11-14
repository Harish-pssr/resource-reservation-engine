#!/usr/bin/env bash
# Sourced by the other scripts in this folder (not run directly): loads the core plan and exports what the
# benchmark records with every result. PIN=0 turns pinning off for a smoke run on a laptop; the record then
# says nothing was pinned, so such a run can never pass for a pinned one.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# BACKEND=small selects the constrained database profile (pinning-small.env); the default is the full machine.
if [ "${BACKEND:-full}" = "small" ]; then PINNING_FILE="${PINNING_FILE:-$ROOT/scripts/vm/pinning-small.env}"; fi
set -a
# shellcheck disable=SC1091
. "${PINNING_FILE:-$ROOT/scripts/vm/pinning.env}" # PINNING_FILE: only for testing a different plan
set +a
export PATH="/usr/local/go/bin:$PATH"
if [ "${PIN:-1}" = "1" ]; then
  export BENCH_PINNING="postgres=$PG_CPUS api=$API_CPUS bench=$BENCH_CPUS redis=$REDIS_CPUS spare=$SPARE_CPUS"
  # the override gives the two stores their cores; it is loaded wherever docker compose runs (also by bench e4's restarts)
  export COMPOSE_FILE="docker-compose.yml:docker-compose.vm.yml"
else
  export BENCH_PINNING="" BENCH_MACHINE_TYPE="" BENCH_BACKEND_PROFILE=""
  unset BENCH_DB_POOL_SIZE
  unset COMPOSE_FILE
fi
