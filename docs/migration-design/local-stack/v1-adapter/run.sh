#!/usr/bin/env bash
# Stand the two adapters up beside the legacy service and compare them.
#
# Assumes the compose stack from the directory above is already running:
# bv-beckn on :3000 and mock-mausamgram on :9100. up.sh adds the registry on
# :9200, the V2 provider adapter on :3002 and the V1 adapter on :3001. This
# sends the same five requests to :3000 and :3001 and reports every field that
# differs.
#
#   :3000  the legacy V1 service, calling the provider directly
#   :3001  the V1 adapter, whose answer has been through V2 and back
#
# Every difference the comparison prints is a fidelity loss attributable to the
# V2 hop, which is the measurement this spike exists to take.
#
# Usage: ./run.sh [ONIX_REPO]
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
STACK="$(dirname "$HERE")"
WORK="$HERE/.run"

mkdir -p "$WORK/v1" "$WORK/adapter" "$WORK/req"

trap '"$HERE/down.sh"' EXIT
"$HERE/up.sh" "$@"

# The five scenarios, picked by latitude: the provider reads it and answers
# with the fixture it names. See ../mock-mausamgram/server.py.
printf '%s\n' \
  "happy         18.4 74.4" \
  "gaps          1.1  74.4" \
  "late-only     2.2  74.4" \
  "provider-down 3.3  74.4" \
  "zero-coords   0    0" |
while read -r name lat lon; do
  # The payload is BharatVistaar's, unaltered but for the coordinates. No bpp_id
  # is added: a V1 caller names no receiver, and the V1 adapter is configured to
  # answer an unaddressed request rather than refuse it.
  python3 - "$STACK/requests/weather-mausamgram.json" "$WORK/req/$name.json" "$lat" "$lon" <<'PY'
import json, sys
source, target, lat, lon = sys.argv[1:5]
payload = json.load(open(source))
payload["message"]["intent"]["fulfillment"]["stops"][0]["location"] = {"lat": lat, "lon": lon}
json.dump(payload, open(target, "w"))
PY
  legacy=$(curl -s -m 60 -o "$WORK/v1/$name.json" -w "%{http_code}" \
    -X POST http://localhost:3000/mobility/search \
    -H 'Content-Type: application/json' -d @"$WORK/req/$name.json")
  adapter=$(curl -s -m 60 -o "$WORK/adapter/$name.json" -w "%{http_code}" \
    -X POST http://localhost:3001/mobility/search \
    -H 'Content-Type: application/json' -d @"$WORK/req/$name.json")
  printf "%-14s legacy=%s adapter=%s\n" "$name" "$legacy" "$adapter"
done

echo
python3 "$STACK/compare.py" "$WORK/v1" "$WORK/adapter"
