# ADR-0005 — A follow-up read adds a second answer to the first

**Status:** Accepted
**Date:** 2026-10-23

## Context

PM-KISAN application status needs two reads: `/ChatbotUserDetails` for the
record and `/ChatbotBeneficiaryStatus` for what is holding payment up. The
registry holds one call per action.

## Decision

The envelope makes the second read after a successful first one
(`followUpPath`, `followUpField`, `followUpActions`) and adds its answer to the
first under `followUpField`, on its own time budget (`followUpTimeoutMs`). The
mapping reads both. A failed follow-up leaves the field absent, never empty:
absent means "not checked", `[]` means "nothing found".

## Alternatives considered

- **Two registry actions.** Beckn `status` is one call; the network has no
  action for the second read.

## Consequences

A slow or failed follow-up does not fail the status; the answer simply has no
blockers.
