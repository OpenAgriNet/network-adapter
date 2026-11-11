# ADR-0005 — Provider mappings live in helmcharts

**Status:** Proposed
**Date:** 2026-11-11
**Issues:** OpenAgriNet/network-adapter#127 (raised in the OpenAgriNet/helmcharts#123 review)

## Context

A provider mapping (the JSONata that turns a Beckn select into a provider call
and the answer back) was kept in two places: `config/mappings/` here, which the
tests read, and helmcharts `quick-start/config/mappings/`, which deployments
fetch through the registry's mapping URLs. Nothing kept them equal. The
knowledge mapping drifted: helmcharts gained `provider_id` (helmcharts#105) and
this repo never did, so the tests passed on a file that was not shipped.

## Decision

helmcharts is the only home for provider mappings. This repo holds none.

The tests read them from a checkout of helmcharts at one pinned commit,
`HELMCHARTS_REF` in the Makefile. `make sync-mappings` fetches that commit's
`quick-start/config/mappings/` into the gitignored `.helmcharts/`, once per
ref; `make test`, `make cover` and `make test-ci` depend on it. A test serves
the checked-out file over a local HTTP server, which is how the mapper fetches
a mapping in production.

A mapping change lands in helmcharts first. Taking it here is a commit that
bumps `HELMCHARTS_REF`, so the CI run on that commit proves the new mapping.

Which commit to pin follows the branch: `development` here pins a commit on
helmcharts `development`; a `release-*` branch here pins a commit on the
matching helmcharts release.

## Alternatives considered

- **Keep both copies and diff them in CI.** Still two files to edit for every
  change, and the check only says they differ, not which is right.
- **Fetch from raw.githubusercontent.com inside each test.** One run opened
  over 200 connections, took four times as long, and failed offline instead of
  being hermetic.
- **Follow a helmcharts branch instead of a commit.** Any helmcharts push could
  turn this repo's CI red, and a release branch here would be tested against
  mappings that were never released.

## Consequences

- A plain `go test` on a mapping test needs `make sync-mappings` first; without
  it the mapping is not found and the test fails.
- A mapping fix is two commits in two repos: the change in helmcharts, then the
  ref bump here.
- The pinned commit can fall behind helmcharts. That is the point (nothing
  changes here without a commit), but someone has to bump it.
