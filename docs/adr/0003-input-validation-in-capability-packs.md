# ADR-0003 — Input validation lives in the capability packs, not the mappings

**Status:** Accepted
**Date:** 2026-10-23

## Context

The PM-KISAN mappings checked their own inputs: identifier format, Aadhaar
refusal, category codes, OTP shape, description length, the one-identifier
rule. The Grievance and SchemeApplication packs (network-specs,
`api-schema-packs-v0.1`) state the same rules, and the extended schema
validator enforces them on every object that carries `@context` and `@type`.

## Decision

The mappings carry no input checks; the request half only builds the upstream
body. One check stays in `application-status.status.yaml`: the OTP must be
present. The pack cannot require it on `status` alone, because a status ask and
an init ask have the same shape.

The packs pin `@context` to `openagrinet.github.io`, which does not yet serve
`api-schemas/`. `extendedSchema_contextRewrite` (`from|to` prefix pairs)
fetches them from the branch instead. The payload keeps its `@context`; the
allowlist is checked on the rewritten host.

A schema error carries the reason, code and path, never the value: kin-openapi
appends the value and the schema by default, and a pack marks `enrolmentId` and
`applicant.mobile` no-echo.

## Alternatives considered

- **Keep both.** Two statements of one rule drift, and the mapping's codes
  disagreed with the validator's.
- **A local copy of the packs** (`extendedSchema_localSchemaPath`). It goes
  stale against the branch.

## Consequences

Error codes are the validator's (`SCH_SCHEMA_VALIDATION_FAILED`,
`SCH_INVALID_ENUM`, …) with its paths. The schema's `enrolmentId` minimum is 6
characters, stricter than the mapping's 1. A description padded with spaces is
measured untrimmed. Drop the rewrite once the published URL serves the packs.
