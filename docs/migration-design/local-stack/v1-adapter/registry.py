"""A registry and a mapping host, small enough to read, for the V1 adapter spike.

The adapter needs two things it cannot be configured with directly: a registry
answering what to call, and an HTTP address to fetch the mapping from. Both are
here, in one process, because the spike is about the adapter and neither of
these is the thing under test.

The registry speaks the two searches sunbirdRegistry makes:

  POST /api/v1/ProviderSchema/search   {"filters": {"bindingKey": {"eq": ...}}}
  POST /api/v1/Participant/search      {"filters": {"participantId": {"eq": ...}}}

answering a bare JSON array, which is one of the two shapes the client accepts.

  GET  /mappings/<name>.yaml           this spike's own mapping, from disk
  GET  /onix-mappings/<path>.yaml      the shipped V2 mapping, from the
                                       beckn-onix tree, unmodified
"""

import json
import os
import pathlib
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(os.environ.get("PORT", "9200"))
UPSTREAM = os.environ.get("UPSTREAM", "http://localhost:9100")
MAPPING_HOST = os.environ.get("MAPPING_HOST", f"http://localhost:{PORT}")
HERE = pathlib.Path(__file__).parent

# Two bindings, because the chain is two hops. A V1 search reaches the V1
# adapter, which asks the V2 adapter in the network's own payloads, which asks
# the provider. Each hop is an ordinary provider-step call; what differs is only
# who the "provider" is.
#
#   V1 caller --search--> V1 adapter --select--> V2 adapter --GET--> Mausamgram
#
# The left half of a binding key is the participant. V1 payloads name no
# participant at all, so the capability's own code stands in for one on the
# first hop. That is a finding, not a convenience -- see README.md.
V1_BINDING = "WFC|Weather-Forecast-Mausamgram"
V2_BINDING = "mausamgram|openagrinet:WeatherObservation"

# Where the V2 adapter listens. The V1 adapter calls it exactly as it would call
# any other upstream: a base URL from a participant record, a path and a method
# from an action plan.
V2_ADAPTER = os.environ.get("V2_ADAPTER", "http://localhost:3002")

# The shipped V2 mapping, served from the beckn-onix tree rather than copied
# here. A copy would drift, and the whole question this stack answers is what
# the REAL V2 mapping leaves out.
# Four levels up from docs/migration-design/local-stack/v1-adapter is the repo
# this spike lives in, so the shipped mappings are found without being told
# where they are. up.sh sets the variable anyway; the default is for running
# this file on its own.
ONIX_MAPPINGS = pathlib.Path(
    os.environ.get("ONIX_MAPPINGS", HERE.parents[3] / "config" / "mappings")
)

BINDINGS = [
    {
        "bindingKey": V1_BINDING,
        "participantId": "WFC",
        "capabilityCode": "Weather-Forecast-Mausamgram",
        "status": "ACTIVE",
        "actions": [
            {
                # V1's own word. The V1 adapter reads it off context.action,
                # which the caller sends as "search".
                "action": "search",
                "method": "POST",
                "path": "/select",
                "mappings": f"{MAPPING_HOST}/mappings/weather-forecast-mausamgram.search.yaml",
                "timeoutMs": 30000,
                # V1 tries three times; the registry counts retries AFTER the
                # first attempt, so two.
                "retryMax": 2,
                "status": "ACTIVE",
            }
        ],
    },
    {
        "bindingKey": V2_BINDING,
        "participantId": "mausamgram",
        "capabilityCode": "openagrinet:WeatherObservation",
        "status": "ACTIVE",
        "actions": [
            {
                "action": "select",
                "method": "GET",
                "path": "/get-daily",
                # The file the V2 network actually publishes, unmodified.
                "mappings": f"{MAPPING_HOST}/onix-mappings/mausamgram/weather-observation.select.yaml",
                "timeoutMs": 30000,
                "retryMax": 2,
                "status": "ACTIVE",
            }
        ],
    },
]

PARTICIPANTS = [
    # The V1 adapter's "provider" is the V2 adapter.
    {
        "participantId": "WFC",
        "type": "BPP",
        "role": ["BPP"],
        "status": "ACTIVE",
        "baseUrl": V2_ADAPTER,
        "keys": [],
    },
    # The V2 adapter's provider is the real upstream.
    {
        "participantId": "mausamgram",
        "type": "BPP",
        "role": ["BPP"],
        "status": "ACTIVE",
        "baseUrl": UPSTREAM,
        "keys": [],
    },
]


def matches(record, filters):
    for field, condition in (filters or {}).items():
        wanted = condition.get("eq")
        if wanted is not None and record.get(field) != wanted:
            return False
    return True


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print("registry %s" % (fmt % args), flush=True)

    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            return self._json(200, {"status": "ok"})
        # Two mapping roots. /mappings is this spike's own V1 file; the V2 one
        # is served out of the beckn-onix tree so the chain runs against what
        # that network really publishes.
        if self.path.startswith("/mappings/") or self.path.startswith("/onix-mappings/"):
            if self.path.startswith("/onix-mappings/"):
                relative = self.path[len("/onix-mappings/"):]
                root = ONIX_MAPPINGS
            else:
                relative = self.path[len("/mappings/"):]
                root = HERE / "mappings"
            # The path comes off the wire, so it is resolved and then checked to
            # be inside its root rather than trusted.
            path = (root / relative).resolve()
            if not str(path).startswith(str(root.resolve())) or not path.is_file():
                return self._json(404, {"error": f"no mapping {relative}"})
            body = path.read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "application/yaml")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            return self.wfile.write(body)
        return self._json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        try:
            request = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return self._json(400, {"error": "body is not JSON"})
        filters = request.get("filters") or {}

        if self.path.endswith("/ProviderSchema/search"):
            return self._json(200, [b for b in BINDINGS if matches(b, filters)])
        if self.path.endswith("/Participant/search"):
            return self._json(200, [p for p in PARTICIPANTS if matches(p, filters)])
        return self._json(404, {"error": "not found"})


if __name__ == "__main__":
    print(f"registry on :{PORT}: {V1_BINDING} -> {V2_ADAPTER}, "
          f"{V2_BINDING} -> {UPSTREAM}", flush=True)
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
