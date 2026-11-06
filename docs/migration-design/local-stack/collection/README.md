# Try it yourself

`openagrinet-v1-compatibility.postman_collection.json` drives the local stack
that answers a Beckn V1 request out of the V2 network, with BharatVistaar
unchanged.

```
V1 caller --search--> V1 adapter --select--> V2 adapter --GET--> Mausamgram
              :3001                 :3002               :9100
```

Both adapters are the same beckn-onix binary with different config files. V2 is
what travels between them. V1 never leaves the outer edges, which is the whole
point: the caller sends V1 and receives V1 and has no idea a V2 network is
behind it.

## Start the stack

```
cd docs/migration-design/local-stack
docker compose up -d          # legacy V1 on :3000, mock provider on :9100
cd v1-adapter && ./up.sh      # registry :9200, V2 adapter :3002, V1 adapter :3001
```

`up.sh` builds beckn-onix from source, so the first run takes a minute. Stop
everything with `./down.sh` and `docker compose down`.

## Run it

Import the collection into Postman or Bruno and run folder 0, then folder 1.
The base URLs are collection variables, so nothing needs editing.

From the command line, with no Postman at all:

```
npx newman run collection/openagrinet-v1-compatibility.postman_collection.json
```

Twenty requests, thirty-three assertions. All of them should pass.

## What is in it

**0. Is everything up.** Four health checks. Run these if anything else
misbehaves: they tell you which half of the stack you forgot to start.

**1. V1 in, V1 out.** The claim. Five requests, each exactly as BharatVistaar
sends one today, each answered in the shape BharatVistaar expects, including
the 201 status a Nest service returns. The five cover a complete forecast, a
forecast with a missing day and null readings, a forecast that starts too late
to answer, a provider that is down, and a caller with no coordinates.

**2. The same request to the legacy service.** The identical payloads against
the real V1 service in Docker, which calls the provider directly and has never
heard of V2. Run a request here and its twin in folder 1 and diff the bodies.

**3. Inside the hop.** The two legs the V1 adapter makes for you, drivable on
their own: the V2 select it emits, and the provider GET the V2 adapter emits.
Compare the select's answer against the V1 body and you can see precisely what
the V2 representation keeps and what it drops.

**4. What the adapters read from the registry.** Both participant rows and both
action plans. Four small records are the entire difference between a reachable
capability and an unreachable one, and the two hops use the same mechanism one
level apart.

## Comparing all five at once

Diffing by eye gets old. `../v1-adapter/run.sh` sends all five payloads to both
services and reports every field that differs:

```
cd ../v1-adapter && ./run.sh
```

Today it prints six differences across two scenarios, and the other three match
field for field. `../v1-adapter/README.md` explains what each one is.

## If you would rather use curl

Everything the collection does, by hand. The coordinates select the scenario.

Answer a V1 request out of the V2 network:

```
curl -i -X POST http://localhost:3001/mobility/search \
  -H 'Content-Type: application/json' \
  -d @../requests/weather-mausamgram.json
```

That payload has `__LAT__` and `__LON__` placeholders in it, so substitute them:

```
sed 's/__LAT__/18.4/; s/__LON__/74.4/' ../requests/weather-mausamgram.json > /tmp/v1.json
curl -s -X POST http://localhost:3001/mobility/search \
  -H 'Content-Type: application/json' -d @/tmp/v1.json | jq .
```

The same request to the legacy service, for comparison:

```
curl -s -X POST http://localhost:3000/mobility/search \
  -H 'Content-Type: application/json' -d @/tmp/v1.json | jq .
```

The provider on its own, which needs the fake credential:

```
curl -s -u bv-local:local-dev-key \
  'http://localhost:9100/get-daily?lat=18.4&lon=74.4' | jq .
```

Latitude picks the scenario: `18.4` complete, `1.1` gaps, `2.2` late only,
`3.3` down, `0` no coordinates.

## Regenerating the collection

`build.py` writes the collection from the same request payload the test harness
uses, so the two cannot drift. Edit `build.py`, not the JSON:

```
python3 build.py
```

## A note on the credentials

`bv-local` / `local-dev-key` are invented for this stack and mean nothing
anywhere else. The mock provider checks them only so the adapter has to send a
credential at all, which is the part worth proving.
