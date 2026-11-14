#!/usr/bin/env bash
# e3: see "bench e3 -h". Extra arguments go to `bench e3`, e.g. scripts/bench-e3.sh -out docs/data/<date>-linux-vm
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build.sh
exec bin/bench e3 "$@"
