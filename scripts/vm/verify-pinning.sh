#!/usr/bin/env bash
# Prove the core split from the running processes, not from the scripts' intent: read the cores each process
# is actually allowed to use and compare them with the plan in pinning.env. It also refuses a plan in which two
# parts share a PHYSICAL core (the two threads of one core count as one), which is what pinning is for.
# Exit 1 on any difference.
#   scripts/vm/verify-pinning.sh                 stores, and the API and bench if they are running
#   scripts/vm/verify-pinning.sh --stores-only   only the two containers (used before a run starts)
#   scripts/vm/verify-pinning.sh --plan-only     only check the plan itself (no containers needed)
# CPU_CORE_MAP_FILE=<file> replaces `lscpu -e=CPU,CORE` (for testing the plan check away from the machine).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck disable=SC1091
. scripts/vm/env.sh

bad=0

# "0-2,8-10" -> "0 1 2 8 9 10"
expand() {
  local part lo hi out=""
  IFS=',' read -ra parts <<<"$1"
  for part in "${parts[@]}"; do
    if [[ "$part" == *-* ]]; then lo="${part%-*}"; hi="${part#*-}"; else lo="$part"; hi="$part"; fi
    for ((c = lo; c <= hi; c++)); do out="$out $c"; done
  done
  echo "$out"
}

core_map() {
  if [ -n "${CPU_CORE_MAP_FILE:-}" ]; then cat "$CPU_CORE_MAP_FILE"; else lscpu -e=CPU,CORE | tail -n +2; fi
}

# the physical cores a CPU list touches, e.g. "0 1 2"
cores_of() {
  local cpu map
  map="$(core_map)"
  for cpu in $(expand "$1"); do awk -v c="$cpu" '$1 == c {print $2}' <<<"$map"; done | sort -un | tr '\n' ' '
}

check_plan() {
  local names=(postgres api bench redis) lists=("$PG_CPUS" "$API_CPUS" "$BENCH_CPUS" "$REDIS_CPUS") i j ci cj shared
  echo "core plan: $BENCH_PINNING"
  for i in "${!names[@]}"; do
    ci="$(cores_of "${lists[$i]}")"
    [ -n "$ci" ] || { echo "  WRONG ${names[$i]}: cpus ${lists[$i]} do not exist on this machine"; bad=1; continue; }
    printf '  plan  %-9s cpus %-12s physical cores: %s\n' "${names[$i]}" "${lists[$i]}" "$ci"
    for ((j = i + 1; j < ${#names[@]}; j++)); do
      cj="$(cores_of "${lists[$j]}")"
      shared="$(comm -12 <(tr ' ' '\n' <<<"$ci" | sort) <(tr ' ' '\n' <<<"$cj" | sort) | tr '\n' ' ')"
      if [ -n "${shared// /}" ]; then echo "  WRONG ${names[$i]} and ${names[$j]} share physical core(s): $shared"; bad=1; fi
    done
  done
}

allowed() { awk '/^Cpus_allowed_list:/ {print $2}' "/proc/$1/status"; }
check() { # name, want, pid
  local got
  got="$(allowed "$3")"
  if [ "$got" = "$2" ]; then printf '  ok    %-9s cores %-12s (pid %s)\n' "$1" "$got" "$3"; else printf '  WRONG %-9s cores %s, plan says %s (pid %s)\n' "$1" "$got" "$2" "$3"; bad=1; fi
}
container_pid() { docker inspect -f '{{.State.Pid}}' "$(docker compose ps -q "$1")"; }

check_plan
if [ "${1:-}" != "--plan-only" ]; then
  check postgres "$PG_CPUS" "$(container_pid postgres)"
  check redis "$REDIS_CPUS" "$(container_pid redis)"
  if [ "${1:-}" != "--stores-only" ]; then
    for pair in "api:$API_CPUS" "bench:$BENCH_CPUS"; do
      name="${pair%%:*}"
      pid="$(pgrep -x "$name" | head -n1 || true)"
      if [ -n "$pid" ]; then check "$name" "${pair#*:}" "$pid"; else echo "  --    $name is not running"; fi
    done
  fi
fi
exit "$bad"
