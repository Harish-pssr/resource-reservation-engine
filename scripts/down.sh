#!/usr/bin/env bash
# Closes everything this project starts: the restart loops, the API servers, any benchmark or load
# processes, and the Postgres and Redis containers (when Docker is used).
#
#   scripts/down.sh           stops it all and KEEPS the database and Redis data (scripts/up.sh brings it back)
#   scripts/down.sh --clean   also removes the containers and DELETES their data
set -uo pipefail
cd "$(dirname "$0")/.."
clean=false; [ "${1:-}" = "--clean" ] && clean=true

# 1. The restart loops first, otherwise they would start the servers again as soon as we stop them.
#    pgrep -f also matches this script's own shell, so its own pid is excluded.
loops=0
for pat in 'scripts/dev-api' 'dev-ui' 'run-api'; do
  for pid in $(pgrep -f "$pat" 2>/dev/null | grep -vx "$$"); do kill "$pid" 2>/dev/null && loops=$((loops + 1)); done
done
echo "restart loops stopped: $loops"

# 2. The servers and tools themselves.
killed=0
for name in api api-ui bench loadgen; do
  for pid in $(pgrep -x "$name" 2>/dev/null); do kill "$pid" 2>/dev/null && killed=$((killed + 1)); done
done
echo "processes stopped: $killed (api, api-ui, bench, loadgen)"

# 3. The stores.
if command -v docker >/dev/null 2>&1; then
  if $clean; then docker compose down -v; echo "containers removed and data deleted"
  else docker compose stop; echo "containers stopped; data kept (scripts/up.sh starts them again)"; fi
else
  echo "docker is not installed: nothing to stop there"
fi

# 4. Check, so the script says plainly whether anything is left.
sleep 1
if pgrep -x api >/dev/null || pgrep -x api-ui >/dev/null || pgrep -x bench >/dev/null || pgrep -x loadgen >/dev/null; then
  echo "WARNING: some processes are still running"; exit 1
fi
echo "everything is closed"
