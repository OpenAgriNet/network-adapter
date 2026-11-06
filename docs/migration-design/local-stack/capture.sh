#!/usr/bin/env bash
# Send the weather request to the V1 service once per scenario and keep the replies.
#
# What lands in golden/ is the baseline the V1 Adapter has to reproduce byte for
# byte. Run this against the old service first, then against the adapter, and
# diff the two directories.
#
#   ./capture.sh                     capture from the old service on :3000
#   ./capture.sh http://localhost:3001 golden-adapter    capture from the adapter

set -euo pipefail
cd "$(dirname "$0")"

TARGET="${1:-http://localhost:3000}"
OUT="${2:-golden}"
mkdir -p "$OUT"

# name  lat   lon    what it proves
SCENARIOS=(
  "happy         18.4  74.4   five complete days"
  "gaps          1.1   74.4   a missing day keeps the later day numbers"
  "late-only     2.2   74.4   no fcstday1 or fcstday2, so no answer"
  "provider-down 3.3   74.4   three failed attempts, still a 200"
  "zero-coords   0     0      guarded before the provider is called"
)

for row in "${SCENARIOS[@]}"; do
  read -r name lat lon _ <<<"$row"
  body=$(sed -e "s/__LAT__/$lat/" -e "s/__LON__/$lon/" requests/weather-mausamgram.json)
  code=$(curl -sS -o "$OUT/$name.json" -w '%{http_code}' \
    -X POST "$TARGET/mobility/search" \
    -H 'Content-Type: application/json' \
    --data "$body" || echo 000)
  python3 summarise.py "$name" "$code" "$OUT/$name.json"
done

echo
echo "replies written to $OUT/"
