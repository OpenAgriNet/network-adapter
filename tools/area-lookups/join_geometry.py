#!/usr/bin/env python3
"""Stage 2: attach coordinates to a area lookup table built by build_areas.py.

Stage 1 produces codes without geometry, because LGD's directory dumps carry no
boundaries. This stage joins boundary layers onto those codes and fills in
latitude, longitude and a bounding box, so an AdministrativeAreaReference such as

    {"codeScheme": "LGD", "areaCode": "466", "areaLevel": "District"}

resolves to a coordinate without a live API call.

The join key is always the LGD code carried inside the boundary layer itself.
Names are never used to join. Over a hundred areas currently have a stable LGD
code but a different name in the boundary files than in current LGD (LGD 466 is
"Ahilyanagar" today and "Ahmednagar" in every published boundary layer), so a
name join would silently drop them.

Sources
    --source soi   Survey of India, openly licensed. State + District only.
    --source lgd   LGD codes on BharatMaps geometry. Adds Block, better District
                   coverage, but the upstream is not openly licensed.

Levels a source cannot cover are not left blank: they inherit their nearest
resolved ancestor's point, recorded in point_method as inherited:<level>, so
every row resolves to something and the provenance stays visible. Pass
--no-inherit to leave them empty instead.

Usage
    python3 join_geometry.py
    python3 join_geometry.py --source lgd
    python3 join_geometry.py --areas data/areas/latest
    python3 join_geometry.py --source lgd --with-geometry
    python3 join_geometry.py --simplify 0.005   # coarser, smaller outlines

Stdlib only. Requires bsdtar (default `tar` on macOS) or 7z to extract .7z.
"""

import argparse
import csv
import datetime as dt
import json
import math
import shutil
import subprocess
import sys
import time
import unicodedata
import urllib.error
import urllib.request
from pathlib import Path

REPO = "ramSeraph/indian_admin_boundaries"
BASE = f"https://github.com/{REPO}/releases/download"

# Each level maps to (release tag, asset stem, LGD code field, name field).
# The code field is the join key; the name field is only read to report name
# drift, never to match on.
SOURCES = {
    "soi": {
        "label": "Survey of India",
        "licence": "open",
        "upstream": "https://onlinemaps.surveyofindia.gov.in/Digital_Product_Show.aspx",
        "note": "SOI_Subdistricts carries no LGD code, so Block cannot be joined "
                "from this source.",
        "layers": {
            "State":    ("states", "SOI_States", "State_LGD", "STATE"),
            "District": ("districts", "SOI_Districts", "DISTRICT_L", "District"),
        },
    },
    "lgd": {
        "label": "LGD codes on BharatMaps geometry",
        "licence": "restricted - upstream is not openly licensed",
        "upstream": "https://mapservice.gov.in/gismapservice/rest/services/"
                    "BharatMapService/Admin_Boundary_GramPanchayat/MapServer",
        "note": "Broader coverage than SOI. Confirm licence terms before "
                "publishing derived coordinates.",
        "layers": {
            "State":    ("states", "LGD_States", "State_LGD", "STNAME"),
            "District": ("districts", "LGD_Districts", "dist_lgd", "dtname"),
            "Block":    ("subdistricts", "LGD_Subdistricts", "subdt_lgd", "sdtname"),
        },
    },
}

# Written by stage 1 as empty strings and filled here. Kept in sync with
# build_areas.py OUT_COLUMNS.
GEO_COLUMNS = ["latitude", "longitude", "point_method",
               "bbox_west", "bbox_south", "bbox_east", "bbox_north",
               "geometry_source", "boundary_vintage", "has_polygon"]

# Simplification tolerance for areas.geojsonl, in degrees. At Indian latitudes
# 0.001 deg is roughly 110 m, which is far below the precision any coded
# administrative area implies and cuts the district layer from ~460 MB to ~9 MB.
# Full-fidelity polygons remain available via --with-geometry.
SIMPLIFY_DEG = 0.001

# Smallest polygon part treated as a real place, in square degrees, which is
# about 116 m2 at Indian latitudes.
#
# Published boundaries carry hairline slivers: positive-area, but spikes rather
# than places. Two show why a plain ">0" test is not enough. Hailakandi
# (LGD 289) ships a four-vertex, 0.1 m2 splinter 41 km south of the district,
# which stretches its bbox 41 km into a neighbour. Kancheepuram (LGD 574) has a
# 1 m2 spike whose bounding box still measures 141 x 184 m, pulling the western
# edge 16 km out. Either one makes a consumer using the box as a prefilter match
# points far outside the area.
#
# The floor is checked against area, not extent, because that is what separates
# a spike from an islet. Measured on SOI_Districts it cuts 25 parts: the largest
# dropped is 110 m2 and the smallest kept is 130 m2, both Andaman islets, so
# nothing of consequence sits near the boundary.
MIN_PART_AREA_SQ_DEG = 1e-8

# Child level -> parent level, used when a level has no geometry of its own.
# Ordered parent-first so the inheritance pass can walk down in one sweep.
PARENT_LEVEL = {"State": "Country", "District": "State",
                "Block": "District", "PostalCode": "District",
                "Village": "Block"}

LEVEL_ORDER = ["Country", "State", "District", "Block", "Village", "PostalCode"]


def log(msg):
    print(f"  {msg}", file=sys.stderr)


def simplify_name(name):
    """Fold a place name to letters and digits, for comparison only.

    SOI writes names in transliterated capitals with macrons ("GUJARĀT",
    "ARUNĀCHAL PRADESH") where LGD writes plain ASCII title case. Comparing raw
    strings flags several hundred of those as renames and buries the genuine
    ones, so accents, case, spacing and punctuation are all removed before
    deciding whether a name really differs.
    """
    decomposed = unicodedata.normalize("NFKD", name)
    stripped = "".join(c for c in decomposed if not unicodedata.combining(c))
    return "".join(c for c in stripped.lower() if c.isalnum())


