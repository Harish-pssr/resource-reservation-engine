#!/usr/bin/env bash
# E1, the oversell test: every strategy, repeated runs. Extra arguments go to `bench e1`,
# e.g. scripts/bench-e1.sh -runs 5 -out docs/data/2026-11-20-linux-vm
set -euo pipefail
cd "$(dirname "$0")/.."
scripts/build.sh
exec bin/bench e1 "$@"
