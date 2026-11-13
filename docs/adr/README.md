# Architecture Decision Records

One file per decision, numbered, never edited after acceptance. A decision
that turns out wrong is **superseded** by a new record, not rewritten. The
point of the log is to answer *why does this look the way it does*, and a
rewritten record cannot answer it.

Start from [`0000-template.md`](0000-template.md).

| # | Decision | Issues |
|---|---|---|
| [0001](0001-crawled-catalogs-are-published-as-regular.md) | A crawled catalog is published as REGULAR, never MASTER | network-adapter#76 |
| [0002](0002-the-crawl-signs-with-its-own-key.md) | The background crawl signs for itself, with the sign step's key under the same names | network-adapter#76 |
| [0003](0003-paging-a-crawl.md) | A crawl pages in the signed body and ends on the reported total | network-adapter#76 |
| [0004](0004-sending-a-request-to-another-network.md) | Sending a request addressed to another network: who decides, where it goes, whose key signs it | network-adapter#78 |
| [0005](0005-answering-a-caller-that-cannot-handle-a-nack.md) | Answering a caller that cannot handle a NACK: a fallback mapping half, unaddressed requests, and the answer's status | |
| [0006](0006-serving-some-capabilities-and-forwarding-the-rest.md) | A module serves the capabilities it is configured for and forwards the rest, with forwarding the default | |