# --- geometry, planar on lon/lat degrees ------------------------------------
#
# Every calculation below treats lon/lat as a flat plane. Over a single Indian
# district the distortion is far smaller than the difference between one
# defensible representative point and another, and the output is a lookup hint
# rather than a measurement. Areas are therefore only ever used as relative
# weights between parts of the same shape, never reported as real areas.

def closed(ring):
    """Ensure the ring's last vertex repeats its first."""
    if len(ring) >= 2 and (ring[0][0] != ring[-1][0] or ring[0][1] != ring[-1][1]):
        return list(ring) + [ring[0]]
    return ring


def ring_area_centroid(ring):
    """Signed shoelace area and centroid of one closed ring."""
    ring = closed(ring)
    a = cx = cy = 0.0
    for i in range(len(ring) - 1):
        x0, y0 = ring[i][0], ring[i][1]
        x1, y1 = ring[i + 1][0], ring[i + 1][1]
        cross = x0 * y1 - x1 * y0
        a += cross
        cx += (x0 + x1) * cross
        cy += (y0 + y1) * cross
    a *= 0.5
    if a == 0.0:
        # Degenerate or zero-width ring: fall back to the vertex mean so a
        # sliver polygon still yields a usable point instead of a crash.
        pts = ring[:-1] or ring
        return 0.0, (sum(p[0] for p in pts) / len(pts),
                     sum(p[1] for p in pts) / len(pts))
    return a, (cx / (6.0 * a), cy / (6.0 * a))


def polygon_centroid(rings):
    """Area and centroid of one polygon, subtracting its holes.

    Ring winding in published Indian boundary data does not reliably follow the
    GeoJSON right-hand rule, so orientation is ignored: ring 0 is taken as the
    outer ring and contributes positively, every later ring is a hole and
    contributes negatively.
    """
    total = sx = sy = 0.0
    for i, ring in enumerate(rings):
        a, (cx, cy) = ring_area_centroid(ring)
        w = abs(a) if i == 0 else -abs(a)
        total += w
        sx += cx * w
        sy += cy * w
    if total == 0.0:
        _, c = ring_area_centroid(rings[0])
        return 0.0, c
    return abs(total), (sx / total, sy / total)


def shape_parts(geom):
    """[(area, centroid, rings)] for each polygon in a Polygon/MultiPolygon."""
    t = (geom or {}).get("type")
    if t == "Polygon":
        polys = [geom["coordinates"]]
    elif t == "MultiPolygon":
        polys = geom["coordinates"]
    else:
        return []
    parts = []
    for rings in polys:
        if not rings or not rings[0]:
            continue
        area, centroid = polygon_centroid(rings)
        parts.append((area, centroid, rings))
    return parts


def point_in_ring(pt, ring):
    """Ray-casting test for a point against one closed ring."""
    ring = closed(ring)
    x, y = pt
    inside = False
    for i in range(len(ring) - 1):
        x0, y0 = ring[i][0], ring[i][1]
        x1, y1 = ring[i + 1][0], ring[i + 1][1]
        if (y0 > y) != (y1 > y):
            xint = x0 + (y - y0) * (x1 - x0) / (y1 - y0)
            if x < xint:
                inside = not inside
    return inside


def point_in_polygon(pt, rings):
    if not point_in_ring(pt, rings[0]):
        return False
    return not any(point_in_ring(pt, r) for r in rings[1:])


def interior_point(rings, target, grid=48):
    """An interior point of `rings`, as close to `target` as practical.

    The area-weighted centroid of a crescent-shaped district, or of an island
    group like Andaman & Nicobar, can land in the sea. When that happens, scan a
    coarse grid over the shape's bounding box and keep the interior sample
    nearest the centroid, so the published coordinate is at least on land inside
    the correct area.
    """
    xs = [p[0] for p in rings[0]]
    ys = [p[1] for p in rings[0]]
    west, east, south, north = min(xs), max(xs), min(ys), max(ys)
    best = None
    best_d = None
    for i in range(1, grid):
        x = west + (east - west) * i / grid
        for j in range(1, grid):
            y = south + (north - south) * j / grid
            if not point_in_polygon((x, y), rings):
                continue
            d = (x - target[0]) ** 2 + (y - target[1]) ** 2
            if best_d is None or d < best_d:
                best, best_d = (x, y), d
    return best


