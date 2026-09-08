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
```

| Parameter | Required | Description | Default |
|-----------|----------|-------------|---------|
| `bindingKeys` | **Yes** | Comma-separated capabilities this step answers to. No default is possible: a package serving a family cannot guess which of them a deployment has providers for. | — |
| `authScheme` | No | `none`, `basic`, `header` or `query`. POCRA needs none. | `none` |
| `maxResponseBytes` | No | Cap on what is read from the provider. | 4 MiB |

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

An inbound query resource is `informationMode: OnDemand`, and the schema pack
forbids an OnDemand resource from carrying `location`, `address` or
`facilityType`. So the search origin is read from
`message.contract.commitments[].fulfillment.stops[].location.geo` and the
requested type from `resourceAttributes.supportedFacilityTypes`.

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

Full runbook: `docs/agriculture-facility-testing.md`.
