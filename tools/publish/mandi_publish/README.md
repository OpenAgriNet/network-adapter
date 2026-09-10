# mandi_publish

Collects Agmarknet Vistaar's market master data into one normalized document.

Publishing is not yet part of this command. What it writes is the input to a
later stage that turns markets into a MandiPrice catalog — kept separate so the
collected data can be reviewed before anything reaches a network.

## Running it

```sh
export MANDI_TOKEN_USER=... MANDI_TOKEN_SECRET=...
go run ./tools/publish/mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --out markets.json
```

Credentials come from the environment only. A flag would land in shell history
and in `ps` output.

Omit `--states` to walk every state the upstream reports. Dates are `dd-MM-yyyy`,
which is what this upstream reads, and default to today.

Exit is non-zero when any state failed, and the failures are in `stateErrors`
alongside the markets that did come back.

## Tokens

The token is exchanged once at the start of a run and used for every call in
it. It is **not** re-exchanged on a 401/403. If it expires mid-run, every state
still to be fetched fails and lands in `stateErrors`, and the process exits
non-zero. The remedy is re-running the tool; there is nothing to configure.

## What it collects

Three upstream calls, each described by a file in `mappings/`:

| Call | Mapping | Gives |
|---|---|---|
| `master-data?option=4` | `master-states.yaml` | the state codes that drive the loop |
| `master-data?option=6` | `master-markets.yaml` | every market's coordinates, fetched once for all of India |
| `market-commodity-mapping?statecode=…` | `market-commodity.yaml` | each market's codes and what it trades |

The mappings are the adapter's own format and run through the same JSONata
mapper, so what a reviewer reads is what executes.

## The codes that matter

The market-commodity call carries every value a `select` must send:

| Output field | Price call parameter |
|---|---|
| `marketId` | `marketcode` |
| `districtId` | `districtcode` |
| `stateCode` | `statecode` |
| `commodities[].code` | `commoditycode` |

Master data also carries `agm_market_center_code` and `agm_district_code`. They
are different numbers and are **not** what the price call takes.

## Coordinates

`coordinateQuality` is `ok`, `missing`, `suspect` or `outOfBounds`, first match
wins. `latitude`/`longitude` appear only when it is `ok`.

Upstream carries no coordinates on about a third of its market rows, and 31 rows
repeat the same number in both fields — a copy-paste defect whose point is not
in India. Nothing here repairs a coordinate: a wrong point returns the wrong
mandi to a farmer and nothing downstream can detect it.

## Verifying against the live service

This is a manual step. It is not a test and nothing in CI runs it.

```sh
export MANDI_TOKEN_USER=... MANDI_TOKEN_SECRET=...
go run ./tools/publish/mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --out /tmp/mh.json
```

Expected, from the live service on 2026-09-10:
- about **273 markets**, roughly **2115** market–commodity pairs in total
- **one** market reported as not `ok` for coordinates
- `stateErrors` empty, exit 0
- market `1282` present as `Jamkhed APMC`, `districtId` 338, `stateCode` MH,
  with `4`/`Maize` among its commodities

Confirm those codes answer for real:

```sh
TOKEN=$(curl -sS -X POST http://34.0.4.235:8080/v1/generate-dynamic-token-agmarknet \
  -H 'Content-Type: application/json' \
  -d "{\"access_name\":\"$MANDI_TOKEN_USER\",\"password\":\"$MANDI_TOKEN_SECRET\"}" \
  | python3 -c 'import json,sys;print(json.load(sys.stdin)["token"])')

curl -sS "http://34.0.4.235:8080/v1/fetch-agmarknet-vistaar?token=$TOKEN&statecode=MH&districtcode=338&marketcode=1282&commoditycode=4&from_date=20-08-2026&to_date=20-08-2026"
```

A non-empty result proves the collected codes are the ones the price call
accepts, which is the whole point of the exercise. An empty result with a 200 is
the failure this tool exists to prevent — if it happens, the codes being read
are the wrong ones and the mapping files are where to look.

---

## What Plan 0 does not do

Named so nobody goes looking for them:

- **No publishing.** The catalog generator and the POST to the provider
  adapter's `/publish` are Plan 1, built on `markets.json`.
- **No decision about flawed coordinates.** A `missing` or `suspect` market is
  collected and labelled; whether it is published without geometry is Plan 1's
  call, made once the real distribution has been seen.
- **No concurrency.** A `--concurrency` flag is a later change with no effect on
  the output contract.
- **No retry.** A failed state is recorded and reported; re-running the tool for
  that state is the remedy, and it is cheap.