class Accumulator:
    """Area-weighted point and union bbox over one or more shapes.

    A handful of areas are published as several features sharing one LGD code
    (29 of 6,360 LGD subdistricts), and the country point is derived from every
    state at once. Both need the same accumulation, so geometry is folded in
    incrementally and only the largest single part is retained for the
    interiority test, which keeps memory flat regardless of input size.
    """

    __slots__ = ("area", "sx", "sy", "bbox", "best_area", "best_rings",
                 "best_centroid", "features", "degenerate", "dbbox",
                 "drings", "dcentroid")

    def __init__(self):
        self.area = self.sx = self.sy = 0.0
        self.bbox = None
        self.best_area = -1.0
        self.best_rings = None
        self.best_centroid = None
        self.features = 0
        # Zero-area parts, tracked apart from the real ones. See add().
        self.degenerate = 0
        self.dbbox = None
        self.drings = None
        self.dcentroid = None

    @staticmethod
    def _extend(box, rings):
        xs = [p[0] for p in rings[0]]
        ys = [p[1] for p in rings[0]]
        got = (min(xs), min(ys), max(xs), max(ys))
        if box is None:
            return got
        w, s, e, n = box
        return (min(w, got[0]), min(s, got[1]), max(e, got[2]), max(n, got[3]))

    def add(self, geom):
        """Fold one feature in, quarantining its negligible parts.

        Parts below MIN_PART_AREA_SQ_DEG are digitising artifacts rather than
        places, so they are kept out of both the point and the bbox. They are
        still counted, and used only if an area turns out to have nothing else,
        so no area is silently dropped.
        """
        parts = shape_parts(geom)
        if not parts:
            return False
        self.features += 1
        for area, (cx, cy), rings in parts:
            if area < MIN_PART_AREA_SQ_DEG:
                self.degenerate += 1
                self.dbbox = self._extend(self.dbbox, rings)
                if self.drings is None:
                    self.drings, self.dcentroid = rings, (cx, cy)
                continue
            self.area += area
            self.sx += cx * area
            self.sy += cy * area
            if area > self.best_area:
                self.best_area, self.best_rings, self.best_centroid = area, rings, (cx, cy)
            self.bbox = self._extend(self.bbox, rings)
        return True

    def result(self, check_interior=True):
        """(lon, lat, bbox, method) or None if nothing usable was added."""
        if self.bbox is None or self.best_rings is None:
            # Every part was degenerate. Fall back to them rather than dropping
            # the area, which would leave a blank row instead of a coarse point.
            if self.dbbox is None or self.drings is None:
                return None
            return (self.dcentroid[0], self.dcentroid[1], self.dbbox,
                    "degenerate_geometry")
        if self.area > 0.0:
            lon, lat = self.sx / self.area, self.sy / self.area
        else:
            lon, lat = self.best_centroid
        method = "centroid"
        if check_interior and not point_in_polygon((lon, lat), self.best_rings):
            # For a multipart shape the all-parts centroid can sit outside every
            # individual part, so re-aim at the largest part before scanning.
            target = self.best_centroid
            if point_in_polygon(target, self.best_rings):
                lon, lat = target
            else:
                alt = interior_point(self.best_rings, target)
                if alt is None:
                    method = "centroid_outside_shape"
                else:
                    lon, lat = alt
                    method = "interior_grid"
        return lon, lat, self.bbox, method


# --- simplification ---------------------------------------------------------

def _rdp(pts, eps):
    """Ramer-Douglas-Peucker on an open lon/lat polyline.

    Iterative rather than recursive: a district ring can carry 24k vertices and
    the recursive form overflows Python's stack on the worst of them.
    """
    if len(pts) < 3:
        return list(pts)
    keep = [False] * len(pts)
    keep[0] = keep[-1] = True
    stack = [(0, len(pts) - 1)]
    while stack:
        lo, hi = stack.pop()
        if hi - lo < 2:
            continue
        ax, ay = pts[lo]
        bx, by = pts[hi]
        dx, dy = bx - ax, by - ay
        n = math.hypot(dx, dy)
        far, fi = -1.0, lo
        for i in range(lo + 1, hi):
            px, py = pts[i]
            if n:
                d = abs(dy * (px - ax) - dx * (py - ay)) / n
            else:
                d = math.hypot(px - ax, py - ay)
            if d > far:
                far, fi = d, i
        if far > eps:
            keep[fi] = True
            stack.append((lo, fi))
            stack.append((fi, hi))
    return [pts[i] for i, k in enumerate(keep) if k]


def simplify_ring(ring, eps):
    """Simplify a closed ring, returning a closed ring or None if it collapses.

    The ring is split at the vertex farthest from its first point and each half
    is simplified as an open chain. Running RDP straight down a closed ring
    would anchor it on a zero-length segment, since the first and last vertex
    coincide, and the perpendicular distance test degenerates there.
    """
    q = [tuple(p[:2]) for p in ring]
    if len(q) > 1 and q[0] == q[-1]:
        q = q[:-1]
    if len(q) < 3:
        return None

    ax, ay = q[0]
    j = max(range(1, len(q)), key=lambda i: math.hypot(q[i][0] - ax, q[i][1] - ay))
    head = _rdp(q[:j + 1], eps)
    tail = _rdp(q[j:] + [q[0]], eps)
    out = head[:-1] + tail

    # Drop consecutive duplicates that rounding can introduce, then require a
    # ring with real extent: 3 distinct vertices plus the closing repeat.
    ded = [out[0]]
    for pt in out[1:]:
        if pt != ded[-1]:
            ded.append(pt)
    if ded[0] == ded[-1]:
        ded = ded[:-1]
    if len(ded) < 3:
        return None
    return [[round(x, 5), round(y, 5)] for x, y in ded] + \
           [[round(ded[0][0], 5), round(ded[0][1], 5)]]


def _signed_area(ring):
    a = 0.0
    for i in range(len(ring) - 1):
        x1, y1 = ring[i][0], ring[i][1]
        x2, y2 = ring[i + 1][0], ring[i + 1][1]
        a += x1 * y2 - x2 * y1
    return a / 2.0


def _wind(ring, ccw):
    """Force ring orientation. RFC 7946 wants exterior CCW, holes CW."""
    if (_signed_area(ring) > 0) != ccw:
        return list(reversed(ring))
    return ring


def simplify_geometry(parts, tolerance):
    """Simplify accumulated (rings) parts into one GeoJSON geometry.

    parts is a list of polygons, each a list of rings with the exterior first.
    Returns (geometry, kept_area) or (None, 0.0) if everything collapsed.
    """
    out = []
    for rings in parts:
        # Scale the tolerance to the part. A flat 110 m is negligible on a
        # 7,000 km2 district but costs 2% of Lakshadweep's 30 km2, so a tiny
        # island would ship visibly the wrong size. Tying the tolerance to the
        # part's own diagonal keeps the relative error roughly constant, and
        # small parts are cheap to keep detailed.
        xs = [pt[0] for pt in rings[0]]
        ys = [pt[1] for pt in rings[0]]
        diag = math.hypot(max(xs) - min(xs), max(ys) - min(ys))
        eps = min(tolerance, diag / 300.0) if diag else tolerance

        # Back off the tolerance rather than losing the part. Kancheepuram
        # (LGD 574) has a genuine 130x180 m enclave that vanishes at 110 m and
        # would silently cut 16 km off the district's western extent. A small
        # part carries few vertices, so keeping it whole costs nothing.
        ext = None
        for attempt in (eps, eps / 4.0, eps / 16.0, 0.0):
            ext = simplify_ring(rings[0], attempt)
            if ext is not None:
                break
        if ext is None:
            continue          # under three distinct vertices even untouched
        keep = [_wind(ext, True)]
        for hole in rings[1:]:
            h = simplify_ring(hole, eps)
            if h is not None:
                keep.append(_wind(h, False))
        out.append(keep)
    if not out:
        return None, 0.0
    area = sum(abs(_signed_area(r[0])) - sum(abs(_signed_area(h)) for h in r[1:])
               for r in out)
    if len(out) == 1:
        return {"type": "Polygon", "coordinates": out[0]}, area
    return {"type": "MultiPolygon", "coordinates": out}, area


