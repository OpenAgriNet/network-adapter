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

## Resolving an ISO-3166-2 code

Every OpenAgriNet example that names a State uses `ISO-3166-2` rather than
`LGD`, so the file carries a row per state under both schemes: `IN-KA/State`
alongside `LGD/29/State`. The 7 codes ISO has withdrawn are present too, since
publishers still send them — `IN-TG` was retired in favour of `IN-TS` in
November 2023, and this repository's own examples used it.

An alias row holds its **own copy** of the point, box and outline, identical to
the row it mirrors. That is deliberate: a consumer resolving an ISO code does
one exact-match lookup on `(code_scheme, area_code, area_level)` and is done.
Nothing has to follow `same_as`, which exists to record where the geometry came
from and to let `validate.py` prove the copy has not drifted.

The cost is that the 43 state outlines appear twice in `areas.geojsonl`, which
is 2.9 MB of its 14.3 MB. Code scanning every feature for containment should
therefore skip rows with a non-empty `same_as`, or it will report the same
state twice.

ISO assigns codes to States and Union Territories only, so District and Block
remain LGD-only. There is nothing below State to translate.

`has_polygon` says whether an outline exists, so a consumer can tell from the
CSV alone without opening the second file. It is `false` for PostalCode at every
setting, because no boundary source publishes pincode geometry.

`point_method` says where a coordinate came from. `centroid` and
`interior_grid` are the area's own geometry; `inherited:<level>` is an
ancestor's point reused. Most rows are inherited, so check this before treating
a coordinate as specific to the area.

Provenance, licence, coverage and every validation anomaly for a run are
recorded in `manifest.json` alongside the data.
