# catalog-publish

Builds the MandiPrice catalogs from Agmarknet and either writes them to disk or
publishes them to the discovery service. One run, then it exits — there is no
daemon here and no scheduler.

It is the adapter's crawler publish sweep with the crawler removed. The
pipeline it runs (`pkg/plugin/implementation/MandiPrice/publish/agmarknet.yaml`)
is unchanged and unaware: this supplies the three things the sweep would
otherwise supply — it serves the pipeline folder on a loopback port, builds the
registry record that sanctions the run, and makes the HTTP call.

Nothing about a deployment is written in Go. Every address, identity, date
window and mode comes from `catalog-publish.yaml`.

## What a run produces

**29 catalogs across 27 states, 2,240 markets, 2,040 geographic points.**

One catalog per state. Maharashtra and Madhya Pradesh exceed discovery's
256-geometry budget, so each is split into two (`MH` and `MH-2`) — which is
why there are 29 catalogs and not 27. Nine states return no rows from the
upstream at all and produce no catalog rather than an empty one.

Catalogs publish with `updateMode: FULL`. A resource absent from the body is
**deleted** from that catalog, so a market the upstream stops listing
disappears instead of lingering for ever. It also means a split state must have
*all* its catalogs published — each replaces only its own.

## Running it

```sh
cd beckn-onix
go build -o scripts/catalog-publish/bin/catalog-publish ./cmd/catalog-publish/

cp scripts/catalog-publish/env.example scripts/catalog-publish/.env
$EDITOR scripts/catalog-publish/.env        # the two credentials go here
set -a; . scripts/catalog-publish/.env; set +a
```

**Check what a run would do, without doing it:**

```sh
./scripts/catalog-publish/bin/catalog-publish \
  -config scripts/catalog-publish/catalog-publish.yaml -show-config
```

**Write one document per state, publish nothing** — the default:

```sh
OUTPUT_MODE=files CATALOG_OUT_DIR=./catalogs \
  ./scripts/catalog-publish/bin/catalog-publish \
    -config scripts/catalog-publish/catalog-publish.yaml
```

**Publish to discovery:**

```sh
OUTPUT_MODE=api DISCOVERY_URL=http://localhost:8090 \
  ./scripts/catalog-publish/bin/catalog-publish \
    -config scripts/catalog-publish/catalog-publish.yaml
```

`OUTPUT_MODE=both` does both. Each prints a table of every state — its catalog
id, how many markets and points it carries, and whether it landed.

A run takes a few minutes: the upstream is called once per state, sequentially,
because it rate-limits.

## Configuration

Everything is in `catalog-publish.yaml`, which reads its values from the
environment. `${VAR}` with no default **fails the run** rather than publishing
under an empty identity.

| Variable | Meaning |
|---|---|
| `OUTPUT_MODE` | `files`, `api` or `both`. Default `files` |
| `CATALOG_OUT_DIR` | where documents are written under `files`/`both` |
| `DISCOVERY_URL` | discovery base address; `/publish` is appended. Only used when publishing |
| `NETWORK_ID` | the network the catalogs are visible to. **Required in every mode** — it is stamped into the documents |
| `MANDI_PARTICIPANT_ID` | who the catalogs are published as |
| `MANDI_API_URI` | the Agmarknet address |
| `MANDI_FROM_DATE` / `MANDI_TO_DATE` | the window. `today`, `yesterday`, `N days ago` or `dd-MM-yyyy` |
| `MANDI_TOKEN_USER` / `MANDI_TOKEN_SECRET` | the Agmarknet credentials |

**Credentials cannot go in the config file.** The publish engine refuses to let
a config override an input the pipeline marks `secret`, so they are read from
the environment by the names the pipeline itself declares.

### Publish direct to discovery, not through the provider adapter

The adapter's `/publish` is MERGE-only and would reject `FULL`. Point
`DISCOVERY_URL` at discovery itself.

### The date window

`MANDI_FROM_DATE` defaults to `7 days ago`, not `yesterday`.

The onDemand catalog is a *directory* — which markets exist and what each deals
in — not a price feed. Asked for a single day it collapses to "what sold
yesterday": every Sunday and public holiday would publish almost nothing, and a
market trading on alternate days would look as though it trades nothing at all.

## Building the Postman collection

`build-postman.py` turns the documents a `files` run wrote into per-state
Postman requests — one publish request per catalog, one discover per state that
asserts the expected count.

```sh
cd scripts/catalog-publish

OUTPUT_MODE=files CATALOG_OUT_DIR=/tmp/mandi-states NETWORK_ID=oan.dev \
  ./bin/catalog-publish -config catalog-publish.yaml

NETWORK_ID=oan.dev DISCOVERY_URL=http://localhost:8090 \
SENDER_ID=provider.oan.dev RECEIVER_ID=discovery.oan.dev \
python3 build-postman.py \
  --catalogs /tmp/mandi-states/openagrinet-MandiPrice \
  --merge-into ../../../helmcharts/api-collection/OpenAgriNet.api-collection.json
```

It **replaces** the existing MandiPrice folder in place rather than appending,
so regenerating never leaves a stale copy that looks as authoritative as the
new one. Every deployment-specific value becomes a `{{variable}}`, and those
variables' defaults come from the arguments above — so a collection built for
one environment cannot silently carry another's address.

`--out FILE` writes a standalone collection instead of merging.

The merged collection is ~6.5 MiB, because it carries all 2,240 markets as
request bodies. That is the point of it; if it becomes unwieldy, use `--out`
and keep the states in a collection of their own.

## Things worth knowing

- **`networkId` is a visibility gate, not a filter.** A `/discover` whose
  context omits it returns nothing at all, however correct the rest is. This is
  the most common reason a working query appears to return zero.
- **`textSearch` is not supported** by this discovery build — "discover through
  spatial or filters".
- **`@`-prefixed fields need SQL/JSON-path quoting**:
  `@.resourceAttributes."@type"`. Bracket syntax parses but matches nothing.
- **Running twice is safe.** `FULL` makes a run idempotent: three consecutive
  runs leave exactly 29 catalogs and 2,240 resources, with no duplicates.
- **Only `onDemand` is run here.** `direct` additionally costs one upstream call
  per traded market-and-commodity pair — thousands of calls.
