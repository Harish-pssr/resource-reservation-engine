#!/usr/bin/env bash
# Stop local infrastructure and delete its data.
set -euo pipefail
cd "$(dirname "$0")/.."
docker compose down -v
