# Areas: LGD codes and boundaries

The states and districts the catalog builders publish, keyed by LGD code. A
builder reads codes, names and boundaries from here; it never matches on names.
`areas.go` is the reader both builders use (`areas.Read`).

> **Source:** a subset of the area lookup in `OpenAgriNet/network-adapter`,
> branch `feat/area-gazetteer-tool`, commit `967f9b85`
> (`tools/area-lookups/data/areas/lgd-2026-09-03/`). Not regenerated here.

## What is in it

| Kept | Rows |
|---|---|
| Every State, LGD scheme | 36 |
| Maharashtra's districts (`parent_code` 27), LGD scheme | 36 |

Blocks, PIN codes and the ISO-3166-2 alias rows of the full lookup are left out.
Add a state's districts here when a builder covers that state.

| File | Holds |
|---|---|
| `areas.csv` | one row per area: `code_scheme`, `area_code`, `area_level`, `area_name`, parent, a representative point, a bounding box |
| `areas.geojsonl` | one GeoJSON `Feature` per area, keyed by `code_scheme`, `area_code`, `area_level`; its `geometry` is a `Polygon` or `MultiPolygon` |

Join the two on `(code_scheme, area_code, area_level)`. An LGD code is unique
within a level, not across levels, so `area_code` alone is not a key.

## Sources and credit

| Columns | Source | Snapshot |
|---|---|---|
| codes, names, parents | Local Government Directory, Ministry of Panchayati Raj (<https://lgdirectory.gov.in/>), Government Open Data License – India | 2026-09-03 |
| points, bounding boxes, boundaries | Survey of India, Administrative Boundary Database (<https://onlinemaps.surveyofindia.gov.in/>) | 2023-12-11 |

The boundary layer carries the LGD code itself, so boundaries were joined on the
code, never on names.
