"""Shared element builders for the .excalidraw diagrams in this folder."""
BASE = dict(angle=0, fillStyle="solid", strokeWidth=2, strokeStyle="solid", roughness=1,
            opacity=100, groupIds=[], frameId=None, seed=12345, version=1,
            versionNonce=11111, isDeleted=False, boundElements=None,
            updated=1762300000000, link=None, locked=False)

GREY, BLUE, GREEN, RED = "#868e96", "#1971c2", "#2f9e44", "#e03131"
BG_BLUE, BG_GREEN, BG_YEL, BG_GREY, BG_RED = "#a5d8ff", "#b2f2bb", "#ffec99", "#e9ecef", "#ffc9c9"


class Canvas:
    def __init__(self):
        self.els, self._n = [], 1000

    def _id(self):
        self._n += 1
        return "el%04d" % self._n

    def text(self, x, y, s, size=16, color="#1e1e1e", align="left"):
        lines = s.split("\n")
        t = dict(BASE); t.update(
            id=self._id(), type="text", x=x, y=y,
            width=max(len(l) for l in lines) * size * 0.55, height=len(lines) * size * 1.25,
            strokeColor=color, backgroundColor="transparent", fontSize=size, fontFamily=2,
            text=s, originalText=s, textAlign=align, verticalAlign="top",
            containerId=None, lineHeight=1.25, autoResize=True)
        self.els.append(t); return t

    def box(self, x, y, w, h, label, bg="transparent", stroke="#1e1e1e",
            style="solid", size=16, round=True):
        rid, tid = self._id(), self._id()
        r = dict(BASE); r.update(
            id=rid, type="rectangle", x=x, y=y, width=w, height=h,
            strokeColor=stroke, backgroundColor=bg, strokeStyle=style,
            roundness={"type": 3} if round else None,
            boundElements=[{"type": "text", "id": tid}] if label else None)
        self.els.append(r)
        if label:
            lines = label.split("\n"); th = len(lines) * size * 1.25
            t = dict(BASE); t.update(
                id=tid, type="text", x=x + 8, y=y + (h - th) / 2, width=w - 16, height=th,
                strokeColor=stroke, backgroundColor="transparent", fontSize=size,
                fontFamily=2, text=label, originalText=label, textAlign="center",
                verticalAlign="middle", containerId=rid, lineHeight=1.25, autoResize=False)
            self.els.append(t)
        return r

    def arrow(self, x1, y1, x2, y2, color="#1e1e1e", style="solid", head="arrow", start=None):
        a = dict(BASE); a.update(
            id=self._id(), type="arrow", x=x1, y=y1,
            width=abs(x2 - x1), height=abs(y2 - y1),
            strokeColor=color, backgroundColor="transparent", strokeStyle=style,
            points=[[0, 0], [x2 - x1, y2 - y1]], lastCommittedPoint=None,
            startBinding=None, endBinding=None, startArrowhead=start, endArrowhead=head,
            roundness={"type": 2}, elbowed=False)
        self.els.append(a); return a

    def doc(self):
        return dict(type="excalidraw", version=2, source="https://excalidraw.com",
                    elements=self.els,
                    appState={"gridSize": None, "viewBackgroundColor": "#ffffff"},
                    files={})
