# ADR-0002 — An action served by another binding is named by its binding key

**Status:** Accepted
**Date:** 2026-10-23 (revised 2026-10-26: one PM-KISAN participant)

## Context

PM-KISAN is one participant, `pmkisan`, with two capabilities served by
different services: Grievance by the grievance service, SchemeApplication by
the OTP service (its chatbot service). Grievance's `init` (send the OTP) and the
OTP verify before `support` and `status` are also served by the OTP service.

## Decision

A binding may carry its own `baseUrl`, which is that capability's host in place
of the participant's. PM-KISAN's two bindings each name their service.

Settings that differ by capability (envelope, OTP, follow-up) sit in a config
block named by binding key, e.g. `"pmkisan|openagrinet:Grievance":`; auth stays
in the provider's block.

The grievance block names the OTP service by binding key:
`otpBindingKey: "pmkisan|openagrinet:SchemeApplication"`.
`common.Config.HostedElsewhere` (binding key or provider → action → binding key)
sends those actions to that binding's host, read from the registry per call with
the same active checks as any binding (`common.HostOf`). The verify reads it the
same way. An unpublished binding is a 502.

## Alternatives considered

- **A second participant for the OTP service** (`pmkisan-status`). One
  provider appeared as two, and a capability code had to stand in for a host.
- **One shared base URL with the service name in every path.** No registry
  change, but the verify and follow-up paths in adapter config carried an
  environment's service name, and it breaks if the services move apart.
- **An env var with the host** (`otpBaseUrlEnv`). The address lived in two
  places, and the env one skipped the registry's status checks.
- **A `Router` interface on the envelope.** It existed only for this redirect
  and hid routing inside an encryption concern.

## Consequences

Grievance depends on the `pmkisan|openagrinet:SchemeApplication` binding being
active in the registry. Moving either service is one registry write, with no
restart. The registry's ProviderSchema gains an optional `baseUrl`; bindings
without one keep using their participant's.
