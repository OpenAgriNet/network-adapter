#!/usr/bin/env python3
"""Validate areas.csv and areas.geojsonl before they are relied on.

Stage 3. Stages 1 and 2 build the lookup; this stage tries to break it. The
checks are written independently of the builder — point-in-polygon is
reimplemented here rather than imported — so a bug in the join cannot pass by
agreeing with itself.

Exits non-zero if any check fails, so a refresh can be gated on it:

    python3 build_areas.py && python3 join_geometry.py && python3 validate.py

Usage:
    python3 validate.py
    python3 validate.py --areas data/areas/latest
    python3 validate.py --strict     # treat warnings as failures too

Stdlib only.
"""

import argparse
import csv
import json
import math
import sys
from pathlib import Path

EXPECTED_COLUMNS = [
    "code_scheme", "area_code", "area_level", "area_name",
    "parent_scheme", "parent_code", "parent_level", "census_2011_code",
    "snapshot_date", "source_url",
    "latitude", "longitude", "point_method",
    "bbox_west", "bbox_south", "bbox_east", "bbox_north",
    "geometry_source", "boundary_vintage", "has_polygon",
]

LEVELS = {"Country", "State", "District", "Block", "Village", "PostalCode"}

# Point derivations stage 2 can record. An unknown value means the builder
# grew a new path that this validator has not been taught to judge.
METHODS = {
    "centroid", "interior_grid", "centroid_outside_shape",
    "degenerate_geometry", "aggregate:State",
    "centroid:simplified", "interior_grid:simplified",
    "centroid_outside_shape:simplified", "degenerate_geometry:simplified",
}

# India's envelope with margin, used only to catch transposed lat/lon, which is
# the single most common way a coordinate column goes wrong.
LAT_RANGE = (6.0, 38.0)
LON_RANGE = (68.0, 98.0)


class Report:
    def __init__(self):
        self.failures = []
        self.warnings = []
        self.checks = 0

    def check(self, ok, label, detail=""):
        self.checks += 1
        if not ok:
            self.failures.append(f"{label}: {detail}" if detail else label)
        return ok

    def warn(self, label, detail=""):
        self.warnings.append(f"{label}: {detail}" if detail else label)


def sample(items, n=4):
    items = list(items)
    head = ", ".join(str(i) for i in items[:n])
    return head + (f" ... (+{len(items) - n} more)" if len(items) > n else "")


# --- independent geometry -------------------------------------------------

def in_ring(pt, ring):
    """Ray casting, written fresh so it does not share the builder's bugs."""
    x, y = pt
    inside = False
    for i in range(len(ring) - 1):
        x1, y1 = ring[i][0], ring[i][1]
        x2, y2 = ring[i + 1][0], ring[i + 1][1]
        if (y1 > y) != (y2 > y):
            xi = x1 + (y - y1) * (x2 - x1) / (y2 - y1)
            if x < xi:
                inside = not inside
    return inside


def in_polygon(pt, rings):
    if not in_ring(pt, rings[0]):
        return False
    return not any(in_ring(pt, h) for h in rings[1:])


def polys_of(geom):
    t = geom.get("type")
    if t == "Polygon":
        return [geom["coordinates"]]
    if t == "MultiPolygon":
        return geom["coordinates"]
    return []


def signed_area(ring):
    a = 0.0
    for i in range(len(ring) - 1):
        a += ring[i][0] * ring[i + 1][1] - ring[i + 1][0] * ring[i][1]
    return a / 2.0


# --- checks ---------------------------------------------------------------

def check_schema(rows, columns, rep):
    rep.check(columns == EXPECTED_COLUMNS, "column set",
              f"expected {EXPECTED_COLUMNS}, got {columns}")
    rep.check(bool(rows), "row count", "file has no data rows")

    keys = {}
    dup = []
    for i, r in enumerate(rows, start=2):
        k = (r["code_scheme"], r["area_code"], r["area_level"])
        if k in keys:
            dup.append(f"{k} at lines {keys[k]} and {i}")
        else:
            keys[k] = i
    rep.check(not dup, "primary key uniqueness", sample(dup))

    blank = [f"line {i}" for i, r in enumerate(rows, start=2)
             if not (r["code_scheme"] and r["area_code"] and r["area_level"])]
    rep.check(not blank, "identity columns present", sample(blank))

    bad_level = {r["area_level"] for r in rows} - LEVELS
    rep.check(not bad_level, "known area levels", sample(bad_level))

    noname = [f'{r["area_level"]}/{r["area_code"]}' for r in rows
              if not r["area_name"].strip()]
    if noname:
        rep.warn("rows without a name", sample(noname))
    return keys


