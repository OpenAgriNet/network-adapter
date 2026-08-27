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