# --- fetching ---------------------------------------------------------------

def extract(archive, dest):
    for cmd in (["tar", "-xf", str(archive), "-C", str(dest)],
                ["7z", "x", f"-o{dest}", "-y", str(archive)],
                ["7zz", "x", f"-o{dest}", "-y", str(archive)]):
        if shutil.which(cmd[0]) is None:
            continue
        if subprocess.run(cmd, capture_output=True).returncode == 0:
            return
    sys.exit("need bsdtar or 7z to extract .7z archives")


def _tail_is_valid(path):
    """True if the file's last non-empty line parses as JSON.

    Guards against a cached .geojsonl left truncated by an interrupted
    extraction. Such a file reads fine for thousands of lines and then simply
    stops, which would silently join fewer areas rather than fail.

    Only the tail is read, but the window has to grow until it provably holds a
    whole line: LGD_States packs 36 features into 25 MB, so one line can exceed
    any fixed window and the last fragment would fail to parse even though the
    file is intact. Reading until two newlines are in view, or until the start of
    the file is reached, guarantees the final segment is a complete line.
    """
    try:
        size = path.stat().st_size
        if size == 0:
            return False
        window = 1 << 20
        with open(path, "rb") as f:
            while True:
                offset = max(0, size - window)
                f.seek(offset)
                tail = f.read()
                if offset == 0 or tail.count(b"\n") >= 2 or window >= (1 << 30):
                    break
                window *= 4
    except OSError:
        return False
    lines = [ln for ln in tail.split(b"\n") if ln.strip()]
    if not lines:
        return False
    try:
        json.loads(lines[-1].decode("utf-8", "replace"))
        return True
    except ValueError:
        return False


def ensure_layer(tag, stem, cache):
    """Download and extract one boundary layer, reusing a valid cached copy."""
    cache.mkdir(parents=True, exist_ok=True)
    out = cache / f"{stem}.geojsonl"
    # A symlink whose target is gone still occupies the name, so exists() is
    # False while extract() would follow the link into a missing directory and
    # fail. Clear it and treat the layer as absent.
    if out.is_symlink() and not out.exists():
        log(f"{stem:<20} cached symlink is dangling, refetching")
        out.unlink()
    if out.exists() and _tail_is_valid(out):
        log(f"{stem:<20} cached")
        return out
    if out.exists():
        log(f"{stem:<20} cached copy is truncated, refetching")
        out.unlink()

    archive = cache / f"{stem}.geojsonl.7z"
    if not archive.exists():
        url = f"{BASE}/{tag}/{stem}.geojsonl.7z"
        req = urllib.request.Request(url, headers={"User-Agent": "oan-area-lookup"})
        # Retry transient failures. These layers are up to 263 MB, so a dropped
        # connection part-way through is likely enough to be worth handling
        # rather than aborting a refresh that has already done real work.
        for attempt in range(1, 4):
            try:
                with urllib.request.urlopen(req, timeout=900) as r, \
                        open(archive, "wb") as f:
                    shutil.copyfileobj(r, f)
                break
            except urllib.error.HTTPError as e:
                sys.exit(f"{stem} not available ({e.code}): {url}")
            except (urllib.error.URLError, TimeoutError, OSError) as e:
                archive.unlink(missing_ok=True)
                if attempt == 3:
                    sys.exit(f"{stem}: download failed after 3 attempts ({e})")
                wait = 2 ** attempt
                log(f"{stem}: {e} - retrying in {wait}s")
                time.sleep(wait)
        log(f"{stem:<20} downloaded {archive.stat().st_size / 1e6:,.1f} MB")

    extract(archive, cache)
    if not out.exists():
        sys.exit(f"{stem}: no {stem}.geojsonl inside archive")
    if not _tail_is_valid(out):
        sys.exit(f"{stem}: extracted file is truncated; "
                 f"delete {archive} and {out} and retry")
    return out


def layer_vintage(path):
    """The layer's own publication date, taken from the archived mtime.

    The .7z preserves the timestamp the boundary file was built upstream, which
    is the date that matters for provenance. It is not the date this script ran,
    and the two are usually years apart.
    """
    try:
        return dt.datetime.fromtimestamp(path.stat().st_mtime).date().isoformat()
    except OSError:
        return ""


def load_points(path, code_field, name_field):
    """code -> (lon, lat, bbox, method, name), one entry per LGD code."""
    accs = {}
    names = {}
    skipped = 0
    degenerate = 0
    for line in open(path, encoding="utf-8", errors="replace"):
        if not line.strip():
            continue
        feat = json.loads(line)
        props = feat.get("properties") or {}
        code = props.get(code_field)
        if code in (None, "", " ", 0, "0"):
            skipped += 1
            continue
        code = str(code).strip()
        acc = accs.get(code)
        if acc is None:
            acc = accs[code] = Accumulator()
        if not acc.add(feat.get("geometry")):
            skipped += 1
            continue
        names.setdefault(code, str(props.get(name_field) or "").strip())

    out = {}
    for code, acc in accs.items():
        res = acc.result()
        if res is None:
            skipped += 1
            continue
        lon, lat, bbox, method = res
        degenerate += acc.degenerate
        out[code] = (lon, lat, bbox, method, names.get(code, ""))
    return out, skipped, degenerate


