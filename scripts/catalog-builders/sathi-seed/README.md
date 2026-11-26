# SATHI catalog builder

Builds the catalog that the SATHI seed stock flow discovers: one
`openagrinet:AgricultureResource` per district, each listing every active SATHI crop.
Stock itself is never in the catalog; the adapter fetches it live on each `select`.

1. Reads the state's districts and their boundaries from `../areas/`.
2. Asks SATHI's master API for the crops: `get-crop-group`, then `get-crops-list` for each
   active group. A crop listed in two groups is kept once.
3. Writes one resource per district, for example `res-sathi-stock:27:490` for Pune, with
   its LGD codes, its boundary, the crops and the link to SATHI's stock OpenAPI.

Every crop group it skips, and why, is logged.

## Run it

```sh
cd scripts/catalog-builders
cp sathi-seed/env.example sathi-seed/.env   # fill in the values once
go run ./sathi-seed
```

It writes `sathi-seed/sathi-catalog-publish.json`, a `catalog/publish` body whose network parts and links are
Postman variables: `{{$guid}}`, `{{$isoTimestamp}}`, `{{networkId}}`, `{{senderId}}`, `{{receiverId}}`,
`{{bppId}}`, `{{bppUri}}`, `visibleTo` `["{{networkId}}", "{{otherNetworkId}}"]`, and the
pack and contract links `{{sathi-resource-context}}`, `{{sathi-stock-operation-ref}}`. Paste it into
the API collection's publish request, which fills them, and send it from there.
The current full catalog is kept in helmcharts as `api-collection/sathi-seed/sathi-catalog-publish.json`;
replace it there after a rebuild. The output file itself is not committed here.

## Where the districts come from

From `../areas/`: the LGD codes and names of the state's districts, and their
Survey of India boundaries, joined on the LGD code. `SATHI_STATE_LGD_CODE` picks
the state; Maharashtra (`27`) is the area SATHI is offered for. See
`../areas/README.md` for the sources and credit.

## Settings

| Variable | Needed | What |
|---|---|---|
| `SATHI_AREAS_DIR` | no | default `areas` |
| `SATHI_STATE_LGD_CODE` | no | default `27` (Maharashtra) |
| `SATHI_MASTER_BASE_URL` | yes | SATHI's master API |
| `SATHI_PROVIDER_ID`, `SATHI_CATALOG_ID` | no | default `sathi`, `cat-sathi` |
| `SATHI_OUTPUT` | no | default `sathi-seed/sathi-catalog-publish.json` |

## Watch out for

- **`SATHI_PROVIDER_ID` must be the participant id SATHI is registered under.** The
  adapter finds SATHI by `offer.provider.id`.
- **`sathi-stock` is fixed in the code.** SATHI's contract and the chat API both look for it.
- **A republished catalog can keep resources removed from it**, so a district dropped from
  `../areas/` may still be discoverable.
