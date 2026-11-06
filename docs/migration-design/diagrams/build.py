#!/usr/bin/env python3
"""Build the architecture diagram, then render SVG + PNG.

    python3 diagrams/build.py        # from the migration-design folder
"""
import json, os, subprocess, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from elements import Canvas, GREY, BLUE, GREEN, RED, BG_BLUE, BG_GREEN, BG_YEL, BG_GREY, BG_RED
import render

HERE = os.path.dirname(os.path.abspath(__file__))


def approach():
    """The one diagram. Follows a farmer's question end to end, both paths."""
    c = Canvas()
    c.text(60, -112, "How a farmer's question reaches an answer", size=28)
    c.text(60, -70, "The same question, the same answer. Only the middle changes, one capability at a time.",
           size=15, color=GREY)

    c.box(330, 0, 460, 90, 'A farmer asks\n"Will it rain in my village tomorrow?"', size=15)
    c.arrow(560, 90, 560, 155)

    c.box(330, 155, 460, 80, "Bharat Vistaar's assistant\nworks out it needs a weather forecast", size=14)
    c.arrow(560, 235, 560, 320)
    c.text(585, 250, "sends the same Beckn request it has always sent.\nNot one byte of it changed.",
           size=13, color=GREY)

    c.box(60, 320, 1000, 365, "", bg=BG_GREY, stroke=BLUE, style="dashed", round=False)
    c.text(88, 340, "THE V1 ADAPTER", size=18, color=BLUE)
    c.text(88, 366, "the one new piece", size=13, color=GREY)

    c.box(1100, 320, 580, 392, "", bg="#fff9db", stroke=BLUE, round=True)
    c.text(1126, 344, "THE ONE NEW PIECE", size=16, color=BLUE)

    c.text(1126, 378, "What it is", size=14, color="#8a6d00")
    c.text(1126, 400, "A V1-facing protocol adapter. It is a separate", size=13)
    c.text(1126, 422, "instance of beckn-onix, the adapter V2 already", size=13)
    c.text(1126, 444, "runs, configured for V1.", size=13)
    c.text(1126, 470, "Same runtime and deploy path as V2, but its own", size=13, color=GREY)
    c.text(1126, 492, "failure domain. It holds no state.", size=13, color=GREY)

    c.text(1126, 528, "How it gets into the path", size=14, color="#8a6d00")
    c.text(1126, 550, "It takes over the address Bharat Vistaar already", size=13)
    c.text(1126, 572, "calls, and the old service moves behind it. Either", size=13)
    c.text(1126, 594, "the address is repointed, or one setting changes", size=13)
    c.text(1126, 616, "in their environment. Agreed once, at the start.", size=13)
    c.text(1126, 642, "After that, moving a capability needs nothing", size=13, color=GREY)
    c.text(1126, 664, "from Bharat Vistaar at all.", size=13, color=GREY)

    c.box(380, 390, 360, 70, "Has this capability moved yet?", bg=BG_YEL, size=16)
    c.arrow(440, 460, 280, 520, color=GREEN)
    c.arrow(680, 460, 840, 520, color=BLUE)

    c.box(130, 520, 300, 100, "No, not yet\n\nhand it to the old service,\nuntouched", bg=BG_GREEN, size=14)
    c.box(690, 520, 300, 100, "Yes\n\ncall the provider and build\nthe reply ourselves", bg=BG_BLUE, size=14)
    c.text(133, 630, "the default, and what anything\nwe do not recognise gets", size=12, color=GREY)

    c.arrow(395, 620, 395, 730, color=GREEN)
    c.box(245, 730, 300, 80, "The old service\nstill running, shrinking as we go", bg=BG_GREY, size=14)

    c.arrow(395, 810, 480, 860, color=GREEN)
    c.arrow(840, 620, 700, 860, color=BLUE)
    c.box(390, 860, 380, 90, "Mausamgram\nthe weather provider, same either way", bg=BG_GREEN, size=14)

    c.arrow(580, 950, 580, 1020)
    c.box(330, 1020, 500, 80, "The same answer, byte for byte\nwhichever way it travelled", bg=BG_GREEN, size=15)
    c.arrow(580, 1100, 580, 1170)
    c.box(330, 1170, 500, 80, "The farmer gets the forecast\nand cannot tell which way it went", size=15)

    c.box(1100, 762, 580, 238, "", bg=BG_GREEN, stroke=GREEN, round=True)
    c.text(1126, 786, "WHY THIS IS SAFE", size=16, color=GREEN)
    c.text(1126, 820, "Every capability starts on the left path, so on", size=13)
    c.text(1126, 842, "day one nothing has changed for anyone.", size=13)
    c.text(1126, 876, "We move them to the right path one at a time, and", size=13)
    c.text(1126, 898, "only after the two answers match byte for byte.", size=13)
    c.text(1126, 932, "If one looks wrong, we change a single line of", size=13)
    c.text(1126, 954, "config and it is back on the left path in seconds.", size=13)
    return c.doc()


