#!/usr/bin/env bash
# Build the adapter and start the three processes this spike adds, then leave
# them running.
#
# The compose stack in the directory above supplies the other two: the legacy
# V1 service on :3000 and the mock provider on :9100. Bring that up first with
# `docker compose up -d` from there.
#
#   :9200  mock registry, and the host the mappings are fetched from
#   :3002  the V2 provider adapter, which calls Mausamgram
#   :3001  the V1 adapter, which calls the V2 adapter
#
# run.sh calls this, exercises the stack, and tears it down again. Call it
# directly when you want the stack to stay up -- to drive it from the API
# collection, say, or to edit a mapping and watch the next request pick it up.
#
# Usage: ./up.sh [ONIX_REPO]
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# Four levels up from docs/migration-design/local-stack/v1-adapter is the
# repo this spike lives in, which is the repo it builds. Overridable by
# argument, for running it against a checkout somewhere else.
ONIX="${1:-$(cd "$HERE/../../../.." && pwd)}"
WORK="$HERE/.run"

# The same credential the legacy service builds for itself, presented the same
# way: one ready-made header value rather than a username and a password, so
# the adapter sends the bytes V1 sends.
MAUSAMGRAM_AUTH="Basic $(printf 'bv-local:local-dev-key' | base64)"
export MAUSAMGRAM_AUTH

# The V2 half of the hop is served the mapping beckn-onix actually ships, out of
# the repo, rather than a copy kept here. A copy would let the two drift and the
# comparison would stop meaning anything.
ONIX_MAPPINGS="$ONIX/config/mappings"
export ONIX_MAPPINGS

"$HERE/down.sh" >/dev/null 2>&1 || true
mkdir -p "$WORK"

echo "building the adapter and the four plugins it loads"
( cd "$ONIX"
  go build -o "$WORK/adapter-bin" ./cmd/adapter/
  # Emptied first. Every .so in this directory is loaded, so one left behind by
  # an earlier build fails the whole process with "built with a different
  # version of package ...model" -- which names the stale plugin and not the
  # change that made it stale.
  rm -rf "$HERE/plugins"
  mkdir -p "$HERE/plugins"
  # router is the V1 adapter's forward mode: anything WeatherObservation did
  # not answer goes to the old service through it.
  for name in sunbirdRegistry jsonmapper WeatherObservation router; do
    go build -buildmode=plugin -o "$HERE/plugins/$name.so" \
      "./pkg/plugin/implementation/$name/cmd/plugin.go"
  done
)

python3 -u "$HERE/registry.py" > "$WORK/registry.log" 2>&1 &
echo $! >> "$WORK/pids"

# One binary, two configs. The V2 adapter is started first because the V1 one
# calls it, though neither waits on the other: both resolve their upstream per
# request, off the registry.
"$WORK/adapter-bin" -config "$HERE/v2-adapter.yaml" > "$WORK/v2-adapter.log" 2>&1 &
echo $! >> "$WORK/pids"
"$WORK/adapter-bin" -config "$HERE/adapter.yaml" > "$WORK/v1-adapter.log" 2>&1 &
echo $! >> "$WORK/pids"

for port in 3002 3001; do
  ready=
  for _ in $(seq 1 40); do
    if curl -fsS -m 1 -o /dev/null "http://localhost:$port/health" 2>/dev/null; then ready=1; break; fi
    sleep 0.5
  done
  if [ -z "$ready" ]; then
    echo "adapter on :$port did not come up; see $WORK/*.log" >&2
    exit 1
  fi
done

echo "up: registry :9200, V2 adapter :3002, V1 adapter :3001"
echo "logs in $WORK, stop with ./down.sh"
