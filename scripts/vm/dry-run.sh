#!/usr/bin/env bash
# Every runner at a tiny size, each one timed. Its purpose is to catch setup mistakes and to measure how long the
# real session will take. Nothing it prints is ever quoted as a result (tiny sizes, fast payment profile).
#   scripts/vm/dry-run.sh            on the benchmark machine (pinned)
#   PIN=0 scripts/vm/dry-run.sh      a laptop smoke run (not pinned; the records say so)
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck disable=SC1091
. scripts/vm/env.sh
out=benchmark-scratch/dry-run
rm -rf "$out"
mkdir -p "$out"

# A laptop smoke run may be on battery (the runner refuses that for real runs); its results are never quoted anyway.
# The pinned run on the benchmark machine never gets this flag.
smoke=()
if [ "${PIN:-1}" != "1" ]; then smoke=(-allow-battery); fi
failed=0
summary=""
run() { # name, then the command
  local name="$1" t0 t1 status
  shift
  printf '%-14s ' "$name"
  t0=$(date +%s)
  if "$@" >"$out/$name.log" 2>&1; then status=ok; else status="FAILED (see $out/$name.log)"; failed=1; fi
  t1=$(date +%s)
  printf '%4ss  %s\n' "$((t1 - t0))" "$status"
  summary="$summary$name $((t1 - t0))s $status"$'\n'
}

docker compose up -d --wait >/dev/null 2>&1 || { echo "the stores did not start" >&2; exit 1; }
if [ "${PIN:-1}" = "1" ]; then run verify-pinning scripts/vm/verify-pinning.sh --stores-only; fi

run e1 scripts/vm/run-pinned.sh e1 -runs 1 -buyers 600 -items 200 -arrival 3s -profile fast \
  -strategies naive-unprotected,locking,fastpath -out "$out/e1" "${smoke[@]}"
run e2 scripts/vm/run-pinned.sh e2 -strategies locking -skews 1.1 -profile fast -max-rate 150 \
  -ramp-step 5s -confirm-step 5s -warmup 2s -out "$out/e2" "${smoke[@]}"
run e3 scripts/vm/run-pinned.sh e3 -strategy locking -capacity 150 -factors 2 -duration 5s -profile fast -out "$out/e3" "${smoke[@]}"
run e4 scripts/vm/run-pinned.sh e4 -plans api-kills,redis-restart -runs 1 -buyers 300 -arrival 8s \
  -store-window 15s -profile fast -out "$out/e4" "${smoke[@]}"

if [ "${PIN:-1}" = "1" ]; then
  steal="$(grep -h -o '"cpu_steal_pct": *[0-9.eE+-]*' -r "$out" 2>/dev/null | head -n1 || true)"
  echo "environment record: ${steal:-cpu_steal_pct not recorded}"
fi
echo
echo "Dry-run timings (never quoted):"
printf '%s' "$summary"
if [ "$failed" -ne 0 ]; then echo "At least one runner failed: read its log before going on." >&2; fi
exit "$failed"
