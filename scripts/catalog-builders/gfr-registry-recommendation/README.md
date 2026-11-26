# GFR catalog builder

Builds the catalog the GFR fertilizer flow discovers: one `openagrinet:AgricultureAdvisory`
per state with GFR crops, and one India-wide `openagrinet:AgricultureResource` for the soil
test. Doses and soil values are never in the catalog; the adapter fetches them on `select`.

1. Asks SHC for every state's crop registry (`getCropRegistries`, no login) and keeps the
   crops marked GFR available.
2. Reads each state's LGD name and boundary from `../areas/`, joined on the LGD state code.
   SHC's state code is the LGD code.
3. Writes one advice resource per state, for example `res-gfr-advice:23` for Madhya
   Pradesh: its GFR crops, its LGD code, SHC's own state id, its boundary, and the link to
   GFR's advice OpenAPI. Then the soil test resource, covering the rectangle around every
   state.

A state with no GFR crop, or missing from `../areas/`, is skipped and logged.

## Run it

```sh
cd scripts/catalog-builders
cp gfr-registry-recommendation/env.example gfr-registry-recommendation/.env   # fill in the values once
go run ./gfr-registry-recommendation
```

It writes `gfr-registry-recommendation/gfr-catalog-publish.json`, a `catalog/publish` body whose network parts and links are
Postman variables: `{{$guid}}`, `{{$isoTimestamp}}`, `{{networkId}}`, `{{senderId}}`, `{{receiverId}}`,
`{{bppId}}`, `{{bppUri}}`, `visibleTo` `["{{networkId}}", "{{otherNetworkId}}"]`, and the
pack and contract links `{{gfr-advisory-context}}`, `{{gfr-resource-context}}`, `{{gfr-advice-operation-ref}}`, `{{gfr-soil-operation-ref}}`. Paste it into
the API collection's publish request, which fills them, and send it from there.
The current full catalog is kept in helmcharts as `api-collection/catalogs/gfr-catalog-publish.json`;
replace it there after a rebuild. The output file itself is not committed here.

## Settings

| Variable | Needed | What |
|---|---|---|
| `SOIL_HEALTH_BASE_URL` | yes | SHC's GraphQL URL |
| `GFR_AREAS_DIR` | no | default `areas` |
| `GFR_PROVIDER_ID`, `GFR_CATALOG_ID` | no | default `gfr`, `cat-gfr` |
| `GFR_OUTPUT` | no | default `gfr-registry-recommendation/gfr-catalog-publish.json` |

## Watch out for

- **`GFR_PROVIDER_ID` must be the participant id GFR is registered under.** The adapter
  finds GFR by `offer.provider.id`.
- **`gfr-advice` and `gfr-soil-report` are fixed in the code.** GFR's contracts and the chat
  API both look for them.
- **The SHC state id travels in `coverageAreas`** (`codeScheme` `SHC`). GFR's recommendation
  call needs it, so do not drop it.
- **A republished catalog can keep resources removed from it**, so a state dropped here may
  still be discoverable.
