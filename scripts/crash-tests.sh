#!/usr/bin/env bash
# The real-kill scenarios of docs/crash-analysis.md: each crash row, CRASH_REPS times (default 5), against
# the real API binary. Needs scripts/up.sh first. Raw evidence, one JSON line per run, goes to
# $CRASH_RESULTS (default benchmark-scratch/crash-results.jsonl).
set -euo pipefail
cd "$(dirname "$0")/.."
export CRASH_REPS="${CRASH_REPS:-5}"
# An absolute path: the test process runs in the package directory.
export CRASH_RESULTS="${CRASH_RESULTS:-$(pwd)/benchmark-scratch/crash-results.jsonl}"
mkdir -p "$(dirname "$CRASH_RESULTS")"
: > "$CRASH_RESULTS"
go test ./internal/crashtest -count=1 -v -timeout 30m "$@"
runs=$(wc -l < "$CRASH_RESULTS"); kills=$(grep -c '"killed":true' "$CRASH_RESULTS")
echo "raw evidence: $CRASH_RESULTS ($runs runs, $kills real kills)"
# 11 scenarios (9 crash rows and 2 negative controls) times the repetitions: a shortfall means a run was skipped.
if [ "$runs" -ne $((11 * CRASH_REPS)) ]; then echo "WARNING: expected $((11 * CRASH_REPS)) runs"; exit 1; fi