def country_point(path):
    """Point and bbox for the whole country, from the union of all states.

    The ISO-3166-1 row has no LGD polygon of its own. Deriving it from state
    geometry means a country-wide coverageArea still resolves instead of being
    the one blank row in the file. The interiority test is skipped: a national
    point weighted across all states is meaningful even though it need not fall
    inside the single largest state.
    """
    acc = Accumulator()
    for line in open(path, encoding="utf-8", errors="replace"):
        if line.strip():
            acc.add(json.loads(line).get("geometry"))
    res = acc.result(check_interior=False)
    if res is None:
        return None
    lon, lat, bbox, _ = res
    return lon, lat, bbox, "aggregate:State"


# --- join -------------------------------------------------------------------

def set_point(row, lat, lon, bbox, method, source_layer, vintage):
    # has_polygon is deliberately not touched: write_polygons owns it, and an
    # inheriting row keeps the false it was given.
    row.update({
        "latitude": f"{lat:.6f}",
        "longitude": f"{lon:.6f}",
        "point_method": method,
        "bbox_west": f"{bbox[0]:.6f}",
        "bbox_south": f"{bbox[1]:.6f}",
        "bbox_east": f"{bbox[2]:.6f}",
        "bbox_north": f"{bbox[3]:.6f}",
        "geometry_source": source_layer,
        "boundary_vintage": vintage,
    })


def run(source, gazdir, cache, with_geometry, inherit, simplify):
    conf = SOURCES[source]
    gazdir = Path(gazdir).resolve()
    cache = Path(cache)
    areas_csv = gazdir / "areas.csv"
    if not areas_csv.exists():
        sys.exit(f"no area lookup table at {areas_csv} - run build_areas.py first")

    with open(areas_csv, encoding="utf-8", newline="") as f:
        reader = csv.DictReader(f)
        rows = list(reader)
        columns = list(reader.fieldnames or [])
    if not rows:
        sys.exit(f"{areas_csv} has no rows")
    for c in GEO_COLUMNS:
        if c not in columns:
            columns.append(c)

    # Clear every geometry column before joining. Without this, a second run
    # against an already-populated CSV keeps rows the new source cannot supply:
    # switching from lgd to soi would leave 6,117 Block points still sourced
    # from LGD_Subdistricts while the manifest declares the licence as open.
    for r in rows:
        for c in GEO_COLUMNS:
            r[c] = ""

    log(f"source {conf['label']} ({conf['licence']})")
    log(f"{len(rows):,} rows from {areas_csv}")

    points, vintages, layer_paths = {}, {}, {}
    for level, (tag, stem, code_field, name_field) in conf["layers"].items():
        path = ensure_layer(tag, stem, cache)
        pts, skipped, degenerate = load_points(path, code_field, name_field)
        # Zero coded areas means the upstream renamed its code column, not that
        # the level is genuinely empty. Fail rather than silently publish a
        # area lookup table where a whole level fell back to its parent.
        if not pts:
            sys.exit(f"{stem}: no areas carried field {code_field!r} - the "
                     f"upstream schema likely changed; check SOURCES[{source!r}]")
        points[level] = pts
        vintages[level] = stem
        layer_paths[level] = path
        log(f"{stem:<20} {len(pts):,} coded areas, vintage {layer_vintage(path)}"
            + (f", {skipped:,} features without a usable code or geometry"
               if skipped else "")
            + (f", {degenerate:,} sub-{MIN_PART_AREA_SQ_DEG:g}-sq-deg parts "
               f"excluded as artifacts" if degenerate else ""))

    dates = {lvl: layer_vintage(p) for lvl, p in layer_paths.items()}
    stats = {}
    for r in rows:
        stats.setdefault(r["area_level"],
                         {"total": 0, "matched": 0, "inherited": 0, "empty": 0})
        stats[r["area_level"]]["total"] += 1

    # Pass 1: direct joins on the LGD code carried in the boundary layer.
    drift = []
    resolved = {}
    for r in rows:
        level = r["area_level"]
        pts = points.get(level)
        if not pts or r["code_scheme"] != "LGD":
            continue
        hit = pts.get(r["area_code"])
        if not hit:
            continue
        lon, lat, bbox, method, bname = hit
        set_point(r, lat, lon, bbox, method, vintages[level], dates[level])
        stats[level]["matched"] += 1
        resolved[(r["code_scheme"], r["area_code"], level)] = (lat, lon, bbox, level)
        if bname and r["area_name"] and simplify_name(bname) != simplify_name(r["area_name"]):
            drift.append((level, r["area_code"], r["area_name"], bname))

    # Pass 2: the country row, derived from the union of all state geometry.
    if "State" in layer_paths:
        agg = country_point(layer_paths["State"])
        if agg:
            lon, lat, bbox, method = agg
            for r in rows:
                if r["area_level"] != "Country" or r["latitude"]:
                    continue
                set_point(r, lat, lon, bbox, method, vintages["State"], dates["State"])
                stats["Country"]["matched"] += 1
                resolved[(r["code_scheme"], r["area_code"], "Country")] = \
                    (lat, lon, bbox, "Country")

    # Polygons are written here, between joining and inheriting, because
    # re-deriving a point against its simplified outline has to happen before
    # descendants copy that point.
    polygons = write_polygons(conf, gazdir, cache, rows, simplify, resolved)

    # Pass 3: inherit downward for anything still unresolved.
    #
    # This walks parent-to-child in level order so it completes in one sweep. A
    # Block whose District itself only inherited from a State still resolves,
    # and point_method records the level the geometry actually came from rather
    # than the immediate parent.
    if inherit:
        by_level = {}
        for r in rows:
            by_level.setdefault(r["area_level"], []).append(r)
        for level in LEVEL_ORDER:
            parent = PARENT_LEVEL.get(level)
            if parent is None:
                continue
            for r in by_level.get(level, []):
                if r["latitude"]:
                    continue
                hit = resolved.get((r.get("parent_scheme"), r.get("parent_code"), parent))
                if not hit:
                    continue
                lat, lon, bbox, origin = hit
                set_point(r, lat, lon, bbox, f"inherited:{origin}",
                          vintages.get(origin, ""), dates.get(origin, ""))
                stats[level]["inherited"] += 1
                resolved[(r["code_scheme"], r["area_code"], level)] = \
                    (lat, lon, bbox, origin)

    for r in rows:
        if not r["latitude"]:
            stats[r["area_level"]]["empty"] += 1

    with open(areas_csv, "w", encoding="utf-8", newline="") as f:
        w = csv.DictWriter(f, fieldnames=columns)
        w.writeheader()
        w.writerows(rows)

    containment = check_containment(rows)
    if with_geometry:
        write_geometry(conf, gazdir, cache, rows)
    update_manifest(source, conf, gazdir, stats, vintages, dates, drift,
                    inherit, containment, polygons)
    report(areas_csv, stats, drift, conf, containment, polygons)
    return stats


