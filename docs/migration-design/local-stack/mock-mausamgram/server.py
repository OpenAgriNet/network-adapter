"""Stand-in for the Mausamgram forecast provider.

Serves GET /get-daily?lat=&lon= behind Basic auth, the same shape the real
provider returns. The latitude selects a scenario, so a single V1 request can
drive any of the documented edge cases without touching this file.

    lat 1.1   gaps        fcstday2 absent, rain 0, a missing humidity,
                          an empty warning, a one-element wind array
    lat 2.2   late-only   only fcstday3 onward, which V1 refuses to answer
    lat 3.3   down        HTTP 500 on every attempt
    anything  happy       five complete days

Forecast dates are relative to today in IST, because fcstday1 is today for this
provider. Freezing them would make the V1 day labels look impossible to
reconstruct from a V2 answer, which carries dates and no slot number.
"""

import base64
import datetime
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse, parse_qs

IST = datetime.timezone(datetime.timedelta(hours=5, minutes=30))


def d(n):
    """The date fcstday(n+1) carries: today in IST, plus n days.

    Relative rather than fixed, because fcstday1 IS today for this provider and
    that is the only thing tying a forecast day to its slot number. A V1 answer
    labels the days Today, Day_2 ... and nothing on the wire carries that label
    but the date, so a harness with frozen dates would make the labels look
    unrecoverable when the real provider makes them recoverable.
    """
    return (datetime.datetime.now(IST).date() + datetime.timedelta(days=n)).isoformat()

USER = os.environ.get("MOCK_USER", "bv-local")
KEY = os.environ.get("MOCK_KEY", "local-dev-key")
PORT = int(os.environ.get("PORT", "9100"))


def day(date, rain, tmin, tmax, rhmin, rhmax, wspd, wind, warning, cloud):
    # wdir is the same bearing as `wind`, in degrees. The real provider sends
    # both, and which of the two survives a hop matters: V1 renders the compass
    # text, the V2 schema pack carries only the degrees.
    d = {"date": date, "rain": rain, "tmin": tmin, "tmax": tmax,
         "rhmax": rhmax, "wspd": wspd, "wind": wind[:2], "wdir": wind[2],
         "weather_warning": warning, "cloud_message": cloud}
    if rhmin is not None:
        d["rhmin"] = rhmin
    return d


# Abbreviation, the long form V1 renders, and the bearing in degrees. IMD's own
# wording for the long form -- "West-North Westerly", not "West-northwest".
WNW = ["WNW", "West-North Westerly", 292.5]
SE = ["SE", "South Easterly", 135.0]
NE_SHORT = ["NE", None, 45.0]


def happy():
    return {
        "fcstday1": day(d(0), 5, 18, 31, 40, 85, 3.2, WNW, "Thunderstorm likely", "Partly cloudy"),
        "fcstday2": day(d(1), 0, 19, 32, 42, 83, 2.8, SE, "", "Mainly clear"),
        "fcstday3": day(d(2), 12, 18, 29, 55, 90, 4.1, WNW, "Heavy rain warning", "Overcast"),
        "fcstday4": day(d(3), 2, 17, 30, 48, 88, 3.0, SE, "", "Cloudy"),
        "fcstday5": day(d(4), 0, 17, 31, 44, 86, 2.5, WNW, "", "Clear"),
    }


# fcstday2 is deliberately absent: V1 numbers the remaining days by slot, so the
# reply must read Today, Day_3, Day_4, Day_5 rather than renumbering them.
def gaps():
    return {
        "fcstday1": day(d(0), 0, 18, 31, 40, 85, 3.2, WNW, "", "Dry spell"),
        "fcstday3": day(d(2), 12, 18, 29, None, 90, 4.1, WNW, "Heavy rain warning", "Overcast"),
        "fcstday4": day(d(3), 2, 17, 30, 48, 88, 3.0, SE, "", "Cloudy, light showers"),
        # A wind array with no long form, which V1 renders as a bare "N/A".
        "fcstday5": day(d(4), 7, 17, 31, 44, 86, 2.5, NE_SHORT, "", ""),
    }


def late_only():
    h = happy()
    return {k: h[k] for k in ("fcstday3", "fcstday4", "fcstday5")}


SCENARIOS = {"1.1": ("gaps", gaps), "2.2": ("late-only", late_only)}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *a):
        print("mock-mausamgram " + fmt % a, flush=True)

    def send_json(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        url = urlparse(self.path)
        if url.path == "/healthz":
            return self.send_json(200, {"ok": True})
        if url.path != "/get-daily":
            return self.send_json(404, {"error": "not found"})

        expected = "Basic " + base64.b64encode(f"{USER}:{KEY}".encode()).decode()
        if self.headers.get("Authorization") != expected:
            self.log_message("rejected a request with bad or missing credentials")
            return self.send_json(401, {"error": "unauthorized"})

        q = parse_qs(url.query)
        lat = (q.get("lat") or [""])[0]
        lon = (q.get("lon") or [""])[0]

        if lat == "3.3":
            self.log_message("scenario down: returning 500 for lat=%s", lat)
            return self.send_json(500, {"error": "provider unavailable"})

        name, build = SCENARIOS.get(lat, ("happy", happy))
        days = build()
        self.log_message("scenario %s for lat=%s lon=%s", name, lat, lon)

        # The provider answers with the grid cell it resolved to, which is near
        # the coordinates asked for but not identical. V1 uses the resolved pair
        # in some fields and the asked-for pair in others.
        try:
            resolved = {"lat": round(float(lat) + 0.02, 4), "lon": round(float(lon) - 0.02, 4)}
        except ValueError:
            resolved = {"lat": lat, "lon": lon}

        self.send_json(200, {"location": resolved, **days})


if __name__ == "__main__":
    print(f"mock-mausamgram listening on :{PORT} as {USER}", flush=True)
    HTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
