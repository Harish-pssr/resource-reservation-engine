#!/usr/bin/env bash
# Sourced by the other scripts in this folder (not run directly): loads the core plan and exports what the
# benchmark records with every result. PIN=0 turns pinning off for a smoke run on a laptop; the record then
# says nothing was pinned, so such a run can never pass for a pinned one.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
set -a
# shellcheck disable=SC1091
. "$ROOT/scripts/vm/pinning.env"
set +a
export PATH="/usr/local/go/bin:$PATH"
if [ "${PIN:-1}" = "1" ]; then
  export BENCH_PINNING="postgres=$PG_CPUS api=$API_CPUS bench=$BENCH_CPUS redis=$REDIS_CPUS spare=$SPARE_CPUS"
  # the override gives the two stores their cores; it is loaded wherever docker compose runs (also by bench e4's restarts)
  export COMPOSE_FILE="docker-compose.yml:docker-compose.vm.yml"
else
  export BENCH_PINNING="" BENCH_MACHINE_TYPE=""
  unset COMPOSE_FILE
fi
