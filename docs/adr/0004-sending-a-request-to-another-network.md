# ADR-0004 — Sending a request addressed to another network

**Status:** Accepted
**Date:** 2026-10-28
**Issues:** OpenAgriNet/network-adapter#78

## Context

A consumer on our network can find a catalog another network published, because
we crawl what we agreed to read. Acting on it is a transaction — select, init,
confirm, status — and the network that published the catalog is the one that
must answer it.

So a request can arrive at our adapter addressed to somebody else. Something has
to decide that, decide where it goes, and decide whose key signs it.

An inbound request also names its own sender. That name is a claim until
something proves it.

## Decision

1. **The capability step decides, before it looks for a binding.** A binding key
   names an upstream we serve; a request for another network has none, so asking
   the binding first would read "addressed elsewhere" as "we cannot serve this".
2. **The receiver is `context.receiverId`, and it must be a network we admitted
   as a source.** A request naming no receiver is refused rather than assumed to
   be ours: guessing is what answers another network's request with our data.
3. **The destination is `context.bppUri`.** A consumer takes it from the
   `on_discover` answer, so a transaction follows the catalog back to whoever
   published it. The publisher names where it serves its capabilities.
4. **We sign as ourselves.** The forwarded call carries our own subscriber id,
   not the participant the request names, or signature validation at the far end
   resolves the wrong keyset.
5. **`checkCrossNetworkRequester` is listed after `validateSign`.** It judges
   `context.senderId`, which only `validateSign` proves. Listed first, or listed
   without it, it judges a name nobody checked.

## Alternatives considered

- **Resolve the destination from the registry.** An admitted network's `baseUrl`
  is its `services.discovery` — the discover endpoint. A transaction sent there
  reaches the wrong endpoint, and the descriptor declares no transaction address
  to use instead.
- **Enforce the step order at startup.** The step lists are rendered from this
  repository's own templates rather than written by hand, and ordering rules
  between steps are not otherwise enforced in the loader. Recorded here instead.

## Consequences

- `bppUri` is taken as given. A caller that names an address we would not have
  chosen gets a request signed by us sent there, and the protocol offers nothing
  to check it against.
- A hand-edited config can still order the steps wrongly, and nothing will say
  so. This record is the only statement of the rule.
- A network admitted only as a requester cannot be forwarded to: we agreed they
  may read from us, not that we read from them.
