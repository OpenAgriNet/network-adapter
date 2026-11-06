"""Generate the Postman collection from the same payload the harness uses.

Written rather than hand-authored so the collection cannot drift from
../requests/weather-mausamgram.json, and so the five scenarios are described
once. Re-run it after changing the request payload:

    python3 build.py
"""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))
V1_PAYLOAD = json.load(open(os.path.join(HERE, "..", "requests", "weather-mausamgram.json")))

# lat, lon, what the provider does, and what the V1 contract says comes back.
SCENARIOS = [
    ("happy", "18.4", "74.4", "all five forecast days present",
     "five day groups, Today through Day_5"),
    ("gaps", "1.1", "74.4", "day 2 missing, and some readings null, zero or empty",
     "four day groups numbered by slot: Today, Day_3, Day_4, Day_5"),
    ("late-only", "2.2", "74.4", "only days 3, 4 and 5 present",
     "an empty catalog: V1 will not lead with Day_3"),
    ("provider-down", "3.3", "74.4", "HTTP 500 on every attempt",
     "an empty catalog, because a V1 caller is never shown an error"),
    ("zero-coords", "0", "0", "never called, the coordinates fail the guard first",
     "an empty catalog, decided before anyone is called"),
]

EMPTY = ("late-only", "provider-down", "zero-coords")


def v1_body(lat, lon):
    payload = json.loads(json.dumps(V1_PAYLOAD))
    payload["message"]["intent"]["fulfillment"]["stops"][0]["location"] = {"lat": lat, "lon": lon}
    return json.dumps(payload, indent=2)


def v2_body(lat, lon):
    """The select the V1 adapter emits for these coordinates.

    Here so the middle of the hop can be driven on its own. It is the output of
    the request half of the V1 mapping, and a V2 caller would send exactly this.
    """
    return json.dumps({
        "context": {
            "version": "2.0.0", "action": "select", "networkId": "openagrinet.local",
            "receiverId": "oan.local",
            "transactionId": "11111111-1111-1111-1111-111111111111",
            "messageId": "22222222-2222-2222-2222-222222222222",
            "timestamp": "2026-01-01T00:00:00Z",
        },
        "message": {"contract": {"commitments": [{
            "status": {"descriptor": {"code": "ACTIVE", "name": "active"}},
            "offer": {
                "id": "offer:mausamgram:open-data",
                "resourceIds": ["res:mausamgram:point-forecast"],
                "provider": {"id": "mausamgram", "descriptor": {
                    "code": "IMD-NWP-01", "name": "IMD Mausamgram NWP"}},
            },
            "resources": [{
                "id": "res:mausamgram:point-forecast",
                "quantity": {"count": 1},
                "resourceAttributes": {
                    "@context": "https://schemas.openagrinet.global/schema/WeatherObservation/v0.1/context.jsonld",
                    "@type": "openagrinet:WeatherObservation",
                    "informationMode": "OnDemand",
                    "subjectCategories": ["Weather"],
                    "location": {"geo": {"type": "Point",
                                         "coordinates": [float(lon), float(lat)]}},
                },
            }],
        }]}},
    }, indent=2)


def test(*lines):
    return {"listen": "test", "script": {"type": "text/javascript", "exec": list(lines)}}


def post(name, url, body, description, tests=None):
    item = {
        "name": name,
        "request": {
            "method": "POST",
            "header": [{"key": "Content-Type", "value": "application/json"}],
            "body": {"mode": "raw", "raw": body,
                     "options": {"raw": {"language": "json"}}},
            "url": {"raw": url, "host": [url]},
            "description": description,
        },
        "response": [],
    }
    if tests:
        item["event"] = [tests]
    return item


def get(name, url, description, tests=None):
    item = {
        "name": name,
        "request": {"method": "GET", "header": [],
                    "url": {"raw": url, "host": [url]}, "description": description},
        "response": [],
    }
    if tests:
        item["event"] = [tests]
    return item


# --- the folders -------------------------------------------------------------

v1_adapter_tests = lambda empty: test(
    "// V1 is a Nest service and answers 201 on POST. The adapter is configured",
    "// to match, because a status code is part of the contract too.",
    "pm.test('201, the status V1 sends', () => pm.response.to.have.status(201));",
    "",
    "const providers = pm.response.json().message.catalog.providers;",
    *(["pm.test('an empty catalog, not an error', () => pm.expect(providers).to.be.empty);"]
      if empty else
      ["pm.test('a forecast came back', () => pm.expect(providers).to.not.be.empty);",
       "const tags = providers[0].items[0].tags;",
       "pm.test('the first day is labelled Today', () =>",
       "  pm.expect(tags[0].descriptor.code).to.eql('Today'));",
       "pm.test('every day carries all ten readings', () =>",
       "  tags.forEach(t => pm.expect(t.list).to.have.lengthOf(10)));"]),
)

