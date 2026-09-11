# mandi_publish

Collects, builds, and (optionally) publishes Agmarknet Vistaar market catalogs for the Beckn/OpenAgriNet network.

This tool now runs as a single pipeline by default: collect upstream rows in memory, transform them into one or more per-state catalog JSON files, and optionally POST those catalogs to a provider adapter. The former file-based "collect → build → publish" flags (`--out`, `--in`) have been removed.

Workflows

- One-stage (collect → build): authenticate with Agmarknet, fetch per-state market rows, build per-state catalog files and write them to `--catalog-out`.
- Publish-only: publish catalog files already on disk by providing `--catalog-in` together with `--publish`.
- Full run: collect, build, and publish in one invocation by supplying credentials and `--publish`.

Important environment variables

- `MANDI_TOKEN_USER`, `MANDI_TOKEN_SECRET` — credentials for Agmarknet Vistaar (required for any run that collects from upstream).
- `MANDI_PUBLISH_URL` — provider adapter base URL (used when publishing; can be overridden by `--publish-url`).
- `MANDI_PARTICIPANT_ID` — fallback for `--participant-id` (default `agmarknet`).
- `APP_NETWORK_ID` — fallback for `--network-id` (default `oan-dev`).

Key flags (short summary)

- `--states` — comma-separated state codes to restrict the run (default: every state the upstream reports).
- `--from`, `--to` — window dates in `dd-MM-yyyy` format (default: today).
- `--catalog-out` — directory to write per-state catalog files (default: `catalog`).
- `--participant-id`, `--network-id` — identity fields used in generated catalog envelopes.
- `--without-geometry` — `publish` (default) or `skip`: how to handle markets whose coordinates are not `ok`.
- `--publish` — enable the publish stage (POST catalogs to the provider adapter).
- `--publish-url` — provider adapter base URL (falls back to `MANDI_PUBLISH_URL`).
- `--catalog-in` — publish already-built catalogs from this directory (use with `--publish`).
- `--dry-run` — when publishing, print targets without sending HTTP requests.
- `--retire-old` — publish an `isActive:false` tombstone for the old India-wide catalog.

Chunking behavior

The discovery service refuses catalogs carrying more than 256 geometries. Each market with usable coordinates contributes one geometry, so the tool proactively splits large states into multiple numbered catalogs to stay under that cap. A state whose market count fits within the 256-geometry budget keeps the plain filename `mandi-<STATE>.json`. A split state is written as `mandi-<STATE>-1.json`, `mandi-<STATE>-2.json`, etc., each carrying up to 256 markets.

Examples

1. Collect and write per-state catalogs (no publish):

```sh
export MANDI_TOKEN_USER=your_user MANDI_TOKEN_SECRET=your_secret
go run ./tools/publish/mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --catalog-out /tmp/catalog
```

2. Publish catalogs already on disk (dry-run):

```sh
export MANDI_PUBLISH_URL=http://localhost:9200
go run ./tools/publish/mandi_publish --publish --catalog-in /tmp/catalog --states MH --dry-run
```

3. Full end-to-end run (collect → build → publish):

```sh
export MANDI_TOKEN_USER=your_user MANDI_TOKEN_SECRET=your_secret MANDI_PUBLISH_URL=http://localhost:9200
go run ./tools/publish/mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --catalog-out /tmp/catalog --publish
```

Notes and tips

- Do not pass credentials via flags — the program reads `MANDI_TOKEN_USER` and `MANDI_TOKEN_SECRET` from the environment only.
- Use `--dry-run` to verify what would be posted without making HTTP requests.
- If you need to re-post catalogs reviewed on disk, use `--catalog-in` together with `--publish`.
- To retire the old India-wide polygon catalog, run with `--retire-old` and `MANDI_PUBLISH_URL` set.

For design rationale and invariants (zero-commodity filtering, coordinate handling, deterministic ordering), see the code in `build.go` and `main.go`.
