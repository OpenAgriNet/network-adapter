# Administrative area lookup

Builds `areas.csv`, which resolves an OpenAgriNet `AdministrativeAreaReference`
(`codeScheme`, `areaCode`, `areaLevel`) to a name, parent and coordinate.

## Refresh

```bash
cd tools/area-lookups
python3 build_areas.py && python3 join_geometry.py
```

Writes `data/areas/latest/areas.csv` — 25,097 rows covering Country, State,
District, Block and PostalCode.

Add `--source lgd` to `join_geometry.py` for Block-level coordinates.

---

# Appendix — sources and columns

| Source | Columns it fills |
|---|---|
| **LGD**, Ministry of Panchayati Raj — <https://lgdirectory.gov.in/> | `code_scheme`, `area_code`, `area_level`, `area_name`, `parent_scheme`, `parent_code`, `census_2011_code`, `snapshot_date`, `source_url` |
| **Survey of India** *(default)* — <https://onlinemaps.surveyofindia.gov.in/> | `latitude`, `longitude`, `bbox_west`, `bbox_south`, `bbox_east`, `bbox_north`, `point_method`, `geometry_source`, `boundary_vintage` — State and District only |
| **BharatMaps / NIC** *(`--source lgd`)* — `mapservice.gov.in` | the same geometry columns, and additionally resolves Block |

Provenance, licence and coverage for each run are recorded in
`manifest.json` alongside the CSV.
