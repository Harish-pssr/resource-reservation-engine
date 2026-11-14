#!/usr/bin/env bash
# Run one benchmark with every part on its own cores:
#   scripts/vm/run-pinned.sh e1|e2|e3|e4 [bench flags]     e.g.  scripts/vm/run-pinned.sh e1 -runs 5 -out docs/data/<date>-linux-vm/e1
# The stores start (or are recreated) with their cpusets, the API runs under api-pinned.sh, and bench itself,
# which is also the load generator and the checker, runs on its own cores.
# PIN=0 runs without any pinning (laptop smoke runs only; the result records that nothing was pinned).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck disable=SC1091
. scripts/vm/env.sh
cmd="${1:?usage: scripts/vm/run-pinned.sh e1|e2|e3|e4 [bench flags]}"
shift
docker compose up -d --wait
if [ "${PIN:-1}" = "1" ]; then
  command -v taskset >/dev/null || { echo "taskset not found; use PIN=0 only for a laptop smoke run" >&2; exit 1; }
  scripts/vm/verify-pinning.sh --stores-only
  exec taskset -c "$BENCH_CPUS" bin/bench "$cmd" -api scripts/vm/api-pinned.sh "$@"
fi
api=bin/api
[ -x bin/api.exe ] && api=bin/api.exe
exec bin/bench "$cmd" -api "$api" "$@"
