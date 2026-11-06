#!/usr/bin/env bash
# Stop whatever up.sh started. Safe to run when nothing is running.
set -euo pipefail
WORK="$(cd "$(dirname "$0")" && pwd)/.run"
[ -f "$WORK/pids" ] || exit 0
while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$WORK/pids"
rm -f "$WORK/pids"