the_point = {
    "name": "1. V1 in, V1 out",
    "description":
        "A request exactly as BharatVistaar sends it today, answered in the shape\n"
        "BharatVistaar expects. Between the two, the request left as a V2 select and\n"
        "the V2 answer was mapped back. Nothing in either payload here is V2.\n\n"
        "Send these first. They are the whole claim.",
    "item": [
        post(f"{name}  ({does})", "{{v1Adapter}}/mobility/search", v1_body(lat, lon),
             f"Provider behaviour: {does}.\n\nExpected: {expects}.",
             v1_adapter_tests(name in EMPTY))
        for name, lat, lon, does, expects in SCENARIOS
    ],
}

alongside = {
    "name": "2. The same request to the legacy service",
    "description":
        "The identical payload against the V1 service running in Docker, which calls\n"
        "the provider directly and has never heard of V2. Run a request here and the\n"
        "matching one above, and diff the two bodies: that is the measurement.\n\n"
        "../v1-adapter/run.sh does this for all five and reports every field that\n"
        "differs, which is faster than diffing by eye.",
    "item": [
        post(f"{name}  ({does})", "{{legacy}}/mobility/search", v1_body(lat, lon),
             f"The contract. Whatever this returns is what the adapter has to return.",
             test("pm.test('201', () => pm.response.to.have.status(201));"))
        for name, lat, lon, does, expects in SCENARIOS
    ],
}

inside = {
    "name": "3. Inside the hop",
    "description":
        "The two legs the V1 adapter makes on your behalf, drivable on their own.\n\n"
        "The select is what leaves the V1 adapter; the GET is what leaves the V2 one.\n"
        "Run the select and compare its answer to the V1 body above and you can see\n"
        "exactly what the V2 representation keeps and what it drops.",
    "item": [
        post("the V2 select the V1 adapter sends", "{{v2Adapter}}/select",
             v2_body("18.4", "74.4"),
             "What the request half of the V1 mapping produces. A V2 caller sends\n"
             "exactly this; the V1 adapter is not a privileged client.",
             test(
                 "pm.test('200', () => pm.response.to.have.status(200));",
                 "const res = pm.response.json().message.contract.commitments[0].resources;",
                 "pm.test('one resource per forecast day', () =>",
                 "  pm.expect(res.length).to.be.above(0));",
                 "// Every day is a separate resource with its own validity window. The",
                 "// day NUMBER is not on the wire: the V1 mapping recovers it from the",
                 "// date, which works because fcstday1 is today for this provider.",
                 "pm.test('each day carries a validity window and no day number', () =>",
                 "  res.forEach(r => {",
                 "    pm.expect(r.resourceAttributes.validity.startsAt).to.be.a('string');",
                 "    pm.expect(JSON.stringify(r)).to.not.include('Day_');",
                 "  }));",
             )),
        get("the provider GET the V2 adapter sends",
            "{{provider}}/get-daily?lat=18.4&lon=74.4",
            "Mausamgram's own shape, before anything maps it. Needs the Basic\n"
            "credential, which is set on this folder and is fake.\n\n"
            "Change lat to 1.1, 2.2 or 3.3 to pick a different scenario.",
            test(
                "pm.test('200', () => pm.response.to.have.status(200));",
                "// cloud_message is the field with nowhere to go in V2. V1 renders it",
                "// as Weather Condition when there is no warning; the V2 parameter",
                "// enum has no member that holds prose, so it is dropped and the V1",
                "// adapter can only say N/A. It is the one real loss in the five",
                "// scenarios, and you can see it originate here.",
                "pm.test('the provider sends a prose cloud summary', () =>",
                "  pm.expect(pm.response.json().fcstday1).to.have.property('cloud_message'));",
            )),
    ],
    "auth": {"type": "basic", "basic": [
        {"key": "username", "value": "bv-local"},
        {"key": "password", "value": "local-dev-key"},
    ]},
}

def lookup(entity, field, value):
    return json.dumps({"filters": {field: {"eq": value}}}, indent=2)


