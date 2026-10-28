# ADR-0002 — The background crawl signs for itself

**Status:** Accepted
**Date:** 2026-10-28
**Issues:** OpenAgriNet/network-adapter#76

## Context

Every call we make to another network is signed as us. On the request path a
sign step does this: it sits in a handler's step chain and reads the key from
the chain's context.

The crawl has no request to ride. It runs on a timer, in the background, with
no inbound call and so no step chain to be part of.

## Decision

The crawl holds its own signing key and signs its own discovers, configured
under the same names the sign step uses: `signingPrivateKey` and `keyId`.

Same key, same names, reached a different way.

## Alternatives considered

- **Reach the sign step from the crawl.** A step is built for a chain and
  takes its key from the context a request created. There is no request.
- **Give the crawl a second key of its own.** Two keys for one network means
  two things to rotate and two to publish, and a verifier resolving the wrong
  one fails with nothing pointing at the cause.

## Consequences

- The key is configured twice in one deployment, once for the sign step and
  once for the crawl. Same value, so a rotation has to change both.
- A crawl in a deployment with no key configured refuses to start rather than
  sending unsigned discovers.
