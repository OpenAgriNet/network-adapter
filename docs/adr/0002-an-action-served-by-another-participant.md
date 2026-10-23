# ADR-0002 — An action served by another participant is named by its binding key

**Status:** Accepted
**Date:** 2026-10-23

## Context

PM-KISAN grievance's `init` (send the OTP) and the OTP verify before `support`
and `status` are served by the chatbot service, not the grievance service. A
binding has one participant, so one `baseUrl`. The chatbot service is already
a participant in the registry for application status.

## Decision

The provider block names the OTP service by binding key:
`otpBindingKey: "pmkisan-chatbot|openagrinet:SchemeApplication"`.
`common.Config.HostedElsewhere` (provider → action → binding key) sends those
actions to that participant's `baseUrl`, read from the registry per call with
the same active checks as any participant (`common.HostOf`). The verify reads
it the same way. An unpublished binding is a 502.

## Alternatives considered

- **An env var with the host** (`otpBaseUrlEnv`). The address lived in two
  places, and the env one skipped the registry's status checks.
- **A `Router` interface on the envelope.** It existed only for this redirect
  and hid routing inside an encryption concern.

## Consequences

Grievance depends on the chatbot's binding being active in the registry. Moving
the OTP service is one registry write, with no restart.
