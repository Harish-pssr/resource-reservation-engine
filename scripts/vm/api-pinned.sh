#!/usr/bin/env bash
# The API binary under the API's cores. The benchmark starts this in place of bin/api (-api scripts/vm/api-pinned.sh);
# `exec` keeps the process id, so the benchmark's SIGKILLs and crash-hook checks still hit the real API process.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$here/env.sh"
exec taskset -c "$API_CPUS" "$ROOT/bin/api" "$@"