def check_coordinates(rows, rep):
    bad_parse, out_env, out_box, bad_box, bad_method = [], [], [], [], []
    for r in rows:
        tag = f'{r["area_level"]}/{r["area_code"]}'
        if r["point_method"] and r["point_method"] not in METHODS \
                and not r["point_method"].startswith("inherited:"):
            bad_method.append(f'{tag} {r["point_method"]!r}')
        # An absent coordinate is a legitimate state, not a malformed one:
        # --no-inherit leaves every unjoinable level blank, and PostalCode has
        # no boundary source at any setting. It is reported as a warning below
        # so --strict still catches it, but it is not a parse failure.
        if not r["latitude"] and not r["longitude"]:
            continue
        try:
            lat = float(r["latitude"])
            lon = float(r["longitude"])
            box = (float(r["bbox_west"]), float(r["bbox_south"]),
                   float(r["bbox_east"]), float(r["bbox_north"]))
        except ValueError:
            bad_parse.append(tag)
            continue
        if not all(map(math.isfinite, (lat, lon) + box)):
            bad_parse.append(tag)
            continue
        if not (LAT_RANGE[0] <= lat <= LAT_RANGE[1]
                and LON_RANGE[0] <= lon <= LON_RANGE[1]):
            out_env.append(f"{tag} at {lat},{lon}")
        if box[0] > box[2] or box[1] > box[3]:
            bad_box.append(f"{tag} {box}")
        elif not (box[0] <= lon <= box[2] and box[1] <= lat <= box[3]):
            out_box.append(f"{tag} point {lon},{lat} box {box}")

    rep.check(not bad_parse, "coordinates parse as finite numbers", sample(bad_parse))
    rep.check(not out_env, "coordinates inside India's envelope", sample(out_env))
    rep.check(not bad_box, "bounding boxes are well ordered", sample(bad_box))
    rep.check(not out_box, "point lies inside its own bounding box", sample(out_box))
    rep.check(not bad_method, "point_method is a known value", sample(bad_method))

    empty = [f'{r["area_level"]}/{r["area_code"]}' for r in rows
             if not r["latitude"]]
    if empty:
        rep.warn(f"{len(empty):,} rows carry no coordinate", sample(empty))


def check_parents(rows, keys, rep):
    """Parent references must resolve, and inherited points must match them."""
    # Keyed on all three parts. An LGD code is only unique within its level,
    # so a (scheme, code) lookup would silently resolve a Block's parent to a
    # State that happens to share the number.
    by_key = {(r["code_scheme"], r["area_code"], r["area_level"]): r for r in rows}

    dangling, mismatched, no_level = [], [], []
    for r in rows:
        ps, pc, pl = (r.get("parent_scheme", ""), r.get("parent_code", ""),
                      r.get("parent_level", ""))
        tag = f'{r["area_level"]}/{r["area_code"]}'
        if not ps and not pc:
            if r["area_level"] != "Country":
                dangling.append(f"{tag} has no parent")
            continue
        if not pl:
            no_level.append(tag)
            continue
        if (ps, pc, pl) not in by_key:
            dangling.append(f"{tag} -> {ps}/{pc}/{pl} missing")
            continue
        if r["point_method"].startswith("inherited:") and r["latitude"]:
            # An inherited point must be byte-identical to some ancestor's, or
            # inheritance silently invented a coordinate.
            want = (r["latitude"], r["longitude"])
            chain, seen, cur = [], set(), r
            while True:
                nk = (cur.get("parent_scheme"), cur.get("parent_code"),
                      cur.get("parent_level"))
                if nk in seen or nk not in by_key:
                    break
                seen.add(nk)
                cur = by_key[nk]
                chain.append((cur["latitude"], cur["longitude"]))
            if want not in chain:
                mismatched.append(f"{tag} {want} not in any ancestor")

    rep.check(not no_level, "non-root rows declare a parent_level",
              sample(no_level))
    rep.check(not dangling, "parent references resolve", sample(dangling))
    rep.check(not mismatched, "inherited points match an ancestor",
              sample(mismatched))


