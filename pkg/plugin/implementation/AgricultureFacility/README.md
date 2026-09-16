# Agriculture Facility Plugin

A **provider step plugin** that serves `openagrinet:AgricultureFacility` by
calling an ordinary HTTP API that has never heard of Beckn.

Today that API is POCRA's aggregator, which answers
`pocra|openagrinet:AgricultureFacility` for the `select` action.

## What lives here

Almost nothing. Recognising a capability, resolving the call plan,
authenticating, calling with the registry's budget and translating in both
directions are all `internal/upstream`'s. This package owns its name and its
prerequisites — which are empty, because a facility search names the point and
the facility type it wants, and POCRA's search takes exactly those.

Named for the schema-pack family rather than for POCRA: a provider is a registry
row, and a second state aggregator would be another row and another mapping, not
another package.

## Configuration

```yaml
providerSteps:
  - id: AgricultureFacility
    config:
      bindingKeys: "pocra|openagrinet:AgricultureFacility"
      authScheme: none
      searchConcurrency: 4
```

## How a multi-type search is served

POCRA's search takes exactly ONE category code: a comma-separated pair answers
200 with no providers at all, and a category array is refused outright, both
verified against the live API. A Beckn payload asking for three facility types
therefore has to become three calls.

That is this package's own job, and all of it lives here:

- `search.go` is the step the adapter runs. It reads the facility types out of
  the payload, splits one payload into one single-type payload per type -- each
  with a fresh `context.messageId`, because POCRA returns the union of
  everything asked for under one id -- runs the ordinary upstream step over
  each part concurrently, and merges the answers into one.
- `payload.go` is the one path read this package does in Go rather than in a
  mapping, and the one place `supportedFacilityTypes` is located. Its own
  comment says what that costs.
- `internal/upstream` serves one payload with one call and knows none of this.
  `jsonmapper` compiles the two halves every mapping has and no third thing.
  `internal/concurrent` runs N of anything, bounded and ordered, and has never
  heard of Beckn.

**Ordering, worth knowing:** within one facility type the mapping ranks by
POCRA's distance, and that ranking survives the merge. ACROSS types the answer
is type-blocked -- every KrishiVigyanKendra, then every Warehouse -- rather
than globally nearest-first, because the schema pack says query-relative
distance is not a facility attribute, so the mapping drops it before the merge
could sort on it.

| Parameter | Required | Description | Default |
|-----------|----------|-------------|---------|
| `bindingKeys` | **Yes** | Comma-separated capabilities this step answers to. No default is possible: a package serving a family cannot guess which of them a deployment has providers for. | — |
| `providerIdAt` | No | Path override for where the provider-id half of a binding key sits in a payload. Beckn v2 convention if absent. | Beckn v2 convention |
| `capabilityCodeAt` | No | Path override for the capability-code half. Must be given together with `providerIdAt`. | Beckn v2 convention |
| `authScheme` | No | `none`, `basic`, `header` or `query`. POCRA needs none. | `none` |
| `maxResponseBytes` | No | Cap on what is read from the provider. | 4 MiB |
| `searchConcurrency` | No | How many of a multi-type search's calls run at once, up to `MaxFacilityTypes` (8, this package's own constant -- see `search.go`). 4 is every governed type at once -- full concurrency for this capability. **Trade-off:** defaults to 1 (sequential) because POCRA's failure mode when pushed is a 200 with an *empty* catalog, indistinguishable from "no results" -- a parallel search can silently drop a facility type with no error. Verify against the live API before raising it in production. | 1 (sequential) |

This package's `Config` (`AgricultureFacility.go`, a flat struct of its own because it carries `searchConcurrency`, which `upstream.Config` has no field for) also has the `basic`/`header`/`query` auth credential pairs (`usernameEnv`/`passwordEnv`, `headerName`/`headerValueEnv`, `queryName`/`queryValueEnv`). This plugin's `parseConfig` does not wire them through -- POCRA needs none of them. A second provider on `openagrinet:AgricultureFacility` (see "What lives here") that needs one adds the corresponding line to `parseConfig`, mirroring `maxResponseBytes`.

The id must also appear in the module's `steps:` list, and must be unique across
`steps` and `providerSteps` — a repeat is refused at startup, because both land
in one id-keyed map and one capability would otherwise be lost silently.

## Registry rows

Two, joined on `participantId`.

```json
{ "participantId": "pocra", "name": "PoCRA Provider Aggregator",
  "type": "upstream", "status": "active",
  "baseUrl": "https://middleware-bap-client.mahapocra.gov.in" }
```
```json
{ "bindingKey": "pocra|openagrinet:AgricultureFacility",
  "participantId": "pocra",
  "capabilityCode": "openagrinet:AgricultureFacility",
  "status": "active",
  "actions": [ { "action": "select", "method": "POST", "path": "/search",
                 "mappings": "<published>/agriculture-facility.select.yaml",
                 "timeoutMs": 30000, "retryMax": 2, "status": "active" } ] }
```

