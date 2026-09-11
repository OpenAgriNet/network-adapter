# mandi_publish

Collects, builds, and publishes Agmarknet Vistaar market catalogs for the Beckn/OpenAgriNet network.

The workflow is split into three deliberate stages, each with a reviewable seam on disk:

```
Upstream Agmarknet Vistaar
         |
         |  Plan 0: collect
         v
    markets.json
         |
         |  Plan 1: build
         v
    catalog/mandi-<STATE>.json
         |
         |  Plan 2: publish
         v
Provider Adapter (POST /publish)
```

Keeping these stages apart means collected data can be reviewed before payloads are built, and catalog JSON files can be inspected and diffed before anything reaches a network.

---

## 1. Collect (Plan 0)

Authenticates with Agmarknet Vistaar, reads master data and market-commodity mappings, and writes one normalized collection document (`markets.json`).

```sh
export MANDI_TOKEN_USER=... MANDI_TOKEN_SECRET=...
go run ./tools/publish/mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --out markets.json
```

- Credentials come from the environment only (`MANDI_TOKEN_USER`, `MANDI_TOKEN_SECRET`).
- Omit `--states` to walk every state the upstream reports.
- Dates are `dd-MM-yyyy` (default: today).
- Output records `coordinateQuality` (`ok`, `missing`, `suspect`, `outOfBounds`).
- Exit is non-zero if any state failed.

---

## 2. Build (Plan 1)

Reads `markets.json`, runs `mappings/catalog.yaml` through the embedded JSONata mapper, and writes one `catalog/publish` JSON file per state.

```sh
go run ./tools/publish/mandi_publish --in markets.json --catalog-out catalog/
```

### Options for Build

| Flag | Default | Meaning |
|---|---|---|
| `--in` | *none* | The collection document to read (e.g. `markets.json`) |
| `--catalog-out` | `catalog` | Directory to write per-state catalog files into |
| `--participant-id` | `$MANDI_PARTICIPANT_ID`, else `agmarknet` | Left half of binding key; `provider.id` and catalog ID prefix |
| `--network-id` | `$APP_NETWORK_ID`, else `oan-dev` | `publishDirectives[].visibleTo` |
| `--without-geometry` | `publish` | `publish` or `skip` — how to handle markets without `ok` coordinates |
| `--states` | *all* | Restrict building to named state codes |

### Invariants enforced

- **Zero-commodity markets skipped**: `supportedCommodities` has `minItems: 1`. Markets with zero commodities are omitted and counted in the run summary.
- **Coordinates**: GeoJSON `[longitude, latitude]` order. When `without-geometry` is `publish`, geometry-less markets carry the district `AdministrativeAreaReference` without a Point.
- **Deterministic output**: Resources sorted by `marketId`, commodities deduplicated and sorted by numeric code.
- **Two validity vocabularies**: Catalog envelope uses `startDate`/`endDate`; resource attributes use `startsAt`/`endsAt`.
- **Quiet states**: A state with zero publishable markets produces no file.

---

## 3. Publish (Plan 2)

Reads the catalog files produced by Plan 1 and POSTs each sequentially to the provider adapter's `/publish` endpoint (unsigned inbound; the provider adapter signs and forwards to the network).

```sh
export MANDI_PUBLISH_URL=http://localhost:9200
go run ./tools/publish/mandi_publish --publish --catalog-in catalog/ --states MH --dry-run
```

### Options for Publish

| Flag | Default | Meaning |
|---|---|---|
| `--publish` | `false` | Enables the publish stage |
| `--publish-url` | `$MANDI_PUBLISH_URL` | Base URL of the provider adapter (no default; refuses if absent) |
| `--catalog-in` | `catalog` | Directory of catalog files to publish |
| `--states` | *all* | Restrict publishing to named state codes |
| `--dry-run` | `false` | Prints target URL and catalog IDs without sending HTTP requests |
| `--retire-old` | `false` | Additionally posts `isActive: false` tombstone for `cat-agmarknet-mandi-prices` |

### Chaining Build and Publish

Build and publish can be executed in one command while still writing the intermediate files to disk:

```sh
MANDI_PUBLISH_URL=http://localhost:9200 \
  go run ./tools/publish/mandi_publish --in markets.json --catalog-out catalog/ --publish --states MH
```

### Publishing Outcome Checks

For each state, three conditions must hold:
1. HTTP 2xx response.
2. `results[0].status == "ACCEPTED"`.
3. `results[0].catalogId` matches the catalog's own ID.

Failures are logged verbatim and the process exits non-zero if any state fails.

---

## Retiring the Old Catalog

The single India-wide polygon catalog (`cat-agmarknet-mandi-prices`) is retired explicitly via `--retire-old`:

```sh
MANDI_PUBLISH_URL=http://localhost:9200 \
  go run ./tools/publish/mandi_publish --retire-old
```

---

## Verifying against the Live Upstream

```sh
# 1. Collect real data for Maharashtra
export MANDI_TOKEN_USER=... MANDI_TOKEN_SECRET=...
go run ./tools/publish/mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --out /tmp/mh.json

# 2. Build catalog
go run ./tools/publish/mandi_publish --in /tmp/mh.json --catalog-out /tmp/catalog

# 3. Dry-run publish
MANDI_PUBLISH_URL=http://localhost:9200 \
  go run ./tools/publish/mandi_publish --publish --catalog-in /tmp/catalog --states MH --dry-run
```
