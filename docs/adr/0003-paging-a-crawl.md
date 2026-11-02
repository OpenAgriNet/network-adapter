# ADR-0003 — A crawl pages in the signed body and ends on the reported total

**Status:** Accepted
**Date:** 2026-10-28
**Issues:** OpenAgriNet/network-adapter#76

## Context

A network may share more catalogs than one answer should carry, so a crawl
walks it a page at a time. Two things have to be decided: where the page we
ask for travels, and how we know we have reached the end.

A signature covers the request body. It does not cover the query string, so a
page asked for there is not covered either: the page could be changed in
flight and the signature would still verify.

discovery-service 0.1.0 read the page off the query string and answered with
catalogs alone. Its `development` build refuses a query-string page, reads it
from the body, and answers with the page it applied — `pagination` carrying
`limit`, `offset` and `total` (their ADR-0017). It pages by **resource**, and
one catalog may hold several, so a page can carry fewer catalogs than the
limit asked for.

## Decision

1. **The page travels in the message body**, as `message.limit` and
   `message.offset`, so the signature covers it.
2. **The walk ends where the answer says the results do** — when
   `offset + limit` reaches `total`, using the values the network reported
   rather than the ones we asked for, because an oversize limit is clamped.
3. **A network that reports no page is walked on a short page**, as before, so
   a network still on 0.1.0 is crawled rather than refused.
4. **A network whose offset does not advance is stopped and logged.** Not
   after a fixed number of pages: no page count is right for every network,
   and a wrong one truncates a large catalog silently.

## Alternatives considered

- **Page in the query string.** Simpler to read, and unsigned. The page a
  crawl asked for would be the one field of the request anyone could change.
- **End on a short page.** What we did. Against a build that pages by
  resource it ends the walk on the first page whose catalogs number fewer
  than the limit, which is most of them.
- **Cap the number of pages.** Bounds a runaway walk, but the cap is a guess:
  too low truncates a large network, too high still walks a broken one for a
  long time. The total already bounds it, and a stalled offset is the case a
  cap was really for.

## Consequences

- A crawl against 0.1.0 discovery still works, but cannot tell a clamped page
  from the end of the results.
- `total` counts resources while our log line counts catalogs. The two are
  different units on purpose and the log names both.
- A network that reports a total it never reaches is walked until its offset
  stops moving, then dropped with an error naming it.
