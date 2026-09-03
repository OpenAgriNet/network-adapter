# Administrative area lookup

Builds `areas.csv` and `areas.geojsonl`, which resolve an OpenAgriNet
`AdministrativeAreaReference` (`codeScheme`, `areaCode`, `areaLevel`) to a name,
parent, coordinate and boundary outline.

## Refresh

```bash
cd tools/area-lookups
python3 build_areas.py && python3 join_geometry.py && python3 validate.py
```

Writes `data/areas/latest/` — 25,140 rows covering Country, State, District,
Block and PostalCode, plus 807 polygons. `validate.py` exits non-zero if the
output is not usable, so the refresh is gated on it.

Add `--source lgd` to `join_geometry.py` for Block coordinates and 6,967
polygons.

## Integrate

Cache both files at startup as maps keyed on
`(code_scheme, area_code, area_level)`. Then per `AdministrativeAreaReference`:

1. **Key on those three fields only.** Ignore `areaName` — 117 areas are
   spelled differently upstream. A missing `areaLevel` is safe to infer for
   `ISO-3166-1` (Country), `ISO-3166-2` (State) and `IN-PIN` (PostalCode);
   never for `LGD`, whose codes repeat across levels.
2. Look the key up in `areas.csv`.
3. If `has_polygon` is `true`, return `areas.geojsonl[key]` — already a valid
   Beckn `GeoJSONGeometry`, no conversion needed.
4. Otherwise fall back to the point, but only if `point_method` is not
   `inherited:*`, which is an ancestor's coordinate rather than this area's.
   GeoJSON order is `[longitude, latitude]`.
5. Append the geometry as a new `coverageAreas` item; keep the coded one, since
   each item is a `oneOf`.

```python
key = (ref["codeScheme"], ref["areaCode"], ref["areaLevel"])
row = areas[key]                                    # areas.csv
geom = shapes[key] if row["has_polygon"] == "true" else None
```

`same_as` needs no handling: alias rows carry their own copy of the geometry.
Skip them only if you scan every feature instead of looking up a key, or a
state will match twice.

---

# Appendix — sources and columns

| Source | Columns it fills |
|---|---|
| **LGD**, Ministry of Panchayati Raj — <https://lgdirectory.gov.in/> | `code_scheme`, `area_code`, `area_level`, `area_name`, `parent_scheme`, `parent_code`, `parent_level`, `census_2011_code`, `snapshot_date`, `source_url` |
| **Survey of India** *(default)* — <https://onlinemaps.surveyofindia.gov.in/> | `latitude`, `longitude`, `bbox_west`, `bbox_south`, `bbox_east`, `bbox_north`, `point_method`, `geometry_source`, `boundary_vintage`, `has_polygon` — State and District only |
| **BharatMaps / NIC** *(`--source lgd`)* — `mapservice.gov.in` | the same geometry columns, and additionally resolves Block |
| **ISO 3166-2:IN** — static crosswalk in `build_areas.py` | `same_as`, and 43 extra `State` rows keyed by ISO code |

Two files, joined on the same `(code_scheme, area_code, area_level)` key:

| File | Holds |
|---|---|
| `areas.csv` | one row per area: identity, parent, a representative point, a bounding box |
| `areas.geojsonl` | one simplified `Polygon`/`MultiPolygon` per area, for containment tests |

`parent_code` is only meaningful together with `parent_level`: an LGD code is
unique within a level, not across levels, and 765 codes in this file name a
State, a District and a Block at once.

Every state is present under both schemes — `ISO-3166-2/IN-KA/State` alongside
`LGD/29/State` — because every OpenAgriNet example that names a State uses ISO
codes. The 7 codes ISO has withdrawn are carried too, as publishers still send
them. ISO codes States and Union Territories only, so District and Block stay
LGD-only.

Those alias rows duplicate their state's outline, 2.9 MB of the 14.3 MB file.
`same_as` records which row an alias mirrors, and `validate.py` proves the copy
has not drifted.

`has_polygon` says whether an outline exists, so a consumer can tell from the
CSV alone without opening the second file. It is `false` for PostalCode at every
setting, because no boundary source publishes pincode geometry.

`point_method` says where a coordinate came from. `centroid` and
`interior_grid` are the area's own geometry; `inherited:<level>` is an
ancestor's point reused. Most rows are inherited, so check this before treating
a coordinate as specific to the area.

Provenance, licence, coverage and every validation anomaly for a run are
recorded in `manifest.json` alongside the data.
