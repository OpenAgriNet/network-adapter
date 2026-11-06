# Weather — Mausamgram

*[capabilities](README.md) · [how it works](../README.md) · [current state](../current-state.md)*

---

## 1. Status

**Proxied today. First candidate to move.**

It is the easiest capability in the set: no state, no OTP, nothing carried between calls, one upstream call per request — and a `WeatherObservation` / `mausamgram` pack already exists in V2, which is what makes it eligible to move at all.

Three things have to happen before it does: the traffic count, one curl to settle the reply wrapper, and a captured corpus of real requests and replies to test against.

The shape now runs end to end against a stubbed provider in the [local stack](../local-stack/README.md), which turned several of the predictions below from readings of the code into observed behaviour. Those are marked **verified** where they appear.

## 2. The V1 call

`POST /mobility/search`, routed on `category.descriptor.name` (`app.controller.ts:188`). The name pins the provider, so this is a pinned call, not a discovery one.

Request (`Orchestrator/agents/tools/weather.py:306-359`), trimmed:

```json
{ "context": { "domain": "schemes:vistaar", "action": "search", "version": "1.1.0",
               "timestamp": "1762243200", "ttl": "PT10M",
               "tags": { "session_id": "…", "question_id": "…" } },
  "message": { "intent": {
    "category":    { "descriptor": { "name": "Weather-Forecast-Mausamgram", "code": "WFC" } },
    "fulfillment": { "stops": [ { "location": { "lat": 18.4, "lon": 74.4 } } ] } } } }
```

Response — one item whose `tags[]` holds one group per day (`weatherforecast.service.ts:628-711`):

```json
{ "context": { "action": "on_search", "timestamp": "2026-11-05T09:00:00.000Z", "…": "…" },
  "message": { "catalog": {
    "descriptor": { "name": "Weather Catalog for Weather-Forecast-Mausamgram" },
    "providers": [ { "id": "mausamgram-provider",
      "descriptor":   { "name": "Mausamgram Weather Forecast",
                        "short_desc": "Gram Panchayat-Level Weather Forecasts", "images": [] },
      "categories":   [ { "id": "c1", "descriptor": { "code": "Weather-forecast", "name": "Weather Forecast" } } ],
      "fulfillments": [ { "id": "f1", "stops": [ { "location": { "lat": "18.4", "lon": "74.4" } } ] } ],
      "items": [ { "id": "mausamgram-item-1",
        "descriptor": { "name": "Weather Report for Lat: 18.4, Lon: 74.4",
                        "short_desc": "Mausamgram Forecast", "images": [] },
        "matched": true, "recommended": true,
        "category_ids": ["c1"], "fulfillment_ids": ["f1"],
        "tags": [
          { "descriptor": { "code": "Today" }, "list": [
            { "descriptor": { "code": "Location" },  "value": "Lat: 18.4, Lon: 74.4" },
            { "descriptor": { "code": "Date" },      "value": "2026-11-05" },
            { "descriptor": { "code": "Rainfall" },  "value": "5 mm" },
            { "descriptor": { "code": "Min Temp" },  "value": "18 °C" },
            { "…": "Max Temp, Min/Max Humidity, Wind Speed, Wind Direction, Weather Condition" } ] },
          { "descriptor": { "code": "Day_2" }, "list": [ "…" ] } ] } ] } ] } } }
```

**Values are pre-formatted display strings, units included.** The agent prints them; it does not parse them.

**The status is `201`, not `200` — verified.** Nest answers every POST with `201 Created` unless a handler overrides it, and none of these do. It applies to the empty-catalog replies too. The adapter has to match it, and `200` would be a visible difference.

**The wrapper question.** `Orchestrator/weather.py:261-263` declares `WeatherResponse {context, responses: List[ResponseItem]}` and validates **strictly** — it does not coerce. But this handler returns a bare `{context, message}` (`:256-262`), while the IMD handler hand-builds the wrapper (`:109-121`).

Those cannot both be true on a working path. A census of both clients found that **exactly one tool of ten coerces the bare shape** — `Orchestrator/mandi.py:676` — and the other nine require the wrapper outright.

**Half of this is now settled.** Running the service locally confirms it returns a bare `{context, message}` with no `responses[]` anywhere in it. So the wrapper is not the BPP's doing. Either something in front of it adds one, and the V1 Adapter has to stand in that same position, or this path is broken in production today. One curl at the deployed `BAP_ENDPOINT` settles the remaining half.

