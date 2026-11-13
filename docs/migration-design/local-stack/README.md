# Local stack

*[how it works](../README.md) · [current state](../current-state.md) · [weather capability](../capabilities/weather-mausamgram.md)*

---

Runs the weather use case end to end on one machine, so the V1 Adapter can be built against a reply we have actually seen rather than one we have read about.

## What it runs

| | |
|---|---|
| **bv-beckn** | the real Bharat Vistaar V1 service, unmodified, on port 3000 |
| **mock-mausamgram** | a stand-in for the upstream forecast provider, on port 9100 |

Two containers is the whole use case. The Mausamgram path never reads Postgres, so no database is needed. Only the IMD weather path does, and that capability is staying where it is.

## Why the provider is mocked

The real Mausamgram credentials live in the legacy service's environment and nobody should need them to work on this. More importantly, a real provider gives a different answer every day, and the thing we are trying to prove is that two pieces of software turn **the same input** into **the same output**. A fixed provider is what makes that a test rather than an observation.

## Running it

```
docker compose up --build
./capture.sh
```

`capture.sh` sends the weather request once per scenario and writes each reply to `golden/`. That directory is the baseline the V1 Adapter has to reproduce.

Once the adapter is running, point the same script at it and compare:

```
./capture.sh http://localhost:3001 golden-adapter
python3 compare.py golden golden-adapter
```

`compare.py` reports differences by their path in the document, so a failure names the field that is wrong rather than printing two walls of JSON. It exits non-zero when anything differs, which makes it usable from a build.

It ignores exactly one field, `context.timestamp`, which the service regenerates on every call and which therefore never matches across two runs. Nothing else is excused. A run that reports no differences is the sign-off.

## The scenarios

The latitude in the request picks which payload the provider returns, so every case is driven from the request alone and nothing has to be reconfigured between runs.

| Scenario | lat | What it proves |
|---|---|---|
| **happy** | 18.4 | five complete days, the ordinary answer |
| **gaps** | 1.1 | the traps: a missing day, a zero rainfall, a missing humidity, an empty warning, a short wind array |
| **late-only** | 2.2 | the provider returns only day 3 onward, and V1 declines to answer |
| **provider-down** | 3.3 | the provider fails every attempt, and V1 still returns a success |
| **zero-coords** | 0 | V1 refuses before calling the provider at all |

## What running it established

Running this confirmed every behaviour the [weather capability page](../capabilities/weather-mausamgram.md) predicted from reading the code, and corrected two things.

**The traps are real, all of them.**

| | Reply |
|---|---|
| A day is missing from the middle | `Today, Day_3, Day_4, Day_5`, so the later days keep their numbers |
| Rainfall is zero | `0 mm`, not `N/A mm` |
| Humidity is absent | `N/A %`, with the unit still appended |
| The wind array has one element | a bare `N/A` |
| The warning is empty | falls through to the cloud message, and a bare `N/A` only when both are empty |
| Coordinates | the `Location` tag shows what was asked for, the fulfillment stop and item name show what the provider resolved to |

**The replies are `HTTP 201`, not `200`.** Every successful search, including the ones that answer with an empty catalog. This is the framework's default for a POST and nothing in the service overrides it. The adapter has to match it.

**The service returns a bare `{context, message}`.** There is no `responses[]` wrapper on the way out of the V1 service. That settles half of the open wrapper question: if clients require the wrapper, something in front of this service is adding it, and the adapter has to sit in the same place.

**The replies do not depend on where the service runs.** The same five scenarios were captured twice, once with the service running directly on a laptop and once from the container, and every field matched apart from the regenerated timestamp. So a difference found later is a difference in the mapping, which is the only reason this comparison is worth anything.

## A defect in the repo's own Dockerfile

The `Dockerfile` in the Bharat Vistaar Beckn repo no longer builds. It pins `node:20.0.0`, whose Debian release has been archived, so its `apt-get install curl` step fails on a 404 from the security mirror. The curl it is installing is unused in any case; the service makes its HTTP calls through axios. Its start script also points at `dist/main` while the build actually emits `dist/src/main`.

None of this is ours to fix, and we have not touched that repo. The compose file here inlines a working equivalent instead. It is worth flagging to whoever owns that service, because it means the running deployment cannot currently be rebuilt from source.

## The adapters beside it

`v1-adapter/` adds two more beckn-onix adapters, chained, and replays the same five requests through them:

```
V1 caller --search--> V1 adapter --select--> V2 adapter --GET--> Mausamgram
              :3001                 :3002               :9100
```

So a V1 request is answered out of the V2 network, with the V2 representation genuinely on the wire in between, and the reply can be diffed against the legacy one field by field. That is where the comparison this stack exists to support actually happens. See [v1-adapter/README.md](v1-adapter/README.md).

The V1 adapter also runs the other mode. Anything it is not configured to answer is forwarded to the legacy service on :3000 and the reply comes back untouched, which is checked here by sending the same request to both and comparing. They agree on status and on every byte of the body except the timestamp the legacy service regenerates per call. The weather request, which the adapter does answer, is served locally and never forwarded.

## Poking at it by hand

`collection/` is an API collection covering all of the above: the V1 requests, the legacy requests to diff them against, the two legs inside the hop, and the registry rows that make the hop work. Runs in Postman or from the command line with newman. See [collection/README.md](collection/README.md).

## What is not covered yet

The failure path proves V1 answers with an empty catalog rather than an error, but the retry budget is only partly exercised. A provider that refuses fast costs about four seconds. A provider that hangs costs up to about ninety. The second shape needs a slow mode in the mock before the adapter's timeout is chosen.

Forwarding runs into the same question from the other side, and it already bit once here. The legacy knowledge path takes about seventy-five seconds to fail in this stack, and the adapter's write timeout started at sixty, so the forwarded reply arrived after the deadline and the caller got a dropped connection instead of the legacy service's own error. It reads like the adapter failing when it was the old service being slow. A forwarding adapter's write timeout has to exceed the slowest thing it forwards to, which means the real one cannot be chosen until the old service's live latency is known.