def check_containment(rows):
    """Flag directly-joined points that fall outside their parent's bbox.

    An independent check on the join itself: a District point sitting outside
    its own State's box means the code matched the wrong polygon upstream. The
    comparison is against the parent's bounding box rather than its outline, so
    a coastline simplified differently between two layers does not register.

    Outliers are reported, never repaired. Two different situations produce the
    same signal and cannot be told apart automatically:

      - a genuine upstream error, such as LGD district 766 (Mauganj, Madhya
        Pradesh) whose published polygon sits in Arunachal Pradesh, roughly
        1,000 km from the state it belongs to
      - a legitimate exclave, such as the Puducherry blocks Mahe and Yanam,
        which really are several hundred km from Puducherry proper

    Substituting a parent point would fix the first and corrupt the second, so
    the full list goes to the manifest for a human to adjudicate.
    """
    boxes = {}
    for r in rows:
        if r["latitude"] and not r["point_method"].startswith("inherited"):
            boxes[(r["code_scheme"], r["area_code"], r["area_level"])] = r

    checked = 0
    outliers = []
    for r in rows:
        if not r["latitude"] or r["point_method"].startswith("inherited"):
            continue
        parent = boxes.get((r.get("parent_scheme"), r.get("parent_code"),
                            PARENT_LEVEL.get(r["area_level"])))
        if not parent:
            continue
        checked += 1
        lon, lat = float(r["longitude"]), float(r["latitude"])
        west, south = float(parent["bbox_west"]), float(parent["bbox_south"])
        east, north = float(parent["bbox_east"]), float(parent["bbox_north"])
        # A degree of slack absorbs differing border simplification between two
        # independently produced layers.
        pad = 1.0
        if (west - pad <= lon <= east + pad) and (south - pad <= lat <= north + pad):
            continue
        dx = max(west - lon, 0.0, lon - east)
        dy = max(south - lat, 0.0, lat - north)
        km = math.hypot(dx * 111.0 * math.cos(math.radians(lat)), dy * 111.0)
        outliers.append({
            "level": r["area_level"],
            "code_scheme": r["code_scheme"],
            "area_code": r["area_code"],
            "area_name": r["area_name"],
            "latitude": r["latitude"],
            "longitude": r["longitude"],
            "parent_code": r.get("parent_code", ""),
            "parent_name": parent["area_name"],
            "km_outside_parent_bbox": round(km),
        })
    outliers.sort(key=lambda o: -o["km_outside_parent_bbox"])
    return {"checked": checked, "violations": len(outliers), "outliers": outliers}