registry = {
    "name": "4. What the adapters read from the registry",
    "description":
        "Every field the adapters look up, and nothing else. Useful for seeing how\n"
        "little a row has to carry for a capability to be reachable, and for seeing\n"
        "that the two hops are the same mechanism one level apart.\n\n"
        "These are POSTs with a filter body, which is the shape sunbirdRegistry sends.",
    "item": [
        post("the V2 adapter, as the V1 adapter sees it",
             "{{registry}}/api/v1/Participant/search",
             lookup("Participant", "participantId", "WFC"),
             "The V1 adapter resolves its upstream here. baseUrl in this row is what\n"
             "makes the V2 adapter reachable, so the hop is configuration and not code.\n\n"
             "participantId is WFC, the capability's own code, because a V1 payload\n"
             "names no participant for the left half of a binding key to come from.",
             test("pm.test('one row', () => pm.expect(pm.response.json()).to.have.lengthOf(1));",
                  "pm.test('points at the V2 adapter', () =>",
                  "  pm.expect(pm.response.json()[0].baseUrl).to.include('3002'));")),
        post("Mausamgram, as the V2 adapter sees it",
             "{{registry}}/api/v1/Participant/search",
             lookup("Participant", "participantId", "mausamgram"),
             "The same lookup one level down, and this one has a real participant id\n"
             "because a V2 payload carries one.",
             test("pm.test('points at the provider', () =>",
                  "  pm.expect(pm.response.json()[0].baseUrl).to.include('9100'));")),
        post("the V1 adapter's action plan",
             "{{registry}}/api/v1/ProviderSchema/search",
             lookup("ProviderSchema", "bindingKey", "WFC|Weather-Forecast-Mausamgram"),
             "Method, path and mapping URL for the V1 hop. Note the action is V1's own\n"
             "word, search, and the path it maps onto is /select.",
             test("const a = pm.response.json()[0].actions[0];",
                  "pm.test('V1 search becomes a V2 select', () => {",
                  "  pm.expect(a.action).to.eql('search');",
                  "  pm.expect(a.path).to.eql('/select');",
                  "});")),
        post("the Mausamgram action plan",
             "{{registry}}/api/v1/ProviderSchema/search",
             lookup("ProviderSchema", "bindingKey", "mausamgram|openagrinet:WeatherObservation"),
             "The second hop's plan. Changing the upstream means changing this row,\n"
             "not the adapter.",
             test("const a = pm.response.json()[0].actions[0];",
                  "pm.test('a select becomes a plain GET', () => {",
                  "  pm.expect(a.method).to.eql('GET');",
                  "  pm.expect(a.path).to.eql('/get-daily');",
                  "});")),
    ],
}

health = {
    "name": "0. Is everything up",
    "description":
        "Run these first if anything below misbehaves. The adapters are started by\n"
        "../v1-adapter/up.sh and the other two by docker compose, so a failure here\n"
        "tells you which of the two you forgot.",
    "item": [
        get("V1 adapter", "{{v1Adapter}}/health", "Started by ../v1-adapter/up.sh.",
            test("pm.test('up', () => pm.response.to.have.status(200));")),
        get("V2 adapter", "{{v2Adapter}}/health", "Started by ../v1-adapter/up.sh.",
            test("pm.test('up', () => pm.response.to.have.status(200));")),
        get("mock registry", "{{registry}}/healthz", "Started by ../v1-adapter/up.sh.",
            test("pm.test('up', () => pm.response.to.have.status(200));")),
        get("mock Mausamgram", "{{provider}}/healthz", "Started by docker compose.",
            test("pm.test('up', () => pm.response.to.have.status(200));")),
    ],
}

collection = {
    "info": {
        "name": "OpenAgriNet - V1 on the V2 network",
        "description":
            "Drive the local stack that answers a Beckn V1 request out of the V2\n"
            "network, with BharatVistaar unchanged.\n\n"
            "    V1 caller --search--> V1 adapter --select--> V2 adapter --GET--> Mausamgram\n"
            "                :3001                   :3002                 :9100\n\n"
            "Both adapters are the same beckn-onix binary with different configs. V2 is\n"
            "what travels between them; V1 never leaves the outer edges.\n\n"
            "Before running anything:\n\n"
            "    cd docs/migration-design/local-stack\n"
            "    docker compose up -d\n"
            "    cd v1-adapter && ./up.sh\n\n"
            "Then run folder 0, then folder 1. Stop with ./down.sh.",
        "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
    },
    "item": [health, the_point, alongside, inside, registry],
    "variable": [
        {"key": "legacy", "value": "http://localhost:3000"},
        {"key": "v1Adapter", "value": "http://localhost:3001"},
        {"key": "v2Adapter", "value": "http://localhost:3002"},
        {"key": "provider", "value": "http://localhost:9100"},
        {"key": "registry", "value": "http://localhost:9200"},
    ],
}

out = os.path.join(HERE, "openagrinet-v1-compatibility.postman_collection.json")
with open(out, "w") as f:
    json.dump(collection, f, indent=2)
    f.write("\n")
print("wrote", out)
