# Administrative area gazetteer

Resolves an OpenAgriNet `AdministrativeAreaReference` to a name, a parent chain
and a coordinate, using a cached CSV instead of a live API call.

```json
{ "codeScheme": "LGD", "areaCode": "466", "areaLevel": "District" }
```

becomes

```
LGD,466,District,Ahilyanagar,LGD,27,522,02Sep2026,<source_url>,
19.205391,74.675231,centroid,73.614375,18.329871,75.582971,19.995188,
SOI_Districts,2023-12-11
```

(one line in the file; wrapped here to fit)

`coverageAreas` in the OpenAgriNet schema packs is a `oneOf` between a coded
`AdministrativeAreaReference` and a Beckn `GeoJSONGeometry`. Publishers prefer
the coded form: it is short, stable and human-checkable. Consumers that need to
draw a map or run a distance filter need coordinates. This tool is the bridge,
built as a refreshable snapshot rather than a service so that resolution has no
runtime dependency.

### Where this fits

The `AdministrativeAreaReference` contract itself lives in the
[`network-specs`](https://github.com/OpenAgriNet/network-specs) repository, under
`schema/AgricultureResource/v0.1/attributes.yaml`. This tool only produces the
lookup table; it does not define or validate the schema.

It is a standalone build-time utility, **not part of the adapter runtime**. It is
Python in an otherwise Go repository, deliberately: it is stdlib-only, invoked by
hand or from CI when a refresh is wanted, and links nothing into the adapter
binary. Nothing under `cmd/` or `pkg/` imports it, and it adds no Go package, so
Go compilation is untouched. The output is a CSV any language can read.

Stdlib Python only, no pip install. Requires `bsdtar` (the default `tar` on
macOS) or `7z` to unpack `.7z` archives. Developed against Python 3.13.

---

## Quick start

Two stages, run in order:

```bash
cd tools/gazetteer

# Stage 1 - codes, names, parent chain. ~30 s, writes a dated directory.
python3 build_gazetteer.py

# Stage 2 - joins boundary geometry onto those codes and fills coordinates.
# First run downloads ~150 MB (SOI) and caches it. ~2 min warm.
python3 join_geometry.py
```

Result:

```
data/gazetteer/
  lgd-2026-09-02/          dated snapshot, never overwritten
    gazetteer.csv          25,097 rows, 6.3 MB
    manifest.json          provenance, coverage, data-quality report
  latest -> lgd-2026-09-02 stable path for consumers
  .cache/                  downloaded boundary layers, ~500 MB, safe to delete
```

To refresh, re-run both stages. Each run writes its own dated directory and
never touches an earlier one, so a Resource published against an older snapshot
still resolves. Only the `latest` symlink moves.

### Options

| Flag | Stage | Default | Effect |
|---|---|---|---|
| `--date 02Sep2026` | 1 | latest available | pin a specific LGD snapshot |
| `--out DIR` | 1 | `data/gazetteer` | output root |
| `--with-villages` | 1 | off | adds ~670k Village rows; no geometry exists for them |
| `--source soi\|lgd` | 2 | `soi` | which boundary source to join (see below) |
| `--gazetteer DIR` | 2 | `data/gazetteer/latest` | snapshot to fill in place |
| `--cache DIR` | 2 | `data/gazetteer/.cache` | where boundary layers are kept |
| `--with-geometry` | 2 | off | also write `boundaries.geojsonl` (335 MB) |
| `--no-inherit` | 2 | off | leave unmatched rows empty instead of borrowing the parent point |

Stage 2 is idempotent and re-runnable against an existing snapshot. It clears
every geometry column before joining, so switching `--source` fully replaces the
previous provenance rather than leaving a mixture.

---

## Stage 1 - `build_gazetteer.py`

### What it pulls

| Source | Value |
|---|---|
| Authority | **LGD (Local Government Directory)**, Ministry of Panchayati Raj — <https://lgdirectory.gov.in/> |
| Accessed via | `github.com/ramSeraph/opendata`, release tag `lgd-latest-extra1` |
| Licence | GODL-India |
| Why the mirror | `lgdirectory.gov.in` has no bulk export or documented API; the mirror re-extracts it daily and publishes dated `.csv.7z` assets. The `.gov.in` origin is recorded in the manifest either way. |

Four components are pulled by default, one optional:

| Component | Rows | Becomes `area_level` | Code field used |
|---|---|---|---|
| `states` | 36 | `State` | State Code |
| `districts` | 784 | `District` | District Code |
| `subdistricts` | 7,092 | `Block` | Sub-district Code |
| `pincode_villages` | ~670k | `PostalCode` (17,184 unique) | Pincode |
| `villages` *(opt-in)* | ~670k | `Village` | Village Code |

Plus one synthetic row: `ISO-3166-1 / IN / Country / India`, so a country-wide
`coverageAreas` entry resolves through the same lookup.

`pincode_villages` is village-grained, so many rows share one pincode. It is
collapsed to one row per pincode, keeping the district as the parent — pincodes
are postal routes, not areas, and have no boundary data anywhere in this
pipeline.

Two safeguards, both there because LGD's own column naming drifts:

- Header names are normalised to letters and digits only. `Sub-district Code`,
  `Sub-District Code` and `SubDistrict Code` all occur across files, as do
  `District Name(In English)` and `District Name (In English)`.
- If any expected level parses to zero rows the run **fails** rather than
  publishing a snapshot with a level silently missing.

The snapshot date is the newest date for which *every* component is published.
Components are not always released in lockstep, and a half-published day would
otherwise mix a stale `districts` file into an otherwise current gazetteer.

---

## Stage 2 - `join_geometry.py`

Joins published administrative boundary polygons onto the LGD codes from stage 1
and writes a representative point plus a bounding box for each area.

**The join key is always the LGD code carried inside the boundary layer. Names
are never used to join.** Codes are stable; names are not. LGD district 466 is
`Ahilyanagar` today and `Ahmednagar` in every boundary layer published so far.
117 areas (SOI) or 319 (LGD) join on a stable code while carrying a different
name upstream — see *Name drift* below.

### Sources

Both sources are mirrored from `github.com/ramSeraph/indian_admin_boundaries`
releases, because neither `.gov.in` origin is scriptable: `mapservice.gov.in`
returns `{"error":{"code":499,"message":"Token Required"}}`, and `data.gov.in`
serves a JavaScript application rather than fetchable assets. The upstream
`.gov.in` origin is recorded in the manifest for every run.

#### `--source soi` (default)

| Field | Value |
|---|---|
| Authority | **Survey of India** — <https://onlinemaps.surveyofindia.gov.in/Digital_Product_Show.aspx> |
| Licence | **open** |
| Vintage | 2023-12-11 |

| Level | Layer asset | Features | Code field read | Name field read |
|---|---|---|---|---|
| State | `SOI_States` | 40 | `State_LGD` | `STATE` |
| District | `SOI_Districts` | 742 | `DISTRICT_L` | `District` |

The 4 extra features in `SOI_States` are disputed-boundary polygons
(`DISPUTED (MADHYA PRADESH & GUJARĀT)` and three similar) carrying
`State_LGD = 0`. Code `0` is explicitly skipped when joining, so they never
attach to an area — but they *are* included in the national extent, which is
correct, since they are Indian territory.

`SOI_Subdistricts` exists but carries only `TEHSIL_C`, a census-style code, with
no LGD field. **Block therefore cannot be joined from SOI at all** and every
Block row falls back to inheriting its district's point.

#### `--source lgd`

| Field | Value |
|---|---|
| Authority | **BharatMaps / NIC** — `mapservice.gov.in/gismapservice/rest/services/BharatMapService/Admin_Boundary_GramPanchayat/MapServer` |
| Licence | **restricted — the upstream is not openly licensed** |
| Vintage | 2023-12-11 (states, districts), 2023-11-23 (subdistricts) |

| Level | Layer asset | Features | Code field read | Name field read |
|---|---|---|---|---|
| State | `LGD_States` | 36 | `State_LGD` | `STNAME` |
| District | `LGD_Districts` | 785 | `dist_lgd` | `dtname` |
| Block | `LGD_Subdistricts` | 6,471 | `subdt_lgd` | `sdtname` |

Materially better coverage — it is the only source here that can resolve Block —
but it is not open data. **Confirm licence terms before publishing derived
coordinates from this source.** This is why `soi` is the default: the open
source is what you get unless someone deliberately asks for the other one.

### How a point is chosen

1. **Centroid.** Area-weighted polygon centroid via the shoelace formula, with
   holes subtracted and every part of a multipart shape weighted by its true
   area. Computed on lon/lat treated as a plane.
2. **Interior fallback.** If that centroid falls outside the shape — normal for
   a crescent-shaped district or an island group — a grid scan finds an interior
   point near the largest part's centroid instead. 6 areas needed this.
3. **Country.** The `ISO-3166-1 / IN` row has no polygon of its own, so it is
   derived from the union of all state geometry.
4. **Parent inheritance.** Anything still unresolved borrows its parent's point,
   walked in level order so the chain works: a Block can inherit from a District
   that itself only inherited from its State. Disable with `--no-inherit`.

The planar lon/lat approximation is deliberate. Across a single Indian district
the error is well under a kilometre — irrelevant for a lookup hint, and this
point is a hint, not a measurement. Do not use these coordinates for area
calculation or as a survey reference.

---

## What gets stored

### `gazetteer.csv`

18 columns. The first 9 are written by stage 1, the last 9 by stage 2.

| Column | Written by | Meaning |
|---|---|---|
| `code_scheme` | 1 | matches `AdministrativeAreaReference.codeScheme` — `LGD`, `ISO-3166-1` or `IN-PIN` |
| `area_code` | 1 | matches `.areaCode` |
| `area_level` | 1 | matches `.areaLevel` — `Country`/`State`/`District`/`Block`/`Village`/`PostalCode` |
| `area_name` | 1 | matches `.areaName`, as LGD spells it |
| `parent_scheme` | 1 | scheme of the parent, for the inheritance fallback |
| `parent_code` | 1 | code of the parent |
| `census_2011_code` | 1 | crosswalk for joining census-coded boundary files |
| `snapshot_date` | 1 | LGD snapshot this row came from |
| `source_url` | 1 | exact asset the row was parsed from |
| `latitude` | 2 | representative point, WGS84 decimal degrees |
| `longitude` | 2 | " |
| `point_method` | 2 | how the point was derived — vocabulary below |
| `bbox_west` | 2 | bounding box of the area's geometry |
| `bbox_south` | 2 | " |
| `bbox_east` | 2 | " |
| `bbox_north` | 2 | " |
| `geometry_source` | 2 | which boundary layer supplied the point, e.g. `SOI_Districts` |
| `boundary_vintage` | 2 | publication date of that layer |

`geometry_source` is the column to check before trusting or redistributing a
coordinate — it names the exact layer, and therefore the licence, per row.

#### `point_method` vocabulary

| Value | Meaning |
|---|---|
| `centroid` | area-weighted centroid, inside the shape |
| `interior_grid` | centroid fell outside the shape; an interior point was substituted |
| `centroid_outside_shape` | centroid fell outside and no interior point was found; the centroid was kept |
| `aggregate:State` | derived from the union of all states — only the country row |
| `inherited:<level>` | borrowed from an ancestor at `<level>`; the point describes the parent, not this area |

An `inherited:*` point is a coarse locator, correct only to the granularity of
the level it names. A `PostalCode` row reading `inherited:District` sits at its
district's centre, which may be tens of kilometres from the actual post office.
Filter on `point_method` when precision matters.

### `manifest.json`

Provenance and a self-assessment for each snapshot. Stage 1 writes the
top-level keys; stage 2 adds the `geometry` block.

| Key | Contents |
|---|---|
| `snapshot_date`, `lgd_snapshot` | ISO date and the LGD asset date it came from |
| `provenance` | LGD authority URL, mirror and licence |
| `components` | exact download URL per component |
| `row_count`, `rows_by_level` | what was produced |
| `geometry_populated` | whether stage 2 has run |
| `geometry.source`, `.source_label`, `.licence`, `.upstream`, `.mirror` | which boundary source and **under what licence** |
| `geometry.layers` | asset name and vintage per level |
| `geometry.join_key`, `.point_definition` | how the join and the point were computed |
| `geometry.coverage` | per level: total, matched, inherited, empty |
| `geometry.name_drift_count` | areas joined on code but named differently upstream |
| `geometry.parent_containment_check` | rows checked, violations, and the full outlier list |

### `boundaries.geojsonl` (optional, `--with-geometry`)

One GeoJSON feature per line, for every joined area, with the gazetteer's
`code_scheme` / `area_code` / `area_level` injected into each feature's
properties. 6,953 features / 335 MB under `--source lgd`. Only worth generating
if you actually need to draw or intersect polygons; the CSV point plus bbox
covers lookup and coarse filtering.

### `.cache/`

Downloaded `.7z` archives and the layers extracted from them, ~500 MB
(`LGD_Subdistricts.geojsonl` alone is 263 MB). Safe to delete at any time; the
next run re-downloads. A cached layer is validated by reading its tail before
being reused — a download truncated by an interrupted run is detected and
refetched rather than silently producing a smaller join.

---

## Measured coverage

`--source soi` (open, the default):

| Level | Rows | Joined | Inherited | Empty | Joined % |
|---|---|---|---|---|---|
| Country | 1 | 1 | 0 | 0 | 100.0% |
| State | 36 | 36 | 0 | 0 | 100.0% |
| District | 784 | 728 | 56 | 0 | 92.9% |
| Block | 7,092 | 0 | 7,092 | 0 | 0.0% |
| PostalCode | 17,184 | 0 | 17,184 | 0 | 0.0% |

`--source lgd` (restricted):

| Level | Rows | Joined | Inherited | Empty | Joined % |
|---|---|---|---|---|---|
| Country | 1 | 1 | 0 | 0 | 100.0% |
| State | 36 | 36 | 0 | 0 | 100.0% |
| District | 784 | 771 | 13 | 0 | 98.3% |
| Block | 7,092 | 6,117 | 975 | 0 | 86.3% |
| PostalCode | 17,184 | 0 | 17,184 | 0 | 0.0% |

Every row resolves to *some* coordinate under both sources; none are empty. The
gap between the two is Block: 6,117 real block-level points versus none.

`PostalCode` never joins under either source, because no source here publishes
pincode boundaries. Those 17,184 rows always carry their district's point.

Districts that do not join are mostly recent splits — the boundary layers are
from 2023 and LGD has created districts since. They inherit their state's point.

---

## Data quality

### Name drift

117 areas (SOI) or 319 (LGD) join on a stable code while the boundary layer
spells the name differently:

```
State     LGD 35   gazetteer='Andaman And Nicobar Islands'  boundary='ANDAMAN & NICOBAR'
District  LGD 502  gazetteer='Ananthapuramu'                boundary='ANANTAPUR'
District  LGD 291  gazetteer='Kamrup'                       boundary='KAMRUP RURAL'
```

Every one of these would have failed a name-based join. They are reported, not
corrected: LGD's spelling is authoritative for the gazetteer. Pure diacritic
differences (`GUJARĀT` vs `Gujarat`) are folded out before counting, so the
reported figure is genuine drift rather than transliteration noise.

### Parent containment

Every joined point is checked against its parent's bounding box. Under `soi`,
764/764 are clean. Under `lgd`, 7 of 6,866 fall outside:

| Level | Code | Name | Parent | km outside | Assessment |
|---|---|---|---|---|---|
| District | 766 | Mauganj | Madhya Pradesh | 1,069 | **upstream error** — polygon sits in Arunachal Pradesh |
| Block | 3477 | Mauganj | Mauganj (766) | 1,211 | artifact of 766; the block itself is correctly near Rewa |
| Block | 3478 | Naigarhi | Mauganj (766) | 1,210 | artifact of 766 |
| Block | 3476 | Hanumana | Mauganj (766) | 1,182 | artifact of 766 |
| Block | 2109 | Katlichara | Hailakandi | 251 | **upstream error** |
| Block | 5908 | Yanam | Puducherry | 577 | **correct** — genuine Puducherry exclave |
| Block | 5913 | Mahe | Puducherry | 441 | **correct** — genuine Puducherry exclave |

**Outliers are reported, never repaired.** The two situations produce an
identical signal and cannot be told apart automatically: district 766's polygon
really is in the wrong state, while Mahe and Yanam really are hundreds of
kilometres from Puducherry proper. Substituting the parent's point would fix the
first case and corrupt the second. The full list goes to the manifest for a
human to adjudicate.

All points under both sources fall inside India's plausible envelope
(6.0–37.5 N, 68.0–97.5 E).

---

## Licence summary

| Source | Levels resolvable | Licence | Publishable? |
|---|---|---|---|
| LGD codes and names (stage 1) | all | GODL-India | yes |
| Survey of India boundaries | State, District | open | yes |
| BharatMaps / NIC boundaries | State, District, **Block** | restricted | **confirm first** |

Stage 2 defaults to `soi` so an ordinary run produces openly licensed
coordinates. Choosing `--source lgd` buys Block-level precision and takes on a
licence question; the manifest records that choice, its licence string and a
warning on every run, and `geometry_source` records it per row.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `no gazetteer at .../gazetteer.csv` | stage 2 ran before stage 1; run `build_gazetteer.py` |
| `no single date has all core components published` | LGD mirror is mid-publish; retry later or pin `--date` |
| `no rows produced for: <level>` | LGD renamed a column; extend the candidates in `pick()` |
| `<layer>: no areas carried field '<x>'` | the boundary layer's schema changed; check `SOURCES` in `join_geometry.py` |
| `cached copy is truncated, refetching` | informational; a prior download was interrupted and is being repaired |
| `extracted file is truncated` | delete the named `.7z` and `.geojsonl` from `.cache/` and retry |
| `need bsdtar or 7z to extract .7z` | install `p7zip`; macOS's stock `tar` already handles it |

Large layers take minutes to download and extract. `LGD_Subdistricts` is 263 MB
extracted; allow several minutes on a cold cache and do not interrupt it.

---

## Limits

- **Vintage mismatch is structural.** Codes refresh daily; boundaries are from
  2023. New districts will keep appearing with no polygon to join, and will
  inherit their state's point until the boundary layers are republished.
- **No Village geometry** from any source here, even with `--with-villages`.
- **No PostalCode geometry** from any source here.
- Points are lookup hints computed on a planar lon/lat approximation. Not for
  measurement, area calculation or survey use.
- Boundary layers are mirrored from GitHub, not fetched from `.gov.in`, because
  the government endpoints are not scriptable. The `.gov.in` origin is recorded
  in the manifest; the mirror is the delivery mechanism, not the authority.