def write_polygons(conf, gazdir, cache, rows, tolerance, resolved):
    """Emit areas.geojsonl: one simplified polygon per directly-joined area.

    A coded area is an area, not a point, so a consumer resolving
    coverageAreas needs the outline to answer containment questions. The CSV
    keeps the point and box for cheap prefiltering; the real shape lives here,
    keyed identically on (code_scheme, area_code, area_level).

    Also sets has_polygon on every row, so a consumer can tell from the CSV
    alone whether a real outline exists without opening this file. That flag
    cannot be inferred from point_method: a row can carry its own centroid and
    still have no polygon here, if simplification collapsed the shape.
    """
    wanted = {}
    for r in rows:
        r["has_polygon"] = "false"
        if (r["latitude"] and not r["point_method"].startswith("inherited")
                and r["area_level"] in conf["layers"]):
            wanted.setdefault(r["area_level"], {})[r["area_code"]] = r
    if not wanted:
        return {"features": 0, "note": "no level in this source has geometry"}

    out = gazdir / "areas.geojsonl"
    written = 0
    collapsed, outside, repointed = [], [], []
    worst_area = worst_bbox = 0.0
    with open(out, "w", encoding="utf-8") as fout:
        for level, (_tag, stem, code_field, _name) in conf["layers"].items():
            keep = wanted.get(level) or {}
            if not keep:
                continue
            path = cache / f"{stem}.geojsonl"

            # Collect every feature carrying a code before simplifying: an area
            # published as several polygons has to stay one multipart feature,
            # or a point-in-polygon test against an outlying island fails.
            acc = {}
            for line in open(path, encoding="utf-8", errors="replace"):
                if not line.strip():
                    continue
                feat = json.loads(line)
                code = feat.get("properties", {}).get(code_field)
                if code in (None, "", " ", 0, "0"):
                    continue
                code = str(code).strip()
                if code in keep:
                    acc.setdefault(code, []).extend(
                        shape_parts(feat.get("geometry")))

            for code, parts in sorted(acc.items()):
                row = keep[code]
                # Mirror the bbox rule in Accumulator.add. Shipping an
                # artifact as an outline would put back exactly the extent the
                # bbox excluded, leaving the two artifacts inconsistent.
                real = [r for a, _c, r in parts if a >= MIN_PART_AREA_SQ_DEG]
                if not real:
                    real = [r for _a, _c, r in parts]
                    full = sum(a for a, _c, _r in parts)
                else:
                    full = sum(a for a, _c, _r in parts
                               if a >= MIN_PART_AREA_SQ_DEG)
                geom, kept = simplify_geometry(real, tolerance)
                if geom is None:
                    collapsed.append(f"{level}/{code}")
                    continue

                # The point in the CSV must land inside the shape shipped
                # beside it, or the two artifacts disagree about the same area.
                # Sundaragada (LGD 373) is the case that matters: its point is
                # grid-derived against a concave outline, and simplification
                # moves the edge past it. Re-derive from the simplified shape
                # so the two agree by construction rather than by luck.
                polys = ([geom["coordinates"]] if geom["type"] == "Polygon"
                         else geom["coordinates"])
                pt = (float(row["longitude"]), float(row["latitude"]))
                if not any(point_in_polygon(pt, rings) for rings in polys):
                    acc = Accumulator()
                    acc.add(geom)
                    res = acc.result()
                    if res is None:
                        outside.append(f"{level}/{code}")
                    else:
                        nlon, nlat, _box, nmethod = res
                        row["latitude"] = f"{nlat:.6f}"
                        row["longitude"] = f"{nlon:.6f}"
                        row["point_method"] = f"{nmethod}:simplified"
                        # Keep the inheritance source in step. Descendants copy
                        # their ancestor's point verbatim, so a correction made
                        # here has to reach them or the file contradicts itself:
                        # 62 Blocks under Sundaragada (LGD 373) kept the
                        # pre-correction coordinate when this was missed.
                        rk = (row["code_scheme"], code, level)
                        if rk in resolved:
                            _la, _lo, rbox, rorigin = resolved[rk]
                            resolved[rk] = (nlat, nlon, rbox, rorigin)
                        repointed.append(f"{level}/{code}")
                        if not any(point_in_polygon((nlon, nlat), rings)
                                   for rings in polys):
                            outside.append(f"{level}/{code}")

                if full > 0:
                    worst_area = max(worst_area, abs(kept - full) / full)
                xs = [c[0] for rings in polys for r in rings for c in r]
                ys = [c[1] for rings in polys for r in rings for c in r]
                worst_bbox = max(
                    worst_bbox,
                    abs(min(xs) - float(row["bbox_west"])),
                    abs(min(ys) - float(row["bbox_south"])),
                    abs(max(xs) - float(row["bbox_east"])),
                    abs(max(ys) - float(row["bbox_north"])))

                fout.write(json.dumps({
                    "type": "Feature",
                    "properties": {"code_scheme": row["code_scheme"],
                                   "area_code": code,
                                   "area_level": level,
                                   "area_name": row["area_name"],
                                   "geometry_source": stem},
                    "geometry": geom,
                }, separators=(",", ":")) + "\n")
                row["has_polygon"] = "true"
                written += 1

    size_mb = out.stat().st_size / 1e6
    log(f"areas.geojsonl       {written:,} polygons, {size_mb:,.1f} MB, "
        f"tolerance {tolerance} deg")
    if collapsed:
        log(f"  {len(collapsed):,} areas collapsed below tolerance: "
            f"{', '.join(collapsed[:5])}"
            + (" ..." if len(collapsed) > 5 else ""))
    if repointed:
        log(f"  {len(repointed):,} points re-derived from the simplified "
            f"outline: {', '.join(repointed[:5])}"
            + (" ..." if len(repointed) > 5 else ""))
    if outside:
        log(f"  {len(outside):,} stored points STILL fall outside their "
            f"outline: {', '.join(outside[:5])}"
            + (" ..." if len(outside) > 5 else ""))
    return {
        "file": "areas.geojsonl",
        "features": written,
        "size_bytes": out.stat().st_size,
        "simplify_tolerance_deg": tolerance,
        "join_key": ["code_scheme", "area_code", "area_level"],
        "winding": "RFC 7946: exterior counterclockwise, holes clockwise",
        "collapsed_below_tolerance": collapsed,
        "points_rederived_from_simplified_outline": repointed,
        "point_outside_simplified_outline": outside,
        "max_area_error_fraction": round(worst_area, 6),
        "max_bbox_shift_deg": round(worst_bbox, 6),
        "note": "Simplified outlines. Use --with-geometry for full fidelity.",
    }


def write_geometry(conf, gazdir, cache, rows):
    """Emit boundaries.geojsonl for areas that joined to a real polygon."""
    wanted = {}
    for r in rows:
        if r["latitude"] and not r["point_method"].startswith("inherited"):
            if r["area_level"] in conf["layers"]:
                wanted.setdefault(r["area_level"], {})[r["area_code"]] = r["area_name"]

    out = gazdir / "boundaries.geojsonl"
    written = 0
    with open(out, "w", encoding="utf-8") as fout:
        for level, (_tag, stem, code_field, _name) in conf["layers"].items():
            keep = wanted.get(level) or {}
            if not keep:
                continue
            path = cache / f"{stem}.geojsonl"
            for line in open(path, encoding="utf-8", errors="replace"):
                if not line.strip():
                    continue
                feat = json.loads(line)
                code = feat.get("properties", {}).get(code_field)
                if code in (None, "", " ", 0, "0"):
                    continue
                code = str(code).strip()
                if code not in keep:
                    continue
                # Every feature for a code is kept, so an area published as two
                # polygons stays complete for point-in-polygon use.
                fout.write(json.dumps({
                    "type": "Feature",
                    "properties": {"code_scheme": "LGD", "area_code": code,
                                   "area_level": level, "area_name": keep[code],
                                   "geometry_source": stem},
                    "geometry": feat.get("geometry"),
                }, separators=(",", ":")) + "\n")
                written += 1
    log(f"boundaries.geojsonl  {written:,} features, "
        f"{out.stat().st_size / 1e6:,.1f} MB")


