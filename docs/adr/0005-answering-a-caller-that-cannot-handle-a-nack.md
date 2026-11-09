# ADR-0005 — Answering a caller that cannot handle a NACK

**Status:** Accepted
**Date:** 2026-11-06
**Issues:** _not yet raised_

## Context

An adapter can be deployed to stand in for a service that already exists, so
that the callers of that service keep working without being changed. The
translation itself is already config: a mapping turns the caller's payload into
the network's own, calls the upstream, and turns the answer back. Three things
about such a caller could not be expressed that way.

1. **It has no failure shape but an empty answer.** Its only way of saying
   "nothing for you" is an ordinary success carrying an empty list. It has no
   branch for a protocol error, so returning a NACK presents it with a response
   it has never handled. The adapter's only way of declining was a NACK.
2. **Its payloads address nobody.** They name a capability and nothing else.
   The receiver check that ADR-0004 relies on refuses every one of them.
3. **It answers on a status of its own.** The service being replaced returned
   201, and a caller may be checking the code rather than the body.

None of the three is specific to one caller or one capability. All three are
properties of standing in for something.

## Decision

1. **A mapping may declare a `fallback` half**, which produces the answer to
   give when the upstream could not be asked or did not answer. It is read on
   two occasions and is the same document for both: the request half refused
   before the call, or the call itself failed. A mapping that declares no
   fallback leaves both as errors, which stays the default. The body is
   capability-shaped, which is what makes it the mapping's and not the
   adapter's.
2. **The fallback reads `beckn` and `_local` only.** There is no response to
   read, since not having one is the situation it exists for, and the upstream's
   own failure text is deliberately not offered, so a fallback cannot leak why
   we could not answer.
3. **`answersUnaddressed` lets a step serve a request whose context names no
   receiver.** Off by default, and it must stay off for any step serving the
   network's own payloads, where a missing receiver is a mistake worth catching.
4. **`responseStatus` sets the HTTP status of an answer a step produced.** Zero
   means 200. It changes the status only, never the body.
5. **All three are ordinary step settings**, read by the shared config parser in
   `internal/common` alongside every other step setting, and usable by any
   capability. None of them name a particular caller, protocol version, or
   domain.

## Alternatives considered

- **Produce the empty answer in the response half.** The response half runs on
  the upstream's answer. The cases that need a fallback are precisely the ones
  where there is no answer to run on, so there is no document to map.
- **Return the NACK and let the caller cope.** The caller is the thing we agreed
  not to change. A NACK reaching code with no branch for it is an outage at the
  far end, not a reported error.
- **A separate compatibility step, or a separate binary.** Both put the
  translation somewhere other than the mapping, which is the thing an operator
  can change without a rebuild. The point of doing it in the mapping is that
  swapping the behaviour needs no new code.
- **Hard-code the status for the deployment that needs 201.** The status belongs
  to the service being stood in for, and the next one will differ. One setting
  read from config costs less than a second code path.

## Consequences

- A step with a fallback turns an upstream failure into a success on the wire.
  Watching the adapter's own status codes no longer shows that the upstream is
  down; the logs and the upstream's own metrics are where it shows.
- `answersUnaddressed` switched on for the wrong step disables the check
  ADR-0004 depends on, and nothing outside this record says so.
- A non-success `responseStatus` is refused at config parse, so this cannot be
  used to turn a step into a general error emitter.
- Three more settings exist on every provider step, including the ones that will
  never use them.
