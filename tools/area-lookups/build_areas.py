#!/usr/bin/env python3
"""Build a dated LGD area lookup table CSV for OpenAgriNet coverageAreas resolution.

Pulls LGD (Local Government Directory) snapshots and flattens them into one CSV
shaped like AdministrativeAreaReference, so a coded coverageAreas entry can be
resolved by lookup instead of a live API call.

    (codeScheme, areaCode, areaLevel) -> areaName, parent, census codes

Re-run to refresh. Each run writes its own dated directory and never touches an
earlier one, so a resource published against an older snapshot still resolves.

Geometry is NOT populated here. The lat/lon, bbox and point_method columns are
written empty and filled by join_geometry.py, which joins published boundary
layers onto these codes. Splitting the stages keeps the code snapshot (refreshed
daily) independent of the boundary layers (refreshed every year or two).

Usage:
    python3 build_areas.py                    # latest available snapshot
    python3 build_areas.py --date 02Sep2026   # a specific snapshot
    python3 build_areas.py --out data/areas
    python3 build_areas.py --with-villages    # adds ~670k village rows

Stdlib only. Requires bsdtar (default `tar` on macOS) or 7z for .7z extraction.
"""

import argparse
import csv
import datetime as dt
import json
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

REPO = "ramSeraph/opendata"
TAG = "lgd-latest-extra1"
BASE = f"https://github.com/{REPO}/releases/download/{TAG}"
API = f"https://api.github.com/repos/{REPO}/releases/tags/{TAG}"

# LGD is the upstream authority; this mirror extracts it daily.
PROVENANCE = "https://lgdirectory.gov.in/ (via https://github.com/ramSeraph/opendata, GODL-India)"

# Components pulled by default. Villages are opt-in: 670k rows, rarely needed
# for coverage matching, and no geometry exists for them anyway.
CORE = ["states", "districts", "subdistricts", "pincode_villages"]
OPTIONAL = ["villages"]

OUT_COLUMNS = [
    "code_scheme",       # matches AdministrativeAreaReference.codeScheme
    "area_code",         # matches .areaCode
    "area_level",        # matches .areaLevel
    "area_name",         # matches .areaName
    "parent_scheme",     # fallback chain for unresolvable children
    "parent_code",
    "parent_level",      # required: LGD codes repeat across levels
    "census_2011_code",  # crosswalk to census-coded boundary files
    "snapshot_date",
    "source_url",
    # Everything below is written empty here and filled by join_geometry.py.
    "latitude",
    "longitude",
    "point_method",      # centroid | interior_grid | inherited:<level>
    "bbox_west",
    "bbox_south",
    "bbox_east",
    "bbox_north",
    "geometry_source",   # which boundary layer supplied the point
    "boundary_vintage",
    "has_polygon",       # is a real outline present in areas.geojsonl?
    "same_as",           # provenance only: canonical key an alias row mirrors
]


def log(msg):
    print(f"  {msg}", file=sys.stderr)