def update_manifest(source, conf, gazdir, stats, vintages, dates, drift,
                    inherit, containment, polygons):
    path = gazdir / "manifest.json"
    m = json.loads(path.read_text()) if path.exists() else {}
    m["geometry_populated"] = True
    m["geometry"] = {
        "source": source,
        "source_label": conf["label"],
        "licence": conf["licence"],
        "upstream": conf["upstream"],
        "note": conf["note"],
        "mirror": f"https://github.com/{REPO}/releases",
        "layers": {lvl: {"asset": vintages[lvl], "vintage": dates.get(lvl, "")}
                   for lvl in vintages},
        "join_key": "LGD code carried in the boundary layer; names are never "
                    "used to join",
        "point_definition": "Area-weighted centroid computed on lon/lat as a "
                            "plane, holes subtracted. If that point falls "
                            "outside the shape, an interior point near the "
                            "largest part's centroid is substituted. See "
                            "point_method per row.",
        "parent_inheritance": inherit,
        "coverage": stats,
        "name_drift_count": len(drift),
        "parent_containment_check": containment,
        "polygons": polygons,
    }
    path.write_text(json.dumps(m, indent=2) + "\n")


def report(areas_csv, stats, drift, conf, containment, polygons):
    print(f"\n{areas_csv}")
    print(f"\n{'level':<12}{'rows':>8}{'joined':>9}{'inherited':>11}"
          f"{'empty':>8}{'joined %':>10}")
    for level in LEVEL_ORDER + [k for k in stats if k not in LEVEL_ORDER]:
        s = stats.get(level)
        if not s:
            continue
        pct = 100.0 * s["matched"] / s["total"] if s["total"] else 0.0
        print(f"{level:<12}{s['total']:>8,}{s['matched']:>9,}"
              f"{s['inherited']:>11,}{s['empty']:>8,}{pct:>9.1f}%")

    c = containment
    if c["checked"]:
        print(f"\nparent containment: {c['checked'] - c['violations']:,}/"
              f"{c['checked']:,} joined points fall inside their parent's box")
        for o in c["outliers"][:8]:
            print(f"  {o['level']:<9}LGD {o['area_code']:<6}{o['area_name'][:24]:<26}"
                  f"~{o['km_outside_parent_bbox']:,} km outside {o['parent_name'][:20]}")
        if c["violations"]:
            print("  Reported, not corrected: these are either upstream polygon "
                  "errors or\n  genuine exclaves, and the two are "
                  "indistinguishable automatically.")
            print("  Full list in manifest.json under "
                  "geometry.parent_containment_check.")

    if drift:
        print(f"\n{len(drift)} areas joined on a stable code but carry a "
              f"different name upstream:")
        for level, code, ours, theirs in drift[:8]:
            print(f"  {level:<9} LGD {code:<6} ours={ours!r} boundary={theirs!r}")
        if len(drift) > 8:
            print(f"  ... and {len(drift) - 8} more")
        print("  These are why the join uses codes, not names.")

    if polygons.get("features"):
        print(f"\nareas.geojsonl: {polygons['features']:,} simplified outlines, "
              f"{polygons['size_bytes'] / 1e6:,.1f} MB")
        print(f"  tolerance {polygons['simplify_tolerance_deg']} deg "
              f"(~{polygons['simplify_tolerance_deg'] * 111000:,.0f} m), "
              f"max area error {polygons['max_area_error_fraction'] * 100:.3f}%, "
              f"max box shift {polygons['max_bbox_shift_deg'] * 111000:,.0f} m")
        if polygons["points_rederived_from_simplified_outline"]:
            print(f"  {len(polygons['points_rederived_from_simplified_outline'])} "
                  f"point(s) re-derived so the CSV agrees with the outline")
        bad = (len(polygons["collapsed_below_tolerance"])
               + len(polygons["point_outside_simplified_outline"]))
        print(f"  has_polygon=true on {polygons['features']:,} rows; "
              + ("no point/outline disagreements" if not bad
                 else f"{bad} anomalies, see manifest.json"))

    empty_levels = [lvl for lvl, s in stats.items()
                    if s["total"] and not s["matched"] and lvl != "Country"]
    if empty_levels:
        print(f"\nNo geometry joined for: {', '.join(sorted(empty_levels))}")
        print(f"  {conf['note']}")


def main():
    p = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--source", choices=sorted(SOURCES), default="soi",
                   help="boundary source (default: soi, the openly licensed one)")
    p.add_argument("--areas", default="data/areas/latest",
                   help="directory holding areas.csv from stage 1")
    p.add_argument("--cache", default="data/areas/.cache",
                   help="where boundary downloads are kept between runs")
    p.add_argument("--simplify", type=float, default=SIMPLIFY_DEG,
                   metavar="DEG",
                   help=f"areas.geojsonl simplification tolerance in degrees "
                        f"(default: {SIMPLIFY_DEG}, ~110 m); 0 keeps every vertex")
    p.add_argument("--with-geometry", action="store_true",
                   help="also write boundaries.geojsonl at full fidelity")
    p.add_argument("--no-inherit", action="store_true",
                   help="leave unjoinable levels empty instead of using a parent point")
    a = p.parse_args()
    if a.simplify < 0:
        sys.exit("--simplify must be >= 0")
    run(a.source, a.areas, a.cache, a.with_geometry, not a.no_inherit,
        a.simplify)


if __name__ == "__main__":
    main()