`retryMax` is 2. The step marks a 4xx other than 429 as permanent and stops
retrying it, so a schema NACK caused by our own malformed request costs one
attempt rather than three. What the budget buys is resilience against a 5xx, a
429 or a transport failure, backing off exponentially from 50ms.

## Facility types

The governed enum maps one-to-one onto POCRA's category codes. The translation
lives in the mapping and nowhere else.

| `FacilityType` | POCRA code |
|---|---|
| `CustomHiringCentre` | `chc` |
| `KrishiVigyanKendra` | `kvk` |
| `Warehouse` | `warehouse` |
| `SoilTestingFacility` | `soil_lab` |

Adding a type is three lines in
`config/mappings/pocra/agriculture-facility.select.yaml` — the forward table in
the request half, the inverse in the response half, and the governed value in the
precondition's list. No rebuild.

## Where the query lives

An inbound query resource is `informationMode: OnDemand`, and both its inputs
sit on `resourceAttributes`: the requested type in `supportedFacilityTypes`, the
point to search around in `location.geo`. WeatherObservation carries its query
point on `resourceAttributes.location` too, so a consumer that has written one
select can write this one.

The one difference is the wrapper, and it belongs to the packs rather than to
either plugin. AgricultureFacility types `location` as `CompleteLocation`, which
requires `geo`; WeatherObservation types it as a bare `CompleteGeoJSONGeometry`,
one level flatter. A payload that copies WeatherObservation's shape here fails
pack validation.

The origin used to be read from
`message.contract.commitments[].fulfillment.stops[].location.geo`, because the
pack forbade `location`, `address` and `facilityType` outright under OnDemand
until `network-specs` commit `b76c9ad8a5` on `schema-packs-v0.1` dropped that
constraint. The stop was a workaround for a rule that no longer exists.

The pack's `location` description still says not to populate it with a search
origin. That rule governs the answer, where an inferred point would be a false
claim about a real place, and the response half honours it: it emits no
`location` at all, because POCRA returns no per-facility coordinate, and emits
`address` instead.

**This convention is provisional.** It was chosen on design and has not been
confirmed against a payload captured from the network.

## What the answer deliberately omits

`location`, because POCRA returns no verified per-facility coordinate — the one
`gps` in its response is a fixed stub unrelated to the point that was asked for —
and the pack forbids substituting the search origin.

`services`, `capacity`, `website` and `lastUpdatedAt`, because POCRA supplies
none of them. Deriving services from the facility type, or `lastUpdatedAt` from
the time of the fetch, would assert something nobody verified.

Distance, which POCRA does supply, is used to order the resources nearest-first
and then dropped: the pack states that query-relative distance is not an
intrinsic facility attribute and belongs in result metadata.

`"Unknown"`, `"N/A"`, `"000000"` and `"-"` are treated as absent wherever POCRA
sends them, so a field is omitted rather than published as a placeholder. The
warehouse BPP uses `"-"` for a phone it does not have; the other three use
`"N/A"`.

`@context` is echoed from the request rather than restated here, so this mapping
does not have to know which pack identifier is current and cannot contradict
what the caller declared. Which matters: the identifier the packs name in their
own `x-jsonld`, `https://schemas.openagrinet.global/…`, has no DNS record, so a
deployment tracking the published ref sends the `raw.githubusercontent.com` pack
URL instead. Both are exercised.

## Testing

```sh
go test ./pkg/plugin/implementation/AgricultureFacility/...
```

25 pass, 2 skip. The two skips are live tests against the real POCRA API, opted
into with `POCRA_LIVE=1`.

`mappings_test.go` runs the shipped mapping through the real mapper and the real
step against a fake POCRA. It reads from `config/mappings/pocra/` rather than
from a fixture, so it breaks when what is deployed breaks.

`conformance_test.go` validates the answer against
`openagrinet:AgricultureFacility v0.1` with a real JSON Schema validator. It
also validates the pack's own published examples — if those fail, the
compilation is wrong and nothing else in that file means anything — and covers
the two things the schema is silent on: fields the pack does not declare, and
the README's prose mapping rules.

The schemas are not vendored. `schemacache_test.go` compiles the pack under the
URL that publishes it,

    https://github.com/OpenAgriNet/network-specs/tree/schema-packs-v0.1/schema/AgricultureFacility/v0.1

lets the validator resolve the pack's own `$ref`s, and caches each document
under `testdata/schema-cache/` (gitignored). A cold run needs the network; every
run after it is offline. With an empty cache **and** no network the schema tests
skip rather than fail. To refresh, delete the cache directory and re-run.
