#!/usr/bin/env bash
# Run the API for the console and restart it whenever it exits, so the crash demos can show recovery.
# Fault injection is ON here (it is a development tool): the admin token is printed. Needs scripts/up.sh.
# Settings can be overridden from the environment, for example STRATEGY=fastpath ROOM_RATE=50 scripts/dev-api.sh
set -uo pipefail
cd "$(dirname "$0")/.."
go build -o bin/api ./cmd/api || exit 1
export FAULTS_ENABLED="${FAULTS_ENABLED:-true}" ADMIN_TOKEN="${ADMIN_TOKEN:-dev-token}"
export PAYMENT_PROFILE="${PAYMENT_PROFILE:-fast}" HOLD_TTL="${HOLD_TTL:-60s}" HTTP_ADDR="${HTTP_ADDR:-:8080}"
echo "console: http://localhost:8080   admin token: $ADMIN_TOKEN   (Ctrl-C stops)"
while true; do
  bin/api
  echo "the API exited with status $?; restarting in 1 s"
  sleep 1
done
