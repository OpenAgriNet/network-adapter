# Administrative area gazetteer

Builds a CSV that resolves an OpenAgriNet `AdministrativeAreaReference`
(`codeScheme`, `areaCode`, `areaLevel`) to a name, parent and coordinate, so
`coverageAreas` can be resolved by lookup instead of a live API call.

Standalone build-time utility. Not a plugin, not part of the adapter runtime,
adds no Go package. Stdlib Python only.

## Steps to run

```bash
cd tools/gazetteer
python3 build_gazetteer.py && python3 join_geometry.py
```

That is the full refresh. Stage 1 pulls codes and names, stage 2 fills
coordinates. First run downloads ~150 MB and caches it; later runs reuse the
cache.

Output — `data/gazetteer/latest/`:

| File | Contents |
|---|---|
| `gazetteer.csv` | 25,097 rows: Country, State, District, Block, PostalCode |
| `manifest.json` | source URLs, licence, coverage achieved, data-quality report |

Each run writes a new dated directory and moves the `latest` symlink. Earlier
snapshots are never overwritten, so a Resource published against an older one
still resolves. `data/` is gitignored; delete it any time and re-run.

### Options

Defaults are correct for a normal refresh. Only these matter:

| Flag | Effect |
|---|---|
| `join_geometry.py --source lgd` | use BharatMaps geometry: adds Block-level points, but **not openly licensed** |
| `build_gazetteer.py --with-villages` | add ~670k Village rows (no geometry exists for them) |
| `join_geometry.py --with-geometry` | also write `boundaries.geojsonl` (335 MB polygons) |

---

# Appendix — sources and columns

## Stage 1 — `build_gazetteer.py`

**Source:** LGD (Local Government Directory), Ministry of Panchayati Raj —
<https://lgdirectory.gov.in/>, via the `ramSeraph/opendata` daily mirror.
Licence GODL-India. *Purpose: the authoritative list of area codes and names.*

| Column | Purpose |
|---|---|
| `code_scheme` | `LGD`, `ISO-3166-1` or `IN-PIN` — matches `.codeScheme` |
| `area_code` | matches `.areaCode` |
| `area_level` | `Country` / `State` / `District` / `Block` / `Village` / `PostalCode` |
| `area_name` | matches `.areaName`, as LGD spells it |
| `parent_scheme`, `parent_code` | parent area, used as the coordinate fallback |
| `census_2011_code` | crosswalk for joining census-coded files |
| `snapshot_date`, `source_url` | which LGD snapshot and asset the row came from |

Pulled per level: `states` → State, `districts` → District, `subdistricts` →
Block, `pincode_villages` → PostalCode (collapsed to one row per pincode), plus
one synthetic `ISO-3166-1 / IN` Country row.

## Stage 2 — `join_geometry.py`

Joins boundary polygons onto the LGD codes from stage 1. **Keys on LGD codes
only, never names** — 117 areas join on a stable code while the boundary layer
spells the name differently (LGD 466 is `Ahilyanagar`, every boundary layer says
`Ahmednagar`). *Purpose: give each area a coordinate and a bounding box.*

Fills these columns, whichever source is used:

| Column | Purpose |
|---|---|
| `latitude`, `longitude` | representative point, WGS84 |
| `bbox_west`, `bbox_south`, `bbox_east`, `bbox_north` | extent of the area |
| `point_method` | how the point was derived — see below |
| `geometry_source` | which boundary layer supplied it, i.e. **which licence applies** |
| `boundary_vintage` | publication date of that layer |

`point_method` values: `centroid` (area-weighted, inside the shape),
`interior_grid` (centroid fell outside; interior point substituted),
`aggregate:State` (country row), `inherited:<level>` (borrowed from an ancestor —
a coarse locator only, correct to that level's granularity).

### Sources

`--source soi` — **default, openly licensed.** Survey of India,
<https://onlinemaps.surveyofindia.gov.in/>. Vintage 2023-12-11.

| Level | Layer | Code field read |
|---|---|---|
| State | `SOI_States` | `State_LGD` |
| District | `SOI_Districts` | `DISTRICT_L` |

`SOI_Subdistricts` carries no LGD field, so **Block cannot be joined from SOI**
and inherits its district's point.

`--source lgd` — **not openly licensed; confirm terms before publishing derived
coordinates.** BharatMaps / NIC, `mapservice.gov.in`. Vintage 2023-11/12.

| Level | Layer | Code field read |
|---|---|---|
| State | `LGD_States` | `State_LGD` |
| District | `LGD_Districts` | `dist_lgd` |
| Block | `LGD_Subdistricts` | `subdt_lgd` |

Both are mirrored from `ramSeraph/indian_admin_boundaries` because neither
`.gov.in` origin is scriptable — `mapservice.gov.in` requires a token and
`data.gov.in` serves a JavaScript app. The `.gov.in` origin is recorded in the
manifest.

### Coverage

| Level | Rows | `soi` joined | `lgd` joined |
|---|---|---|---|
| State | 36 | 36 | 36 |
| District | 784 | 728 | 771 |
| Block | 7,092 | 0 | 6,117 |
| PostalCode | 17,184 | 0 | 0 |

No row is ever left empty — anything unjoined inherits its parent's point.
PostalCode never joins because no source publishes pincode boundaries.
Unjoined districts are recent splits that postdate the 2023 boundary layers.

### Known outliers

Under `--source lgd`, 7 of 6,866 points fall outside their parent's box. Two are
genuine upstream errors (district 766 Mauganj's polygon sits in Arunachal
Pradesh; block 2109 Katlichara), three are knock-ons of 766, and two are correct
— Mahe and Yanam really are exclaves of Puducherry. They are reported in the
manifest, never auto-corrected: repairing the errors would corrupt the exclaves.
`--source soi` is clean at 764/764.

Points are lookup hints computed on a planar lon/lat approximation. Not for
measurement or survey use.
