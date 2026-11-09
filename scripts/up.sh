#!/usr/bin/env bash
# Starts everything: Postgres and Redis (Docker), then the API with the console, and a first event.
# Afterwards open http://localhost:8080 . scripts/down.sh closes it all again.
#
# The server runs in the background and is restarted if it exits (so the crash demos can show recovery).
# Its log is logs/api.log. Fault injection is ON in this development setup: the admin token for the
# Failures and Checker pages is printed at the end.
#
# To change how the server runs, set variables first, for example:
#   STRATEGY=locking ROOM_RATE=0 scripts/up.sh
set -euo pipefail
cd "$(dirname "$0")/.."

# ---- internal mode: build the API and run it, restarting it whenever it exits ----
# (--run-api-loop is how up runs the server in the background. Do not pass it yourself.)
if [ "${1:-}" = "--run-api-loop" ]; then
  go build -o bin/api ./cmd/api || exit 1
  export FAULTS_ENABLED="${FAULTS_ENABLED:-true}" ADMIN_TOKEN="${ADMIN_TOKEN:-dev-token}"
  export PAYMENT_PROFILE="${PAYMENT_PROFILE:-fast}" HOLD_TTL="${HOLD_TTL:-60s}" HTTP_ADDR="${HTTP_ADDR:-:8080}"
  # By default show the whole system: the Redis fast path and the waiting room (admits 30 buyers a second).
  export STRATEGY="${STRATEGY:-fastpath}" ROOM_RATE="${ROOM_RATE:-30}"
  set +e
  while true; do
    bin/api
    echo "the API exited with status $?; restarting in 1 s"
    sleep 1
  done
fi

# 1. The stores.
command -v docker >/dev/null 2>&1 || { echo "Docker is not installed."; exit 1; }
docker info >/dev/null 2>&1 || { echo "Docker is not running: start it, then run this again."; exit 1; }
docker compose up -d --wait
echo "postgres: localhost:55432   redis: localhost:56379"

# 2. The API and the console.
addr="${HTTP_ADDR:-:8080}"
port="${addr##*:}"
base="http://127.0.0.1:$port"
healthy() { curl -fs -m 2 "$base/healthz" >/dev/null 2>&1; }

if healthy; then
  echo "the API is already running at $base"
else
  mkdir -p logs
  nohup "$(pwd)/scripts/up.sh" --run-api-loop > logs/api.log 2>&1 &
  echo "starting the API (the first start builds it, which can take a minute)..."
  for _ in $(seq 1 120); do healthy && break; sleep 1; done
  healthy || { echo "the API did not come up: see logs/api.log"; tail -8 logs/api.log; exit 1; }
fi

# 3. A first event, so the seat map is not empty.
if ! curl -fs "$base/events" | grep -q '"id"'; then
  curl -fs -X POST "$base/events" -d '{"name":"Opening night","items":60}' >/dev/null
  echo "created a first event: Opening night, 60 seats"
fi

echo
echo "everything is up:  http://localhost:$port"
echo "admin token (Failures and Checker pages): ${ADMIN_TOKEN:-dev-token}"
echo "close it all with: scripts/down.sh"
