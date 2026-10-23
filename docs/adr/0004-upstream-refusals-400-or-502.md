# ADR-0004 — An upstream refusal is a 400 when its reason is known, else a 502

**Status:** Accepted
**Date:** 2026-10-23

## Context

PM-KISAN answers HTTP 200 for everything and says no with a `"False"` flag.
Some refusals are the caller's to fix (an unknown registration number); others
are PM-KISAN failing. The shared refusal no longer turns a mapping's 202 into a
NACK.

## Decision

As PMFBY does: a refusal whose reason the mapping knows is a 400
`BIZ_GENERIC_ERROR` in our words, with `details.path` on the field to fix. Any
other refusal is a 502 `NET_DOWNSTREAM_UNAVAILABLE`. No record found is a 400
`BIZ_GENERIC_ERROR`. The provider's own text is logged as `cause` and never
returned.

## Alternatives considered

- **Every refusal a 502.** Tells the caller PM-KISAN is down when the
  enrolmentId is wrong.
- **Return the provider's message.** Its wording is not ours to publish and
  can carry personal data.

## Consequences

Each new reason is a line in the mapping's `KNOWN_REFUSALS`. A reason not yet
listed is a 502 until it is.
