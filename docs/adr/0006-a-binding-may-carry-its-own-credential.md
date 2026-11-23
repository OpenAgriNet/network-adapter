# ADR-0006 — A binding may carry its own credential, and a second fixed header

**Status:** Accepted
**Date:** 2026-11-24

## Context

A credential profile was keyed by participant id. One participant, one login.

PMFBY breaks that. Its grievance API and its insurance API sit behind the same
hostname and are two separate systems: different login endpoints, different
response shapes, and different header names for the answer — `Authorization`
for grievance, `token` for insurance. Keyed by participant, the step can hold
one of the two, so configuring insurance would silently break grievance.

The insurance API also wants two of this deployment's own credentials on one
request: the exchanged `token` and a fixed `password`. A scheme sends exactly
one thing, and the only other header slot is the session, which exists to carry
the farmer's consent token and is only ever captured from a response — it
cannot hold a configured value.

## Decision

A credential profile is named for a binding key or for a participant id.
Resolution is the binding's own profile, else its participant's — the order
`forBinding` already uses for envelopes, upstream hosts and sessions. Startup
checks the same order, so what validates is what gets sent.

A profile may additionally set `extraHeaderName` and `extraHeaderValueEnv`.
That header is set after the scheme and before the session, so a request can
carry at most three things: the scheme's credential, one fixed extra header,
and the farmer's session. The two settings are required together, and the extra
header may not name the header the scheme already sets.

Every existing configuration names participants, and a participant id never
contains a pipe, so the binding lookup cannot match and the participant lookup
returns what it returned before.

## Alternatives considered

**Register PMFBY insurance as a second participant.** It would work today and
be wrong permanently: the registry would carry two identities for one
organisation, the catalog would publish two providers a farmer must choose
between, and every later capability on that host would need a third.

**Carry the fixed password in the session slot.** A session is written only by
capturing a field out of an upstream response. Giving it a configured value
would mean a session that is not a session, and the name would stop describing
what the thing does.

**Let a profile list several credentials.** More general and more expensive: it
turns one ordered rule into a list whose precedence an operator has to reason
about, to serve a case with exactly two members.

## Consequences

A binding now has more than one place its credential can come from, so an
operator reading a config has to check two names rather than one. The error
messages name the binding key to limit that.

A request can now carry two of the deployment's own credentials. The rule that
a binding authenticates as exactly one thing is weaker than it was, and the
cap is a hard three headers rather than a principle — a fourth credential will
need its own decision rather than another field here.

`extraHeaderName` applies whatever the scheme, including `none`, which makes a
header-only upstream expressible two ways. The scheme stays the one to reach
for; the extra header is for the second credential, not the first.
