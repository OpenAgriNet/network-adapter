# Administrative area lookup

Builds `areas.csv` and `areas.geojsonl`, which resolve an OpenAgriNet
`AdministrativeAreaReference` (`codeScheme`, `areaCode`, `areaLevel`) to a name,
parent, coordinate and boundary outline.

## Refresh

```bash
cd tools/area-lookups
python3 build_areas.py && python3 join_geometry.py && python3 validate.py
```

Writes `data/areas/latest/` — 25,097 rows covering Country, State, District,
Block and PostalCode, plus 764 polygons. `validate.py` exits non-zero if the
output is not usable, so the refresh is gated on it.

Add `--source lgd` to `join_geometry.py` for Block coordinates and 6,924
polygons.

---

# Appendix — sources and columns

| Source | Columns it fills |
|---|---|
| **LGD**, Ministry of Panchayati Raj — <https://lgdirectory.gov.in/> | `code_scheme`, `area_code`, `area_level`, `area_name`, `parent_scheme`, `parent_code`, `parent_level`, `census_2011_code`, `snapshot_date`, `source_url` |
| **Survey of India** *(default)* — <https://onlinemaps.surveyofindia.gov.in/> | `latitude`, `longitude`, `bbox_west`, `bbox_south`, `bbox_east`, `bbox_north`, `point_method`, `geometry_source`, `boundary_vintage`, `has_polygon` — State and District only |
| **BharatMaps / NIC** *(`--source lgd`)* — `mapservice.gov.in` | the same geometry columns, and additionally resolves Block |

Two files, joined on the same `(code_scheme, area_code, area_level)` key:

| File | Holds |
|---|---|
| `areas.csv` | one row per area: identity, parent, a representative point, a bounding box |
| `areas.geojsonl` | one simplified `Polygon`/`MultiPolygon` per area, for containment tests |

`parent_code` is only meaningful together with `parent_level`: an LGD code is
unique within a level, not across levels, and 765 codes in this file name a
State, a District and a Block at once.

`has_polygon` says whether an outline exists, so a consumer can tell from the
CSV alone without opening the second file. It is `false` for PostalCode at every
setting, because no boundary source publishes pincode geometry.

`point_method` says where a coordinate came from. `centroid` and
`interior_grid` are the area's own geometry; `inherited:<level>` is an
ancestor's point reused. Most rows are inherited, so check this before treating
a coordinate as specific to the area.

Provenance, licence, coverage and every validation anomaly for a run are
recorded in `manifest.json` alongside the data.
