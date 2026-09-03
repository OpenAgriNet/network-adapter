# Administrative area gazetteer

Builds `gazetteer.csv`, which resolves an OpenAgriNet `AdministrativeAreaReference`
(`codeScheme`, `areaCode`, `areaLevel`) to a name, parent and coordinate.

## Refresh

```bash
cd tools/gazetteer
python3 build_gazetteer.py && python3 join_geometry.py
```

Writes `data/gazetteer/latest/gazetteer.csv` — 25,097 rows covering Country,
State, District, Block and PostalCode.

Add `--source lgd` to `join_geometry.py` for Block-level coordinates; that
source is not openly licensed.

---

# Appendix — sources and columns

| Source | Licence | Columns it fills |
|---|---|---|
| **LGD**, Ministry of Panchayati Raj — <https://lgdirectory.gov.in/> | GODL-India | `code_scheme`, `area_code`, `area_level`, `area_name`, `parent_scheme`, `parent_code`, `census_2011_code`, `snapshot_date`, `source_url` |
| **Survey of India** *(default)* — <https://onlinemaps.surveyofindia.gov.in/> | open | `latitude`, `longitude`, `bbox_west`, `bbox_south`, `bbox_east`, `bbox_north`, `point_method`, `geometry_source`, `boundary_vintage` — State and District only |
| **BharatMaps / NIC** *(`--source lgd`)* — `mapservice.gov.in` | **not open** | the same geometry columns, and additionally resolves Block |

Geometry joins on the LGD code carried inside each boundary layer, never on
names. Rows with no boundary match inherit their parent's coordinate, flagged in
`point_method`.
