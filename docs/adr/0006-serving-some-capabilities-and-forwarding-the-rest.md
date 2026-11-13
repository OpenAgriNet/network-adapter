# ADR-0006 — Serving some capabilities and forwarding the rest

**Status:** Accepted
**Date:** 2026-11-13
**Issues:** _not yet raised_

## Context

An adapter deployed in front of a service that already exists ([ADR-0005](0005-answering-a-caller-that-cannot-handle-a-nack.md)) cannot take over all of
that service's work at once. Each capability needs its own mapping, its own
comparison against the old replies, and its own decision to go live. During that
period the adapter has to answer a few capabilities and leave the rest exactly
as they were.

Before this, it could not. A module with provider steps answered what it
recognised and returned `NET_ENTITY_NOT_FOUND` for everything else. The service
being stood in front of has no such failure: its unmatched path falls through to
a general answer. So every capability not yet migrated would have turned a
working reply into an error, and the only safe deployment would have been one
where every capability moved on the same day.

The routing step and the proxy that forwards through it already existed, and
already did the right thing byte for byte. What did not exist was the ability to
put them behind a step that might already have answered: setting a route hands
the request to the proxy, which discards the answer in hand and calls an
upstream for one we were already holding.

## Decision

1. **The routing step leaves an answered request alone.** If an earlier step
   produced a response body, the routing step returns without setting a route.
   This is the mirror of the route check the provider steps already open with,
   and the two together are what let the steps coexist in one module.

2. **The order of the steps list is the dispatch.** Capability steps come first
   and each one either recognises the request and answers it or passes it
   through untouched. Routing comes last, so it is reached only by a request
   nothing recognised. There is no separate mode flag, table of enabled
   capabilities, or dispatcher: the configuration that says which capabilities a
   step serves is the same configuration that decides what gets forwarded.

3. **Forwarding is the default, by omission.** A capability nobody is configured
   to answer is forwarded. Adding a capability is adding a binding; removing one
   is removing it. Neither touches the routing table, which lists the old
   service's endpoints and is written once.

4. **A forwarded reply is not inspected.** Status, headers and body are returned
   as the upstream wrote them. The adapter does not validate, re-sign or
   normalise them, because the caller has been receiving those exact bytes from
   that exact service all along and the purpose of this path is that nothing
   changed.

## Alternatives considered

**A mode flag per capability, read by a dispatcher.** The original design. It
puts the same fact in two places, the list of capabilities a step serves and the
list of modes, and they drift. Step order already expresses it with no second
list to keep in sync.

**Forward from the error handler, on a 404.** Tempting because it needs no
change to the routing step. Rejected: by the time the handler runs, the module
has already decided it has no answer, and reconstructing the original request to
forward it is strictly worse than not having consumed it. It also makes
forwarding an error path, when forwarding is the normal case.

**A separate deployment in front, doing the splitting.** A proxy that sends some
paths to the adapter and the rest to the old service. Rejected: the split is per
capability, which lives in the payload, not per path, so the front proxy would
have to parse the body and would become a second place that knows the capability
list.

**Forwarding every request and merging the answers.** Would make the adapter's
answer verifiable against the live service on every call. Rejected: it doubles
the load on the service being retired and makes the adapter's latency the sum of
both. Comparison belongs in a test harness, not in the request path.

## Consequences

- A capability moves between answered and forwarded as a configuration change,
  and moves back the same way. Rollback for one capability does not touch any
  other and does not involve the caller.
- The first deployment can change nothing. With no capability bound, every
  request is forwarded and the old service sees the traffic it saw before.
- The adapter's write timeout becomes a property of the slowest thing it
  forwards to, not of the work it does itself. Set below that, a forwarded reply
  arrives after the deadline and the caller gets a dropped connection, which
  reads as the adapter failing when it was the upstream being slow.
- The routing table matches an exact path, so an endpoint carrying a path
  parameter cannot be forwarded. Such an endpoint has to be listed some other
  way or kept off the adapter.
- A module that lists routing without any capability step behaves exactly as it
  did before this change, so existing deployments are unaffected.
