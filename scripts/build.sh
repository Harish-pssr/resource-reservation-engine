#!/usr/bin/env bash
# Build the binaries into bin/.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p bin
for c in api loadgen bench; do go build -o "bin/$c" "./cmd/$c"; done
echo "built bin/api bin/loadgen bin/bench"
