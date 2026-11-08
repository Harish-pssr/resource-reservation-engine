#!/usr/bin/env bash
# e2: see "bench e2 -h". Extra arguments go to `bench e2`, e.g. scripts/bench-e2.sh -out docs/data/<date>-linux-vm
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build.sh
exec bin/bench e2 "$@"
