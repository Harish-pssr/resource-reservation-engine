#!/usr/bin/env bash
# Run all tests. Integration tests need the infrastructure from scripts/up.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
go vet ./...
go test ./... "$@"
