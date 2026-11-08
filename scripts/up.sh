#!/usr/bin/env bash
# Start local Postgres and Redis and wait until both are healthy.
set -euo pipefail
cd "$(dirname "$0")/.."
docker compose up -d --wait
echo "postgres: localhost:55432   redis: localhost:56379"