def fetch_json(url):
    req = urllib.request.Request(url, headers={"User-Agent": "oan-area-lookup"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r)


def latest_snapshot_date():
    """Newest date present across all core components.

    Components are not always published in lockstep, so take the newest date
    that every core component actually has. A half-published day would
    otherwise produce a area lookup table with a stale districts file silently mixed in.
    """
    try:
        assets = [a["name"] for a in fetch_json(API)["assets"]]
    except urllib.error.URLError as e:
        sys.exit(f"could not reach GitHub releases API: {e}")

    per_component = {}
    for name in assets:
        # e.g. "districts.02Sep2026.csv.7z"
        parts = name.split(".")
        if len(parts) < 3 or parts[0] not in CORE:
            continue
        try:
            parsed = dt.datetime.strptime(parts[1], "%d%b%Y").date()
        except ValueError:
            continue  # monthly archives use "Sep2026"; skip them
        per_component.setdefault(parts[0], set()).add(parsed)

    missing = [c for c in CORE if c not in per_component]
    if missing:
        sys.exit(f"no dated snapshots found for: {', '.join(missing)}")

    common = set.intersection(*per_component.values())
    if not common:
        sys.exit("no single date has all core components published")
    return max(common).strftime("%d%b%Y")


def extract(archive, dest):
    """Extract a .7z. bsdtar (macOS default tar) handles it; 7z is the fallback.

    GNU unzip cannot read these archives, which is why neither is attempted.
    """
    for cmd in (["tar", "-xf", str(archive), "-C", str(dest)],
                ["7z", "x", f"-o{dest}", "-y", str(archive)],
                ["7zz", "x", f"-o{dest}", "-y", str(archive)]):
        if shutil.which(cmd[0]) is None:
            continue
        if subprocess.run(cmd, capture_output=True).returncode == 0:
            return
    sys.exit("need bsdtar or 7z to extract .7z archives")


def fetch_to_file(url, dest, what, attempts=3):
    """Download url to dest, retrying transient network failures.

    A refresh pulls several large archives from a CDN, and a single dropped
    TLS connection would otherwise abort the whole run. HTTP status errors are
    not retried: a 404 means the asset genuinely is not published, and asking
    again will not change that.
    """
    req = urllib.request.Request(url, headers={"User-Agent": "oan-area-lookup"})
    for attempt in range(1, attempts + 1):
        try:
            with urllib.request.urlopen(req, timeout=300) as r, open(dest, "wb") as f:
                shutil.copyfileobj(r, f)
            return
        except urllib.error.HTTPError as e:
            sys.exit(f"{what} not available ({e.code}): {url}")
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            dest.unlink(missing_ok=True)   # never leave a partial file behind
            if attempt == attempts:
                sys.exit(f"{what}: download failed after {attempts} attempts "
                         f"({e}): {url}")
            wait = 2 ** attempt
            log(f"{what}: {e} - retrying in {wait}s "
                f"({attempt}/{attempts - 1} retries used)")
            time.sleep(wait)


def download_component(component, date, workdir):
    url = f"{BASE}/{component}.{date}.csv.7z"
    archive = workdir / f"{component}.7z"
    fetch_to_file(url, archive, f"{component} {date}")

    extract(archive, workdir)
    csvs = list(workdir.glob(f"{component}.{date}.csv"))
    if not csvs:
        sys.exit(f"{component}: no CSV inside archive")
    log(f"{component:<18} {sum(1 for _ in open(csvs[0], encoding='utf-8', errors='replace')) - 1:>7,} rows")
    return csvs[0], url


def norm(header):
    """Reduce a header to letters and digits only, lowercased.

    LGD spells the same column differently in every file: "Sub-district Code",
    "Sub-District Code" and "SubDistrict Code" all occur, as do
    "District Name(In English)" and "District Name (In English)". Normalising
    both sides makes lookups survive that, and survive future drift.
    """
    return "".join(ch for ch in header.lower() if ch.isalnum())


def read_csv(path):
    with open(path, encoding="utf-8", errors="replace", newline="") as f:
        for row in csv.DictReader(f):
            yield {norm(k): (v or "").strip() for k, v in row.items() if k}


def pick(row, *candidates):
    """First non-empty matching column, matched on normalised names."""
    for c in candidates:
        v = row.get(norm(c), "")
        if v:
            return v
    return ""


# Child level -> parent level. Recorded per row because an LGD code is only
# unique within its level: code 35 is simultaneously the State Andaman And
# Nicobar Islands, the District Kapurthala and the Block Baramulla. Without
# this column a consumer walking parent_code cannot tell which one is meant,
# and 765 of the codes in this file are ambiguous that way.
PARENT_LEVEL = {"State": "Country", "District": "State", "Block": "District",
                "PostalCode": "District", "Village": "Block"}


# ISO-3166-2:IN -> LGD state code.
#
# Publishers reach for ISO codes because they are short and internationally
# recognised: every example in the OpenAgriNet schema packs that names a State
# uses ISO-3166-2, never LGD. The lookup matches on
# (code_scheme, area_code, area_level), so without these rows an ISO reference
# matches nothing at all and the plugin publishes no geometry for it.
#
# ISO assigns codes to States and Union Territories only, so this is the entire
# crosswalk. There is nothing below State to translate.
#
# Verified against ISO 3166-2:IN as of 2023-11-23, the most recent amendment.
# 28 states + 8 union territories = 36.
ISO_3166_2 = {
    "IN-AN": "35", "IN-AP": "28", "IN-AR": "12", "IN-AS": "18",
    "IN-BR": "10", "IN-CG": "22", "IN-CH": "4",  "IN-DH": "38",
    "IN-DL": "7",  "IN-GA": "30", "IN-GJ": "24", "IN-HP": "2",
    "IN-HR": "6",  "IN-JH": "20", "IN-JK": "1",  "IN-KA": "29",
    "IN-KL": "32", "IN-LA": "37", "IN-LD": "31", "IN-MH": "27",
    "IN-ML": "17", "IN-MN": "14", "IN-MP": "23", "IN-MZ": "15",
    "IN-NL": "13", "IN-OD": "21", "IN-PB": "3",  "IN-PY": "34",
    "IN-RJ": "8",  "IN-SK": "11", "IN-TN": "33", "IN-TR": "16",
    "IN-TS": "36", "IN-UK": "5",  "IN-UP": "9",  "IN-WB": "19",
}

# Codes ISO has withdrawn, carried because publishers still send them. This
# repository is itself an example: two of its files use IN-TG, retired in
# favour of IN-TS in November 2023, and one uses IN-TS, so a snapshot that
# accepted only current codes would fail on the specs' own examples.
#
# Each points at its current code rather than straight at LGD. That extra hop
# is what records the row as superseded rather than merely an alternative
# spelling, and it costs the plugin nothing, because stage 2 copies geometry
# onto every one of these rows.
ISO_3166_2_WITHDRAWN = {
    "IN-TG": "IN-TS",   # Telangana, withdrawn 2023-11-23
    "IN-UT": "IN-UK",   # Uttarakhand, withdrawn 2023-11-23
    "IN-UL": "IN-UK",   # Uttarakhand as Uttaranchal, withdrawn 2011-12-13
    "IN-OR": "IN-OD",   # Odisha as Orissa, withdrawn 2014-10-30
    "IN-CT": "IN-CG",   # Chhattisgarh, withdrawn 2002-08-20
    "IN-DN": "IN-DH",   # Dadra and Nagar Haveli, merged 2020-11-11
    "IN-DD": "IN-DH",   # Daman and Diu, merged 2020-11-11
}


def row_out(scheme, code, level, name, parent_scheme="", parent_code="",
            census="", date="", url="", same_as=""):
    return {
        "code_scheme": scheme,
        "area_code": code,
        "area_level": level,
        "area_name": name,
        "parent_scheme": parent_scheme,
        "parent_code": parent_code,
        "parent_level": PARENT_LEVEL.get(level, ""),
        "census_2011_code": census,
        "snapshot_date": date,
        "source_url": url,
        "latitude": "",
        "longitude": "",
        "point_method": "",
        "bbox_west": "",
        "bbox_south": "",
        "bbox_east": "",
        "bbox_north": "",
        "geometry_source": "",
        "boundary_vintage": "",
        "has_polygon": "",
        "same_as": same_as,
    }


def build(date, outdir, with_villages):
    components = CORE + (OPTIONAL if with_villages else [])
    rows = []
    sources = {}

    with tempfile.TemporaryDirectory() as tmp:
        workdir = Path(tmp)
        log(f"snapshot {date}")
        files = {}
        for c in components:
            files[c], sources[c] = download_component(c, date, workdir)

        # ISO-3166-1: one row, so the country code in your examples resolves too.
        rows.append(row_out("ISO-3166-1", "IN", "Country", "India",
                            date=date, url="static"))

        # --- states ---
        state_names = {}
        for r in read_csv(files["states"]):
            code = pick(r, "State Code")
            if not code:
                continue
            name = pick(r, "State Name (In English)", "State Name")
            state_names[code] = name
            rows.append(row_out(
                "LGD", code, "State", name,
                "ISO-3166-1", "IN",
                pick(r, "Census 2011 Code"), date, sources["states"]))

        # --- ISO-3166-2 alias rows ---
        #
        # Names come from LGD rather than from ISO so that both spellings of a
        # state agree with the rest of the file. Where the mapped state is
        # missing from the snapshot the row is dropped and reported, because a
        # code pointing at nothing is worse than a code that is absent.
        missing = []
        for iso, lgd in sorted(ISO_3166_2.items()):
            if lgd not in state_names:
                missing.append(f"{iso}->LGD/{lgd}")
                continue
            rows.append(row_out(
                "ISO-3166-2", iso, "State", state_names[lgd],
                "ISO-3166-1", "IN", "", date, "static",
                same_as=f"LGD/{lgd}/State"))
        for old_code, cur in sorted(ISO_3166_2_WITHDRAWN.items()):
            lgd = ISO_3166_2.get(cur, "")
            if lgd not in state_names:
                missing.append(f"{old_code}->{cur}")
                continue
            rows.append(row_out(
                "ISO-3166-2", old_code, "State", state_names[lgd],
                "ISO-3166-1", "IN", "", date, "static",
                same_as=f"ISO-3166-2/{cur}/State"))
        log(f"ISO-3166-2           {len(ISO_3166_2)} current + "
            f"{len(ISO_3166_2_WITHDRAWN)} withdrawn codes")
        if missing:
            log(f"  WARNING: {len(missing)} ISO codes map to states absent "
                f"from this snapshot: {', '.join(missing)}")

        # --- districts ---
        for r in read_csv(files["districts"]):
            code = pick(r, "District Code")
            if not code:
                continue
            rows.append(row_out(
                "LGD", code, "District",
                pick(r, "District Name(In English)", "District Name"),
                "LGD", pick(r, "State Code"),
                pick(r, "Census 2011 Code"), date, sources["districts"]))

        # --- subdistricts (maps to areaLevel "Block") ---
        for r in read_csv(files["subdistricts"]):
            code = pick(r, "Sub-district Code")
            if not code:
                continue
            rows.append(row_out(
                "LGD", code, "Block",
                pick(r, "Sub-district Name"),
                "LGD", pick(r, "District Code"),
                pick(r, "Census 2011 Code"), date, sources["subdistricts"]))

        # --- villages (opt-in) ---
        if with_villages:
            for r in read_csv(files["villages"]):
                code = pick(r, "Village Code")
                if not code:
                    continue
                rows.append(row_out(
                    "LGD", code, "Village",
                    pick(r, "Village Name (In English)"),
                    "LGD", pick(r, "Sub-District Code"),
                    pick(r, "Census 2011 Code"), date, sources["villages"]))

        # --- pincodes ---
        # pincode_villages is village-grained: many rows share one pincode.
        # Collapse to one row per pincode and keep the district as its parent,
        # which is the usable fallback since pincodes have no boundary data.
        pins = {}
        for r in read_csv(files["pincode_villages"]):
            pin = pick(r, "Pincode")
            if not pin or not pin.isdigit():
                continue
            if pin not in pins:
                pins[pin] = (pick(r, "District Code"), pick(r, "District Name"))
        for pin, (dcode, dname) in sorted(pins.items()):
            rows.append(row_out(
                "IN-PIN", pin, "PostalCode", dname,
                "LGD", dcode, "", date, sources["pincode_villages"]))
        log(f"{'pincodes (unique)':<18} {len(pins):>7,} rows")

    # A component that parsed to zero rows means LGD renamed a column, not that
    # the level is genuinely empty. Fail rather than publish a area lookup table with a
    # silently missing level.
    by_level = {}
    for r in rows:
        by_level[r["area_level"]] = by_level.get(r["area_level"], 0) + 1

    expected = ["Country", "State", "District", "Block", "PostalCode"]
    if with_villages:
        expected.append("Village")
    empty = [lvl for lvl in expected if not by_level.get(lvl)]
    if empty:
        sys.exit(f"no rows produced for: {', '.join(empty)} — "
                 "LGD column names likely changed; check pick() candidates")

    # --- write ---
    stamp = dt.datetime.strptime(date, "%d%b%Y").date().isoformat()
    target = Path(outdir) / f"lgd-{stamp}"
    target.mkdir(parents=True, exist_ok=True)
    out = target / "areas.csv"

    with open(out, "w", encoding="utf-8", newline="") as f:
        w = csv.DictWriter(f, fieldnames=OUT_COLUMNS)
        w.writeheader()
        w.writerows(rows)

    manifest = {
        "snapshot_date": stamp,
        "lgd_snapshot": date,
        "generated_by": "build_areas.py",
        "provenance": PROVENANCE,
        "components": sources,
        "row_count": len(rows),
        "rows_by_level": by_level,
        "geometry_populated": False,
        "notes": "Codes only. Geometry columns are filled by join_geometry.py, "
                 "which adds a `geometry` block to this manifest recording the "
                 "boundary source, its licence and the coverage achieved.",
    }
    (target / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")

    # Stable path for consumers that just want current data.
    latest = Path(outdir) / "latest"
    if latest.is_symlink() or latest.exists():
        latest.unlink()
    latest.symlink_to(target.name)

    print(f"\n{out}  ({len(rows):,} rows)")
    for level, n in sorted(by_level.items(), key=lambda kv: -kv[1]):
        print(f"  {level:<12} {n:>7,}")
    print(f"\nmanifest: {target / 'manifest.json'}")
    print(f"latest:   {latest} -> {target.name}")


def main():
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--date", help="LGD snapshot, e.g. 02Sep2026 (default: latest)")
    p.add_argument("--out", default="data/areas", help="output directory")
    p.add_argument("--with-villages", action="store_true",
                   help="include ~670k village rows")
    a = p.parse_args()
    build(a.date or latest_snapshot_date(), a.out, a.with_villages)


if __name__ == "__main__":
    main()
