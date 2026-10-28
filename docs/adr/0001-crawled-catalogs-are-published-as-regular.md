# ADR-0001 — A crawled catalog is published as REGULAR, never MASTER

**Status:** Accepted
**Date:** 2026-10-28
**Issues:** OpenAgriNet/network-adapter#76

## Context

Discovery holds two kinds of catalog. A REGULAR catalog carries its own
content. A MASTER catalog defines others, and anything inheriting from it
resolves against it.

When we crawl another network we receive catalogs they published. We publish
our copy into our own discovery, and have to say which kind each copy is.

## Decision

A crawled copy is always published as REGULAR.

A network shares what it serves, not what defines its catalogs. Nothing in a
discover answer says a catalog is a MASTER, and nothing gives us the catalogs
that would inherit from one.

## Alternatives considered

- **Carry the kind across from the source.** The answer does not say it, so
  this means guessing from shape. A copy guessed wrong as MASTER would have
  our own catalogs resolve against another network's.

## Consequences

- A network that does use master catalogs is crawled as the resolved
  catalogs it serves, which is what a consumer sees anyway.
- Inheritance stays inside the network that defines it; nothing crosses.
