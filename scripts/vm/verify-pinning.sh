#!/usr/bin/env bash
# Prove the core split from the running processes, not from the scripts' intent: read the cores each process
# is actually allowed to use and compare them with the plan in pinning.env. Exit 1 on any difference.
#   scripts/vm/verify-pinning.sh                 stores, and the API and bench if they are running
#   scripts/vm/verify-pinning.sh --stores-only   only the two containers (used before a run starts)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck disable=SC1091
. scripts/vm/env.sh

allowed() { awk '/^Cpus_allowed_list:/ {print $2}' "/proc/$1/status"; }
bad=0
check() { # name, want, pid
  local got
  got="$(allowed "$3")"
  if [ "$got" = "$2" ]; then printf '  ok    %-9s cores %-6s (pid %s)\n' "$1" "$got" "$3"; else printf '  WRONG %-9s cores %s, plan says %s (pid %s)\n' "$1" "$got" "$2" "$3"; bad=1; fi
}
container_pid() { docker inspect -f '{{.State.Pid}}' "$(docker compose ps -q "$1")"; }

echo "core plan: $BENCH_PINNING"
check postgres "$PG_CPUS" "$(container_pid postgres)"
check redis "$REDIS_CPUS" "$(container_pid redis)"
if [ "${1:-}" != "--stores-only" ]; then
  for pair in "api:$API_CPUS" "bench:$BENCH_CPUS"; do
    name="${pair%%:*}"
    pid="$(pgrep -x "$name" | head -n1 || true)"
    if [ -n "$pid" ]; then check "$name" "${pair#*:}" "$pid"; else echo "  --    $name is not running"; fi
  done
fi
exit "$bad"
