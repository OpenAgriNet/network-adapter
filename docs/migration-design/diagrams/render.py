#!/usr/bin/env python3
"""Render a .excalidraw file to SVG.

Covers only the element types used by the diagrams here (rectangle, text,
arrow). Deterministic and offline -- no headless browser. Clean strokes rather
than Excalidraw's hand-drawn roughness, which reads better at doc scale.
"""
import json, sys, math, html

PAD = 32
DASH = {"solid": None, "dashed": "10 8", "dotted": "2 7"}

def esc(s): return html.escape(s, quote=True)

def bounds(els):
    xs, ys = [], []
    for e in els:
        xs += [e["x"], e["x"] + e["width"]]
        ys += [e["y"], e["y"] + e["height"]]
    return min(xs), min(ys), max(xs), max(ys)

def render(doc):
    els = [e for e in doc["elements"] if not e.get("isDeleted")]
    x0, y0, x1, y1 = bounds(els)
    W, H = (x1 - x0) + 2 * PAD, (y1 - y0) + 2 * PAD
    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{W:.0f}" height="{H:.0f}" '
        f'viewBox="0 0 {W:.0f} {H:.0f}" font-family="Helvetica, Arial, sans-serif">',
        f'<rect width="{W:.0f}" height="{H:.0f}" fill="{doc["appState"].get("viewBackgroundColor","#ffffff")}"/>',
        f'<g transform="translate({PAD - x0:.1f},{PAD - y0:.1f})">',
    ]
    byid = {e["id"]: e for e in els}

    # rectangles first, then arrows, then text on top
    order = {"rectangle": 0, "ellipse": 0, "arrow": 1, "line": 1, "text": 2}
    for e in sorted(els, key=lambda e: order.get(e["type"], 1)):
        t = e["type"]
        sw, sc = e["strokeWidth"], e["strokeColor"]
        da = DASH.get(e["strokeStyle"])
        dash = f' stroke-dasharray="{da}"' if da else ""

        if t == "rectangle":
            bg = e["backgroundColor"]
            fill = "none" if bg == "transparent" else bg
            r = 12 if e.get("roundness") else 0
            out.append(
                f'<rect x="{e["x"]:.1f}" y="{e["y"]:.1f}" width="{e["width"]:.1f}" '
                f'height="{e["height"]:.1f}" rx="{r}" fill="{fill}" fill-opacity="0.55" '
                f'stroke="{sc}" stroke-width="{sw}"{dash}/>'
            )

        elif t == "arrow":
            pts = [(e["x"] + px, e["y"] + py) for px, py in e["points"]]
            d = "M " + " L ".join(f"{px:.1f},{py:.1f}" for px, py in pts)
            out.append(f'<path d="{d}" fill="none" stroke="{sc}" stroke-width="{sw}" '
                       f'stroke-linecap="round"{dash}/>')
            for head, (a, b) in (("endArrowhead", (pts[-2], pts[-1])),
                                 ("startArrowhead", (pts[1], pts[0]))):
                if not e.get(head):
                    continue
                ang = math.atan2(b[1] - a[1], b[0] - a[0])
                L, S = 14, math.radians(22)
                p1 = (b[0] - L * math.cos(ang - S), b[1] - L * math.sin(ang - S))
                p2 = (b[0] - L * math.cos(ang + S), b[1] - L * math.sin(ang + S))
                out.append(f'<path d="M {p1[0]:.1f},{p1[1]:.1f} L {b[0]:.1f},{b[1]:.1f} '
                           f'L {p2[0]:.1f},{p2[1]:.1f}" fill="none" stroke="{sc}" '
                           f'stroke-width="{sw}" stroke-linecap="round" stroke-linejoin="round"/>')

        elif t == "text":
            fs = e["fontSize"]
            lh = fs * e.get("lineHeight", 1.25)
            lines = e["text"].split("\n")
            cid = e.get("containerId")
            if cid and cid in byid:                      # bound label: centre in its box
                c = byid[cid]
                tx, anchor = c["x"] + c["width"] / 2, "middle"
                top = c["y"] + (c["height"] - len(lines) * lh) / 2
            else:
                tx, anchor = e["x"], "start"
                top = e["y"]
            # first line heavier when a box label has several lines
            for i, ln in enumerate(lines):
                if not ln.strip():
                    continue
                wgt = "600" if (cid and i == 0 and len(lines) > 1) else "400"
                out.append(
                    f'<text x="{tx:.1f}" y="{top + i * lh + lh * 0.76:.1f}" '
                    f'text-anchor="{anchor}" font-size="{fs}" font-weight="{wgt}" '
                    f'fill="{sc}">{esc(ln)}</text>'
                )

    out += ["</g>", "</svg>"]
    return "\n".join(out)

if __name__ == "__main__":
    src, dst = sys.argv[1], sys.argv[2]
    open(dst, "w").write(render(json.load(open(src))))
    print(f"{dst} written")