## 3. The provider call

`findMausamgramWeatherContent` (`weatherforecast.service.ts:269-294`):

```
GET {MAUSAMGRAM_ENDPOINT}/get-daily?lat={lat}&lon={lon}
Authorization: Basic base64(MAUSAMGRAM_USER:MAUSAMGRAM_X_API_KEY)
Content-Type: application/json
timeout 30s · 3 attempts · 2s flat pause between
```

**After 3 failed attempts it returns `null`. It does not throw.** That matters — see §6.

Response is a flat object keyed by fixed day slots:

```json
{ "location": { "lat": 18.42, "lon": 74.38 },
  "fcstday1": { "date": "2026-11-05", "rain": 5, "tmin": 18, "tmax": 31,
                "rhmin": 40, "rhmax": 85, "wspd": 3.2,
                "wind": ["WNW", "West-North Westerly"],
                "weather_warning": "…", "cloud_message": "…" },
  "fcstday2": { "…": "…" }, "fcstday3": …, "fcstday4": …, "fcstday5": … }
```

`wind` is a two-element array: abbreviation then prose. **V1 takes `wind[1]`, the prose form.** `location` is the resolved grid cell, which differs from the coordinates asked for.

## 4. Request map

| V1 | Provider | Note |
|---|---|---|
| `intent.fulfillment.stops[0].location.lat` | `?lat=` | |
| `…location.lon` | `?lon=` | |
| `…location.gps` (`"lat,lon"`) | split on `,`, trim | fallback form, `:226-230` |

Auth is `common.Step`'s, configured not mapped. No other V1 field reaches the provider — `session_id` and `question_id` are dropped, as they are today.

**Guard:** if either coordinate parses to `0`, return the empty shape without calling the provider (`:233-244`). `parseFloat` of a missing value yields `NaN`, and `NaN === 0` is false — so match V1's literal `lat === 0 || long === 0` test, not a truthiness test.

## 5. Response map

A transcription of `MausamgramCatalogGenerator` (`:628-711`). Byte-exactness is the bar, and is achievable because the input is the same object.

**Day groups.** Iterate the fixed list `['fcstday1'..'fcstday5']` by position. Emit a group only where the key is present. Code is `Today` at index 0, else `Day_{index+1}`. A gap is skipped and later days keep their numbers — a missing `fcstday2` yields Today, Day\_3, Day\_4, Day\_5. Reading the slots directly makes this free; it was the one defect that could silently hand a farmer the wrong day's forecast.

**Tag values, in this order** (order is part of the contract — the agent renders the list):

| Code | From | Format |
|---|---|---|
| `Location` | the coords **asked for** | `Lat: {lat}, Lon: {lon}` |
| `Date` | `date` | bare, `"N/A"` if absent |
| `Rainfall` | `rain` | `{v} mm` |
| `Min Temp` / `Max Temp` | `tmin` / `tmax` | `{v} °C` |
| `Min Humidity` / `Max Humidity` | `rhmin` / `rhmax` | `{v} %` |
| `Wind Speed` | `wspd` | `{v} m/s` |
| `Wind Direction` | `wind[1]` | bare prose, `"N/A"` if absent |
| `Weather Condition` | `weather_warning` else `cloud_message` | bare, `"N/A"` if both absent |

**Two traps in the missing-value handling.** V1 mixes `??` and `||`, and the unit is *inside* the template string:

1. **A missing numeric renders as `"N/A mm"`, `"N/A °C"`, `"N/A %"`, `"N/A m/s"` — the unit is still appended.** Not a bare `"N/A"`. Only `Date`, `Wind Direction` and `Weather Condition` give a bare `"N/A"`.
2. **`rain: 0` renders `"0 mm"`, not `"N/A mm"`** — those four use `??` (nullish), so zero survives. But `wind[1]` and `weather_warning` use `||`, so an **empty string falls through to the next option**. Transcribe each operator as written; a single JSONata `$exists()` for all of them is wrong in both directions.

**Envelope.** `action: "on_search"`, context otherwise echoed, `timestamp` a fresh `new Date().toISOString()`. Catalog descriptor name is `Weather Catalog for {category.descriptor.name}` — interpolated from the **request**, so it echoes whatever the caller sent.

The fulfillment stop and the item `descriptor.name` use the **resolved** coords (`apiData.location.lat` / `.lon`, falling back to the asked-for pair), while the `Location` tag uses the asked-for pair. V1 already mixes the two; keep them straight.