def check_polygons(rows, areas_dir, rep):
    path = areas_dir / "areas.geojsonl"
    flags = {r["has_polygon"] for r in rows}
    rep.check(flags <= {"true", "false", ""}, "has_polygon is a boolean",
              sample(flags - {"true", "false", ""}))

    claimed = {(r["code_scheme"], r["area_code"], r["area_level"]): r
               for r in rows if r["has_polygon"] == "true"}
    if not path.exists():
        rep.check(not claimed, "areas.geojsonl exists",
                  f"{len(claimed):,} rows claim a polygon but {path.name} is absent")
        return
    if not claimed:
        rep.warn("no row claims a polygon", "nothing to cross-check")

    seen, dup, bad_json, bad_ring, unwound = {}, [], [], [], []
    outside, box_drift, orphan = [], [], []
    for n, line in enumerate(open(path, encoding="utf-8"), start=1):
        if not line.strip():
            continue
        try:
            feat = json.loads(line)
        except ValueError as e:
            bad_json.append(f"line {n}: {e}")
            continue
        pr = feat.get("properties") or {}
        key = (pr.get("code_scheme"), pr.get("area_code"), pr.get("area_level"))
        if key in seen:
            dup.append(f"{key} at lines {seen[key]} and {n}")
            continue
        seen[key] = n
        geom = feat.get("geometry") or {}
        polys = polys_of(geom)
        if not polys:
            bad_json.append(f"line {n}: geometry type {geom.get('type')!r}")
            continue

        for pi, rings in enumerate(polys):
            for ri, ring in enumerate(rings):
                if len(ring) < 4:
                    bad_ring.append(f"{key} part {pi} ring {ri}: {len(ring)} coords")
                elif ring[0] != ring[-1]:
                    bad_ring.append(f"{key} part {pi} ring {ri}: not closed")
                elif not all(len(c) >= 2 and math.isfinite(c[0])
                             and math.isfinite(c[1]) for c in ring):
                    bad_ring.append(f"{key} part {pi} ring {ri}: non-finite coord")
                else:
                    ccw = signed_area(ring) > 0
                    if (ri == 0) != ccw:
                        unwound.append(f"{key} part {pi} ring {ri}")

        row = claimed.get(key)
        if row is None:
            orphan.append(str(key))
            continue
        pt = (float(row["longitude"]), float(row["latitude"]))
        if not any(in_polygon(pt, rings) for rings in polys):
            outside.append(f"{key} point {pt}")
        xs = [c[0] for rings in polys for r in rings for c in r]
        ys = [c[1] for rings in polys for r in rings for c in r]
        drift = max(abs(min(xs) - float(row["bbox_west"])),
                    abs(min(ys) - float(row["bbox_south"])),
                    abs(max(xs) - float(row["bbox_east"])),
                    abs(max(ys) - float(row["bbox_north"])))
        if drift > 0.01:      # ~1.1 km, far beyond any sane tolerance
            box_drift.append(f"{key} {drift * 111000:,.0f} m")

    rep.check(not bad_json, "every polygon line is valid GeoJSON", sample(bad_json))
    rep.check(not dup, "one polygon per area key", sample(dup))
    rep.check(not bad_ring, "rings are closed with 4+ finite coords", sample(bad_ring))
    rep.check(not outside, "polygon contains the CSV point", sample(outside))
    rep.check(not box_drift, "polygon agrees with the CSV bbox", sample(box_drift))
    rep.check(not orphan, "no polygon without has_polygon=true", sample(orphan))

    missing = [str(k) for k in claimed if k not in seen]
    rep.check(not missing, "every has_polygon=true row has a polygon",
              sample(missing))
    if unwound:
        rep.warn(f"{len(unwound):,} rings not wound per RFC 7946", sample(unwound))
    return len(seen)


def main():
    p = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--areas", default="data/areas/latest",
                   help="directory holding areas.csv (default: data/areas/latest)")
    p.add_argument("--strict", action="store_true",
                   help="exit non-zero on warnings as well as failures")
    a = p.parse_args()

    areas_dir = Path(a.areas)
    areas_csv = areas_dir / "areas.csv"
    if not areas_csv.exists():
        sys.exit(f"no area lookup table at {areas_csv}")

    with open(areas_csv, encoding="utf-8", newline="") as f:
        reader = csv.DictReader(f)
        rows = list(reader)
        columns = list(reader.fieldnames or [])

    rep = Report()
    keys = check_schema(rows, columns, rep)
    check_coordinates(rows, rep)
    check_parents(rows, keys, rep)
    polys = check_polygons(rows, areas_dir, rep)

    print(f"{areas_csv}")
    print(f"  {len(rows):,} rows, {len(columns)} columns"
          + (f", {polys:,} polygons" if polys else ""))
    print(f"  {rep.checks} checks run")

    if rep.warnings:
        print(f"\n{len(rep.warnings)} warning(s):")
        for w in rep.warnings:
            print(f"  - {w}")
    if rep.failures:
        print(f"\n{len(rep.failures)} FAILURE(S):")
        for fl in rep.failures:
            print(f"  x {fl}")
        sys.exit(1)

    print("\nPASS" + (" (warnings present)" if rep.warnings else ""))
    if a.strict and rep.warnings:
        sys.exit(2)


if __name__ == "__main__":
    main()
