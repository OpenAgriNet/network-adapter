# IMD mock

Mock IMD weather API. Stands in for the real IMD upstream so the ONIX
JSONata transform (engineering-tracker#64/#66/#67) can be built without hitting real
IMD.

## Run

```bash
PORT=8082 go run .
curl 'http://localhost:8082/IMD/daily-weather-forecast'
```

## API

```
GET /IMD/daily-weather-forecast
```

No auth, no body, no params. Returns an array with one object, same field names IMD's
real response uses. Every value is randomized per request, kept within sane bounds
(temps 18-42C, rainfall 0-40mm, humidity 40-100%, forecast text matched to rainfall).

```jsonc
[{
  "Date": "2026-08-27", "Station_Code": "43382", "Station_Name": "NANCOWRY",
  "Today_Max_temp": 29.8, "Today_Min_temp": 23.4,
  "Past_24_hrs_Rainfall": 7.0,
  "Relative_Humidity_at_0830": 88, "Relative_Humidity_at_1730": 89,
  "Todays_Forecast": "Generally cloudy sky with light rain",
  "Day_2_Max_Temp": 30, "Day_2_Min_temp": 23, "Day_2_Forecast": "...",
  // ... Day_3 .. Day_7, same three fields each
  "Latitude": 7.98333, "Longitude": 93.55
}]
```

## Config

| Variable | Default |
|---|---|
| `PORT` | `8082` |

## Running it against ONIX

`docker-compose.yml` here starts this mock alongside the adapter, so a Beckn
`select` goes in and a Beckn contract comes back.

```bash
docker compose up --build
curl -X POST localhost:8081/weather/select \
  -H 'Content-Type: application/json' \
  --data-binary @sample-select.json
```

**This needs all four open branches merged.** On its own, this branch has the
mock but neither the mapper nor its mapping file, and the adapter will not
start:

| Branch | Provides |
|---|---|
| `feat/64-imd-mock-weather-api` | this mock (you are here) |
| `feat/66-jsonata-transformation-plugin` | the `jsonmapper` plugin |
| `feat/67-jsonata-mapping-files` | `config/mappings-weather.yaml` |
| `feat/46-oanregistry-sender-auth` | registry lookup — built, not exercised |

All four merge into `main` without conflicts.

`config/` holds the adapter and routing config. The mapping file is mounted from
the repo root rather than copied, so there is one copy of it rather than two
that can disagree.

### What this covers, and what it does not

It exercises the transformation path: route to the provider, drop the request
body the provider does not read, map its reply into Beckn.

It does **not** cover signing, schema validation or the registry. Each needs a
participant record and a running SunbirdRC, which is a far larger stack than
this is meant to be — so `feat/46` is merged for the build but never called.

The routing rule sends every `select` to one fixed URL. Choosing an endpoint per
request, and carrying the caller's position through to it, is provider-plugin
work tracked separately.
