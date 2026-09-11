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

An inbound query resource is `informationMode: OnDemand`. The search origin is
read from `message.contract.commitments[].fulfillment.stops[].location.geo`
and the requested type from `resourceAttributes.supportedFacilityTypes`,
rather than from `location`/`address`/`facilityType` on `resourceAttributes` --
a convention this plugin keeps, not a schema requirement. The pack forbade
those fields under OnDemand until `network-specs` commit `b76c9ad8a5` on
`schema-packs-v0.1` dropped that constraint (see
`dev_docs/schema-onDemand-forbid-removed.md`); the plugin's behavior did not
change when that happened, since POCRA has no verified per-facility
coordinate to put there anyway.

The pack's own query example agrees:
`examples/on-demand-facility-discovery.json` is an OnDemand resource carrying
`supportedFacilityTypes` and `coverageAreas` and no `location`, `address` or
`facilityType`.

**One part is still provisional.** The shape of the fulfillment stop the search
origin is read from was chosen on design and has not been confirmed against a
payload captured from the network; the pack's example says nothing about the
stop.

## What the answer deliberately omits

`location`, because POCRA returns no verified per-facility coordinate — the one
`gps` in its response is a fixed stub unrelated to the point that was asked for —
and the pack forbids substituting the search origin.

`capacity` (except for a warehouse, which is the only type POCRA reports one
for), `capacity.basis`, `languages`, `website` and `lastUpdatedAt`, because
POCRA supplies none of them per facility and none of them follows from the
facility type. Taking `lastUpdatedAt` from the time of the fetch, a `basis` from
the pack example's wording, or a `website` from the portal URL POCRA repeats on
every item, would assert something nobody verified.

## What the answer derives from the facility type

`subjectCategories` and `services` are keyed on the governed `facilityType`,
using the values the pack's four Direct examples state for each type:

| `facilityType` | `subjectCategories` | `services[].code` |
|---|---|---|
| `CustomHiringCentre` | Facility, Crop, Practice | `FARM_MACHINERY_HIRE` |
| `KrishiVigyanKendra` | Facility, Crop, Livestock, Practice | `FARM_ADVISORY`, `FARMER_TRAINING` |
| `Warehouse` | Facility, Crop, Market | `GENERAL_STORAGE` |
| `SoilTestingFacility` | Facility, Crop | `SOIL_TESTING` |

These follow from what the governed type means rather than from anything POCRA
left unsaid, and the type itself is never guessed: it comes from the provider
id, the item's category tag or the provider's fulfillment, and an item none of
the three can type is dropped. Both fields are discovery surface the pack
indexes — `profile.json` lists `subjectCategories` and `services[].code` under
both `indexable_paths` and `filterable_paths` — so publishing `["Facility"]`
alone and no services left every facility unfilterable by domain and by service.

The address follows the same examples: `addressLocality` carries the settlement
(POCRA's village, or its taluka when there is no village), and
`extendedAddress` the administrative tail — `"Rahta, Ahmednagar district"`.
`addressLocality` is indexable and filterable too, so putting every
administrative part in `extendedAddress` made locality unsearchable.
`addressRegion` is `Maharashtra`, the state POCRA aggregates; POCRA's own
`address.region` is not read for it, since that field carries a revenue
division or `"Unknown"`.

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