## 6. Behaviour deltas

**None on field content.** That is the whole point of reading the provider directly — `wind[1]` prose and the `cloud_message` fallback are both available, where the V2 mapping deliberately drops them (`weather-observation.select.yaml:58-78`).

What remains is error shape, and it needs a decision rather than a mapping:

| | V1 today | The V1 Adapter | Resolution |
|---|---|---|---|
| **Provider down** | 3 attempts, then `null` → `!apiData` guard → `providers: []`, **HTTP 201**, after up to ~94s | `common.Step` returns an error on a failed upstream call → NACK | the response half **must** emit an empty catalog rather than matching nothing (see below) |
| **Only `fcstday3+` returned** | `providers: []` — the guard needs `fcstday1` **or** `fcstday2` (`:634`), **verified** | would otherwise answer | keep V1's guard verbatim |
| **Generator throws** | `InternalServerErrorException` → 500 → agent says "Weather service unavailable" | NACK | both are failures the agent already handles; decide once, apply to every shape |

**The rule this shape proves.** `internal/common/serve.go:258-263` turns "upstream succeeded but the response half produced nothing" into an error, and therefore a NACK. V1's answer for no data is the opposite: a success with `providers: []`. Left alone, "no forecast for this village" and "the mapping is broken" become the same failure, and the agent's bare `except Exception → ModelRetry` (`weather.py:427-429`) turns both into a silent retry loop. **The response half must be written so it can never match nothing.**

## 7. Verification checklist

**Before building**

- [ ] Census says this shape carries enough traffic to be worth going first
- [ ] One curl at the deployed `BAP_ENDPOINT` — does the answer carry `responses[]`?
- [ ] Captured corpus: real V1 requests with their real V1 responses **and** the raw provider payloads, per client. A synthetic corpus covering the edge cases already exists in `local-stack/golden/`; what is missing is traffic captured from the live clients.
- [ ] Confirm which lat/lon the agent sends — village centroid or GPS fix

**Transform correctness**

- [ ] Golden test: captured provider payload in → captured V1 response out, byte for byte, no exceptions
- [ ] Shape key: `v1Dispatch` computes `weather-forecast-mausamgram` for every captured request that legacy routed there — and for none that it did not
- [ ] `location.gps` form accepted as well as `lat`/`lon`
- [ ] Tag order within a day group matches `:646-657` exactly

**The transcription traps**

All six are **verified** against the real service in the [local stack](../local-stack/README.md). They are kept here as the regression set the adapter must also pass.

- [x] Gap case: a payload missing `fcstday2` yields Today, Day\_3, Day\_4, Day\_5 — **not** consecutive numbering
- [x] `rain: 0` renders `"0 mm"`, not `"N/A mm"`
- [x] A day missing `rhmin` renders `"N/A %"` — with the unit
- [x] `wind: []` or a one-element `wind` renders bare `"N/A"`
- [x] `weather_warning: ""` falls through to `cloud_message`
- [x] `Location` tag uses asked-for coords; fulfillment stop and item name use resolved coords

**Failure behaviour**

- [x] lat or lon exactly `0` → `providers: []`, HTTP 201, provider never called — **verified**
- [x] Mausamgram down → `providers: []`, HTTP 201. **Not a NACK.** — **verified**
- [ ] Payload with only `fcstday3`/`4`/`5` → `providers: []`
- [ ] Both clients' pydantic models parse the V1 Adapter's answer and render the same string

**Operability**

- [ ] `v1Dispatch` counts this shape by route **and by client**
- [ ] `v1_fallback_total` pages, and has fired once in staging
- [ ] Reverting to forward is one row in the capability table, and has been tried once

## 8. Open

1. **Credentials.** The V1 Adapter needs `MAUSAMGRAM_USER` / `MAUSAMGRAM_X_API_KEY`, which today live only in the legacy BPP's environment. Confirm the V1 Adapter may hold them — if not, this shape cannot move and mandi is candidate 1.
2. **Retry budget.** V1 spends up to ~94s before giving up (3 × 30s + 2 × 2s). Match it, or pick a shorter budget and accept that a slow provider now fails faster than it used to?
3. **`session_id` / `question_id` are dropped**, as they are today. Confirm nothing downstream reads them.
4. **Voice does not use this shape** — it sends `Weather-Forecast` to IMD, which has no V2 pack and stays proxied. So migrating this one leaves the two clients on different code paths for weather. Intended, but worth stating.