def sequence():
    """Sequence diagram: one request down both paths."""
    c = Canvas()
    X = {"agent": 560, "lane": 950, "old": 1340, "prov": 1730}
    TOP, BOT = 56, 860

    c.text(60, -112, "What happens to one request", size=28)
    c.text(60, -70, "The top half is a capability we have not moved. The bottom half is one we have. "
                    "The farmer's side is identical in both.", size=15, color=GREY)

    # bands first, so everything else draws on top
    c.box(60, 96, 1800, 374, "", bg=BG_GREEN, stroke=GREEN, style="dashed", round=False)
    c.box(60, 500, 1800, 346, "", bg=BG_BLUE, stroke=BLUE, style="dashed", round=False)

    for k in X:
        c.arrow(X[k], TOP, X[k], BOT, color=GREY, style="dashed", head=None)

    for k, label in (("agent", "Bharat Vistaar\nagent"), ("lane", "The V1 Adapter"),
                     ("old", "The old service"), ("prov", "Mausamgram\nthe provider")):
        c.box(X[k] - 140, 0, 280, 56, label, bg=BG_GREY, size=14)

    def msg(a, b, y, label, color, style="solid"):
        c.arrow(X[a], y, X[b], y, color=color, style=style)
        c.text(min(X[a], X[b]) + 24, y - 26, label, size=13, color=color)

    c.text(84, 112, "FORWARD  ·  the default, and every capability on day one", size=15, color=GREEN)
    msg("agent", "lane", 182, "POST /mobility/search", GREEN)
    msg("lane", "old", 240, "not moved yet, so forward. Same bytes.", GREEN)
    msg("old", "prov", 298, "the old service calls the provider", GREEN)
    msg("prov", "old", 356, "provider data", GREEN, "dashed")
    msg("old", "lane", 414, "V1 reply", GREEN, "dashed")
    c.arrow(X["lane"], 452, X["agent"], 452, color=GREEN, style="dashed")
    c.text(584, 426, "streamed back byte for byte", size=13, color=GREEN)

    c.text(84, 516, "ANSWER  ·  a capability we have moved", size=15, color=BLUE)
    msg("agent", "lane", 586, "POST /mobility/search  ·  identical request", BLUE)
    c.box(970, 614, 300, 40, "already moved", bg=BG_YEL, size=13)
    msg("lane", "prov", 700, "it calls the provider itself", BLUE)
    msg("prov", "lane", 758, "the same provider data", BLUE, "dashed")
    c.arrow(X["lane"], 816, X["agent"], 816, color=BLUE, style="dashed")
    c.text(584, 790, "V1 reply it built, byte-identical", size=13, color=BLUE)
    c.text(1360, 578, "never touched", size=13, color=GREY)

    return c.doc()


for name, fn in (("approach", approach), ("request-path", sequence)):
    stem = os.path.join(HERE, name)
    json.dump(fn(), open(stem + ".excalidraw", "w"), indent=2, ensure_ascii=False)
    open(stem + ".svg", "w").write(render.render(json.load(open(stem + ".excalidraw"))))
    subprocess.run(["rsvg-convert", "-z", "2", "-b", "white",
                    "-o", stem + ".png", stem + ".svg"], check=True)
    print(f"{name}: excalidraw + svg + png")
