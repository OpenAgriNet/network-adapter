# Grievance — flow of execution

Two providers, one capability. Every call is synchronous: the `on_*` reply comes back on
the same HTTP response, not on a callback.

> What the two portals actually accept and return — one line of provenance per field —
> is `grievance-upstream-contracts.md`. Where this page and that one disagree, that one
> wins.

The grievance is lodged with `/support` and read back with `/status`. PMFBY issues a
challenge first — an SMS OTP — so it has an `/init` leg; PM-KISAN does not.

This page is the sequence of execution — the calls in order, and the payloads they carry.

| | PMFBY | PM-KISAN |
|---|---|---|
| Steps | `init` → `support` → `status` | `support` → `status` |
| Identity proof | `challenge`, method `SMS_OTP`, to the farmer's phone | none — the portal asks for none |
| Farmer is known by | mobile number | registration number |
| Case is retrieved by | ticket number + phone | identity + the date it was filed |
| `@type` | `openagrinet:PMFBYGrievance` | `openagrinet:PMKISANGrievance` |

## One rule for where every field sits

**`descriptor` is the complaint. Attributes are the caller and the case record.**

`Support` carries a `descriptor`, and so does `Contract` — so the same rule holds on every
leg, and a reader who learns it once can read any example on this page.

| | holds | on `support` | on `init` / `status` |
|---|---|---|---|
| `orderId` | the enrolment the complaint is against | `applicationNo` on PMFBY, the registration number on PM-KISAN — echoed unchanged on the reply | — (`Contract` has no `orderId`) |
| `descriptor` | the complaint: category `code`/`name`, the farmer's words in `longDesc` | `support.descriptor` | `contract.descriptor` |
| attributes | who is asking, and what the case record says | `channels`, selected by `@type` | `commitmentAttributes` |

`descriptor` is absent where there is nothing to describe: `init` asks for a challenge before
the farmer has stated anything, and a `status` request names a ticket rather than restating
the case.

**On `init` and `status` the envelope is a Beckn `Contract` holding one commitment**, and
the payload rides on `commitmentAttributes` — a grievance is a promise with a lifecycle, not
a catalogable thing of value. The commitment's one resource stays thin: an id pointing at
the catalog entry, plus a `quantity`, which the spec requires but defines no schema for. We
send `{"count": 1}` in both directions; it carries no information. That id is fixed per
provider and never changes, so it names the catalog entry and never the case.

**On `support` there is no contract** — a `SupportAction` has no `contract` property. The
payload rides in `support.channels[0]`, which takes the same base `Attributes` schema that
`commitmentAttributes` extends, so the pack validates unchanged. On the ask there is exactly
one channel and it is the complaint: the adapter routes on that array and refuses a second
entry, because one request maps to one upstream call. On a reply, where a helpline could sit
beside the case, address the entry by `@type` rather than by index.

## Sequence

![Grievance flow of execution](grievance-flow.svg)

Editable source is `grievance-flow.excalidraw`, which opens at
[excalidraw.com](https://excalidraw.com). Re-export both `grievance-flow.svg` and
`grievance-flow.png` after any change.

Every reply is synchronous — the `on_*` rides the same HTTP response, never a callback.
The OTP never reaches the lodge call and is never returned. `orderId` is the enrolment the
complaint is against and comes back on `on_support` unchanged, on both schemes; PMFBY's
ticket is a separate thing and arrives in `ticketNo` on the channel. PM-KISAN issues no
ticket at all, which is why its case is read back by identity and the date it was filed.

---

## PMFBY

### Step 1 — `init`: ask for a challenge

Send the farmer's phone. Nothing else is needed yet, and there is no `descriptor` — the
farmer has not stated a complaint.

```json
POST /init
{
  "context": {
    "version": "2.0.0", "action": "init", "networkId": "openagrinet",
    "transactionId": "3f9a…", "messageId": "8c21…",
    "timestamp": "2026-09-28T10:15:00Z"
  },
  "message": { "contract": {
    "id": "b1d4e2f0-5a63-4c81-9e77-2af0c9d31b45",
    "commitments": [{
      "status": { "descriptor": { "code": "DRAFT", "name": "draft" } },
      "offer": {
        "id": "off:pmfby:grievance",
        "provider": { "id": "pmfby", "descriptor": { "name": "PMFBY Grievance Portal" } },
        "resourceIds": ["res:pmfby:grievance"]
      },
      "resources": [{ "id": "res:pmfby:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "applicantPhone": "9876543210"
      }
    }]
  }}
}
```

**`on_init`** — commitment stays `DRAFT`. The OTP is never returned; you get the mechanism
used and a masked confirmation of where it went.

```json
"commitmentAttributes": {
  "@context": "…/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
  "@type": "openagrinet:PMFBYGrievance",
  "informationMode": "OnDemand",
  "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
  "challengeIssued": {
    "method": "SMS_OTP", "sentTo": "98XXXXXX10", "expiresAt": "2026-09-28T10:25:02Z"
  }
}
```

Still `OnDemand`, even though this is a response. `Direct` means "carrying a real case",
and the pack requires `ticketNo`, `caseStatus`, `filedOn` and `source` wherever it appears.
A challenge acknowledgement has none of them, so it is not `Direct`.

**Read `challengeIssued.method` rather than assuming.** It is `SMS_OTP` on PMFBY today, and
it is the field that tells an experience layer what to collect — six digits texted to a
phone. The network names other mechanisms (`AADHAAR_OTP`, `DEVICE_TOKEN`) in one shared
enum, and a portal that starts offering one answers with that method instead. A client that
branches on `method` keeps working; a client that hard-codes "six digits" does not.
`method`, `sentTo` and `expiresAt` are all required on PMFBY, so all three can be relied on.

**Next:** collect the six-digit OTP from the farmer, then `support`.

### Step 2 — `support`: lodge the grievance

Same `transactionId`, new `messageId`. The contract does not travel.

`descriptor` is the whole complaint. `code` is load-bearing: `3.10` is the portal's category
and sub-category joined by a dot, and the adapter splits it there into two upstream fields.
`channels[0]` holds what identifies and authenticates the farmer, plus `providerId` —
this payload composes no contract, so that is where the adapter reads the provider from.

```json
POST /support
{
  "context": {
    "version": "2.0.0", "action": "support", "networkId": "openagrinet",
    "transactionId": "3f9a…", "messageId": "9d44…",
    "timestamp": "2026-09-28T10:18:30Z"
  },
  "message": {
    "support": {
      "orderId": "KA2026KH00123456",
      "descriptor": {
        "code": "3.10",
        "name": "Enrollment / Portal Issues Login",
        "longDesc": "Claim approved in July but no amount credited."
      },
      "channels": [{
        "@context": "…/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "OnDemand",
        "providerId": "pmfby",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "applicantPhone": "9876543210",
        "cropYear": "2026",
        "season": "Kharif",
        "challenge": { "method": "SMS_OTP", "value": "482137" }
      }]
    }
  }
}
```

**`on_support`** — `orderId` comes back unchanged. The spec describes it as "the order
against which support is required", which is an ask-side meaning, and nothing about a reply
changes what the complaint is against — so the application number is echoed rather than
overwritten. The ticket the portal just issued is a different thing and goes in a different
place: `ticketNo` on the channel. The spec says the provider
"returns it with populated channel details and, when a ticket has been created, the ticket
reference" but never names the field that holds the reference; the channel is where the
scheme's own fields live, so that is where it goes. `challenge` and `applicantPhone` are
dropped by the response allow-list.

The portal's lodge reply carries four things: a success flag, `ticket-no`, `ticket-id` and a
message. Only `ticket-no` becomes a value you can read, as `ticketNo` — the number the
farmer quotes back. `ticket-id` is the portal's own row id and is dropped: nothing sends it
and no later call needs it, which is the same reason `TicketStatusID` is not mapped. The
success flag decides whether this is an ACK at all. The message is never returned; it may hold a stack trace or an
internal hostname. `caseStatus`, `filedOn` and `source` are **not in the reply** — the
adapter asserts them, and `descriptor` is the caller's own words echoed back.

```json
{
  "context": { "action": "on_support", "messageId": "9d44…", "…": "…" },
  "message": {
    "support": {
      "orderId": "KA2026KH00123456",
      "descriptor": {
        "code": "3.10",
        "name": "Enrollment / Portal Issues Login",
        "longDesc": "Claim approved in July but no amount credited."
      },
      "channels": [{
        "@context": "…/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "ticketNo": "100626000099001",
        "caseStatus": { "code": "Registered" },
        "filedOn": "2026-09-28",
        "source": { "sourceId": "pmfby", "sourceName": "PMFBY Grievance Portal" }
      }]
    }
  }
}
```

No helpline channel is emitted. Neither scheme publishes one we have on file, so there is
nothing to put there. If one is confirmed later it becomes a second member of `channels` —
which is why a consumer must select the case record by `@type` rather than by position.

**Next:** store `contract.id` and `ticketNo` against the farmer. `status` whenever they ask
for an update.

### Step 3 — `status`: check the ticket

New `transactionId` — a separate session, days later. Same `contract.id` as `init`. No
`descriptor` on the ask: we are naming a ticket, not restating the complaint.

No OTP. The OTP is spent once, at filing. Note what that means: anyone with the ticket
number **and** the filing phone number can read the case. Both are needed and a ticket is
not guessable from a phone, so it is not enumerable — but the read is not authenticated.

```json
POST /status
{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "transactionId": "b7e0…", "messageId": "2f8c…",
    "timestamp": "2026-10-02T09:02:11Z"
  },
  "message": { "contract": {
    "id": "b1d4e2f0-5a63-4c81-9e77-2af0c9d31b45",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE", "name": "active" } },
      "offer": {
        "id": "off:pmfby:grievance",
        "provider": { "id": "pmfby", "descriptor": { "name": "PMFBY Grievance Portal" } },
        "resourceIds": ["res:pmfby:grievance"]
      },
      "resources": [{ "id": "res:pmfby:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "…/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "applicantPhone": "9876543210",
        "ticketNo": "100626000099001"
      }
    }]
  }}
}
```

**`on_status`** — `contract.descriptor` carries the complaint as the portal holds it, and
the attributes carry the case record. `caseRemark` is what has been written about the
case, not the complaint, so it stays in attributes; it is absent while nothing has been
recorded. `caseStatus.code` is the network's own word for the state; the portal's phrase,
when there is one, rides alongside in `caseStatus.name`.

```json
{
  "context": { "action": "on_status", "messageId": "2f8c…", "…": "…" },
  "message": { "contract": {
    "id": "b1d4e2f0-5a63-4c81-9e77-2af0c9d31b45",
    "descriptor": {
      "code": "3.10",
      "name": "Enrollment / Portal Issues Login",
      "longDesc": "Claim approved in July but no amount credited."
    },
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE", "name": "active" } },
      "offer": {
        "id": "off:pmfby:grievance",
        "provider": { "id": "pmfby", "descriptor": { "name": "PMFBY Grievance Portal" } },
        "resourceIds": ["res:pmfby:grievance"]
      },
      "resources": [{ "id": "res:pmfby:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "…/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "ticketNo": "100626000099001",
        "applicationNo": "KA2026KH00123456",
        "cropYear": "2026",
        "season": "Kharif",
        "caseStatus": { "code": "UnderReview", "name": "Under Review" },
        "filedOn": "2026-09-28",
        "caseRemark": "Claim file reopened, awaiting surveyor report.",
        "source": { "sourceId": "pmfby", "sourceName": "PMFBY Grievance Portal" }
      }
    }]
  }}
}
```

**Next:** repeat `status` on demand. No `caseRemark` means nothing has been recorded yet —
PMFBY publishes the remark but no date against it, so there is no `remarkedOn` to show.

The portal returns a great deal more than this: the farmer's name, mobile number, email,
full address down to the village, the insurance policy number and the insurer. The adapter
maps the fields above and drops the rest. The response mapping is an allow-list, not a
passthrough, and that is deliberate.

**This one reply is not verified.** Everything PMFBY *accepts* is confirmed against the
live system; what it *returns on a case read* is not. The field names this payload is built
from appear in no source we hold, and the legacy code renders that reply without naming a
single field, so its shape cannot be recovered from it. The Beckn side above will not
change — but which upstream field feeds which value may. Confirm against PMFBY's own API
document before building on it; `grievance-upstream-contracts.md` records what is verified
and what is not, field by field. PMFBY's `init` and `support`, and all of PM-KISAN, are
unaffected.

---

## PM-KISAN

No OTP leg. The portal proves nothing about the caller, so neither does the network —
`support` is the first call.

**`orderId` is the registration number.** PM-KISAN has no application number and no case
number — the upstream takes exactly one reference, `IdentityNo`, and nothing else in the API
names a case. So the farmer's enrolment is both the identity the portal authenticates on and
the thing the complaint is against, which is what `orderId` means. It goes in the same slot
PMFBY fills with `applicationNo`, and like PMFBY's it is echoed unchanged on the reply:
`orderId` says what the complaint is against, and that does not change because the portal
answered. Handing back the caller's own registration number over the same signed exchange it
arrived on discloses nothing. It stays `no-log` and `no-trace` regardless.

**Encryption is not yours to do.** PM-KISAN accepts and returns AES-GCM envelopes, not
JSON: `{"EncryptedRequest": "…"}` on the way in, `{"d": {"output": "…"}}` on the way back.
None of that is visible here. The adapter encrypts after the request mapping runs and
decrypts before the response mapping runs, so the experience layer sends and receives the
plain Beckn payloads on this page and nothing else. The key and IV are named by environment
variable in the adapter config and held nowhere near the network.

This codec does not exist yet — it is the one piece of new adapter machinery PM-KISAN
needs, and until it ships nothing in this section can run. PMFBY is unaffected; it speaks
ordinary JSON.

### Step 1 — `support`: lodge the grievance

`descriptor.code` is one of the pack's ten codes. `orderId` is the farmer's PM-KISAN
registration number — the enrolment the complaint is against. `channels[0]` carries nothing
but the scheme, because on this leg there is nothing else to carry.

```json
POST /support
{
  "context": {
    "version": "2.0.0", "action": "support", "networkId": "openagrinet",
    "transactionId": "7f3a…", "messageId": "4a1e…",
    "timestamp": "2026-09-28T11:04:00Z"
  },
  "message": {
    "support": {
      "orderId": "UP12345678A",
      "descriptor": {
        "code": "G003",
        "name": "Installment not received",
        "longDesc": "Third instalment for 2026 has not been credited."
      },
      "channels": [{
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "providerId": "pmkisan",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" }
      }]
    }
  }
}
```

Categories are a closed list, `G001`–`G010`:

```
G001 account number not correct        G006 gender not correct
G002 online application pending        G007 payment related
G003 installment not received          G008 problem in OTP-based eKYC
G004 transaction failed                G009 problem in biometric eKYC
G005 problem in Aadhaar correction     G010 problem in facial eKYC
```

**`on_support`** — `orderId` comes back as it went up, exactly as on PMFBY. What is missing
here is the ticket: PMFBY returns one in `ticketNo` on the channel and PM-KISAN issues none
at all, so the case record comes back without a case identifier.
The portal's lodge reply is `{ Responce, message }` and nothing more — no identifier, no
date, no status. `Responce` becomes the ACK, `message` is logged redacted and never
returned. So **nothing below comes from the portal**: `descriptor` is the caller's own words
echoed back, `caseStatus` and `filedOn` are the adapter's assertions, `source` is
configuration. The reply confirms receipt and nothing more.

```json
{
  "context": { "action": "on_support", "messageId": "4a1e…", "…": "…" },
  "message": {
    "support": {
      "orderId": "UP12345678A",
      "descriptor": {
        "code": "G003",
        "name": "Installment not received",
        "longDesc": "Third instalment for 2026 has not been credited."
      },
      "channels": [{
        "@context": "…/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "caseStatus": { "code": "Registered" },
        "filedOn": "2026-09-28",
        "source": { "sourceId": "pmkisan", "sourceName": "PM-KISAN Grievance Portal" }
      }]
    }
  }
}
```

**Next:** there is no ticket number to keep. Keep **`contract.id`, the registration number
and `filedOn`** — those three are what `status` needs, where the registration number rides in
`commitmentAttributes.registrationNo` rather than `orderId`, because a `Contract` has no
`orderId`.

### Step 2 — `status`: read the replies

`filedOn` is the one from `on_support`, and it is required: the portal has no per-grievance
endpoint, so it is what picks this grievance out of the farmer's list. It is also the weak
point — two grievances filed on the same identity on the same day are indistinguishable, and
the portal issues nothing that would tell them apart.

`contract.id` is a UUID the caller mints here, since PM-KISAN has no earlier leg to mint one.

```json
POST /status
{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "transactionId": "c04b…", "messageId": "e91f…",
    "timestamp": "2026-10-02T09:10:44Z"
  },
  "message": { "contract": {
    "id": "c9b31a45-0f78-4e2d-9a60-84b7d3e15c02",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE", "name": "active" } },
      "offer": {
        "id": "off:pmkisan:grievance",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:grievance"]
      },
      "resources": [{ "id": "res:pmkisan:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "…/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "registrationNo": "UP12345678A",
        "filedOn": "2026-09-28"
      }
    }]
  }}
}
```

**`on_status`** — one commitment: the grievance this contract is about. The farmer's other
grievances are filtered out; each has its own contract. `registrationNo` is not echoed. The
farmer's name, father's name, gender, mobile number and address are dropped by the
allow-list, and `Reg_No` with them: five of the record's fourteen fields survive. The record
carries no category, so `descriptor` comes back with `longDesc` only — the description the
portal stored — and no `code` or `name`.

```json
{
  "context": { "action": "on_status", "messageId": "e91f…", "…": "…" },
  "message": { "contract": {
    "id": "c9b31a45-0f78-4e2d-9a60-84b7d3e15c02",
    "descriptor": {
      "longDesc": "Third instalment for 2026 has not been credited."
    },
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE", "name": "active" } },
      "offer": {
        "id": "off:pmkisan:grievance",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:grievance"]
      },
      "resources": [{ "id": "res:pmkisan:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "…/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "caseStatus": { "code": "Replied" },
        "filedOn": "2026-09-28",
        "remarkedOn": "2026-10-01",
        "caseRemark": "Instalment released on 2026-10-01, credited to the linked account.",
        "source": { "sourceId": "pmkisan", "sourceName": "PM-KISAN Grievance Portal" }
      }
    }]
  }}
}
```

**Next:** repeat `status` on demand.

---

## Three rules that apply to both

- **`informationMode` tells you the direction.** `OnDemand` is a request, `Direct` is an
  answer carrying a real case. Read it rather than the action name.
- **Secrets go one way; identifiers do not.** `challenge` is `writeOnly` as a whole object —
  send it, never expect it back, never log it, and `challenge.value` never reaches the lodge
  call. `challengeIssued` is the `readOnly` other half: returned, never sent, never secret. `applicationNo` and
  `registrationNo` are identifiers rather than secrets and do come back: both are echoed in
  `orderId` on `on_support`, and PMFBY's comes back again in `commitmentAttributes` on
  `on_status`, because the portal sends it. Both are `no-log` and `no-trace` without
  exception either way. Every
  field carrying personal data is marked in the pack with `x-oan-pii`, giving it a class
  and a handling list; the marking is documentation today, not something the validator
  enforces.
- **Nothing is closed yet.** No portal documents a terminal status, so a commitment stays
  `ACTIVE` even after a reply arrives.

## Errors

Errors are the HTTP response, not a later `on_*` callback, and every one is a Beckn NACK —
signed like any other response, with an `error.code` from the spec's `ErrorCode` enum and
`details.path` naming the field that failed:

```json
{
  "message": {
    "status": "NACK",
    "messageId": "<the messageId from the request context>",
    "error": {
      "code": "SCH_REQUIRED_FIELD_MISSING",
      "message": "applicantPhone is required",
      "details": { "path": "$.message.support.channels[0].applicantPhone" }
    }
  }
}
```

| What went wrong | HTTP | `error.code` |
|---|---|---|
| A required field is missing | `400` | `SCH_REQUIRED_FIELD_MISSING` |
| Wrong OTP (PMFBY) | `400` | `BIZ_GENERIC_ERROR` — the lodge call is never made |
| Portal rejects the request, or is unreachable | `502` | `NET_DOWNSTREAM_UNAVAILABLE` |
| Envelope will not decrypt (PM-KISAN) | `500` | `NET_INTERNAL_ERROR` |
| Nothing on file — unknown ticket, or no grievance matching `filedOn` | `202` | `BIZ_NO_RESULTS_FOUND` |

**`details.path` follows the field, not the action.** A missing phone on `support` reports
`$.message.support.channels[0].applicantPhone`; the same failure on `status` reports
`$.message.contract.commitments[0].commitmentAttributes.applicantPhone`. A missing category
reports `$.message.support.descriptor.code`. A caller highlighting the offending input has
to follow the same rule the mapping does; the pack's `x-beckn-path` is the source for both.

**A wrong OTP is deliberately not a `401`.** That code means `NackUnauthorized` — the Beckn
signature on the request failed to verify. A farmer mistyping six digits is not that. The
enum has no OTP-specific value, so `BIZ_GENERIC_ERROR` carries it. The portal publishes no
clean success signal for the verify step either, so this row is provisional.

**The portal's own message is never passed through.** A failure body may hold a stack trace
or an internal hostname, and a rejected request is often quoted back with the credential in
it. The adapter logs it redacted and returns our own message instead.

**`502` is not a response the spec declares.** `/init`, `/support` and `/status` each list
only `200, 202, 400, 401, 403, 429, 500`. The adapter returns `502` for an upstream failure,
so we inherit it.

### Nothing on file

The farmer asks about ticket `100626000088002`. PMFBY is reached, understands the question,
and answers: no complaint with that number. A digit was mistyped, or the ticket was filed
from a different phone, or it was never filed at all.

Nothing broke, so this is not a NACK — there is nothing for the caller to correct. But
neither is there a case to describe. Beckn's answer for exactly this is `202` with an
`AckNoCallback` body: the request was accepted, and nothing further is coming. `/support`
declares `AckNoCallback` among its response codes too, so this holds on the lodge leg.

```json
{
  "message": {
    "status": "ACK",
    "messageId": "2f8c…",
    "error": {
      "code": "BIZ_NO_RESULTS_FOUND",
      "message": "No grievance matches this ticket and phone number."
    }
  }
}
```

`status` is `ACK`, not `NACK`: the request was accepted and processed. The `error` says why
no result follows, not that anything failed.

An earlier draft signalled this by returning a commitment with `commitmentAttributes`
omitted. That is spec-legal — the property is optional — but it says nothing: a consumer
cannot tell an absent case from a provider that dropped the field. The code says it out loud
and needs no knowledge of this network's conventions.

## Registry

Nothing above works until the two providers are registered. Three records each — what the
data is, who we call, and how.

**PMFBY**

```jsonc
{ "SchemaRegistry": {
  "capabilityCode": "openagrinet:PMFBYGrievance",
  "name": "PMFBY Grievance",
  "version": "v0.1",
  "schemaUrl": "https://raw.githubusercontent.com/OpenAgriNet/network-specs/main/api-schemas/PMFBYGrievance/v0.1/attributes.yaml",
  "status": "active"
} }

{ "Participant": {
  "participantId": "pmfby",                  // also the Beckn offer.provider.id
  "name": "PMFBY Grievance Portal",
  "type": "upstream_api",                    // speaks HTTP, not Beckn: no role, no keys
  "status": "active",
  "baseUrl": "https://pmfbydemo.amnex.co.in/krphapi/FGMS"   // demo host; confirm production
} }

{ "ProviderSchema": {
  "bindingKey":     "pmfby|openagrinet:PMFBYGrievance",
  "participantId":  "pmfby",
  "capabilityCode": "openagrinet:PMFBYGrievance",
  "status": "active",
  "actions": [
    { "action": "init",    "method": "POST", "path": "/SendOTP",                 // TODO: path unconfirmed
      "mappings": "mappings/pmfby/grievance.init.yaml",
      "timeoutMs": 20000, "status": "active" },
    { "action": "support", "method": "POST", "path": "/InsertGrievenceTicket",   // TODO: path unconfirmed
      "mappings": "mappings/pmfby/grievance.support.yaml",
      "providerIdAt":     "message.support.channels[].providerId",   // see below
      "capabilityCodeAt": "message.support.channels[].@type",
      "timeoutMs": 30000, "status": "active" },
    { "action": "status",  "method": "POST", "path": "/GetGrievenceTicketsStatus", // TODO: path unconfirmed
      "mappings": "mappings/pmfby/grievance.status.yaml",
      "timeoutMs": 30000, "retryMax": 2, "status": "active" }
  ] } }
```

**PM-KISAN** — same shape, two actions instead of three, because there is no OTP leg.

```jsonc
{ "SchemaRegistry": {
  "capabilityCode": "openagrinet:PMKISANGrievance",
  "name": "PM-KISAN Grievance",
  "version": "v0.1",
  "schemaUrl": "https://raw.githubusercontent.com/OpenAgriNet/network-specs/main/api-schemas/PMKISANGrievance/v0.1/attributes.yaml",
  "status": "active"
} }

{ "Participant": {
  "participantId": "pmkisan",
  "name": "PM-KISAN Grievance Portal",
  "type": "upstream_api",
  "status": "active",
  "baseUrl": "https://pmkisan.gov.in"        // TODO: confirm host and base path
} }

{ "ProviderSchema": {
  "bindingKey":     "pmkisan|openagrinet:PMKISANGrievance",
  "participantId":  "pmkisan",
  "capabilityCode": "openagrinet:PMKISANGrievance",
  "status": "active",
  "actions": [
    { "action": "support", "method": "POST", "path": "/LodgeGrievance",
      "mappings": "mappings/pmkisan/grievance.support.yaml",
      "providerIdAt":     "message.support.channels[].providerId",   // see below
      "capabilityCodeAt": "message.support.channels[].@type",
      "timeoutMs": 30000, "status": "active" },
    { "action": "status",  "method": "POST", "path": "/GrievanceStatusCheck",
      "mappings": "mappings/pmkisan/grievance.status.yaml",
      "timeoutMs": 30000, "retryMax": 2, "status": "active" }
  ] } }
```

Worth noticing:

- **Routing on the lodge leg, and why both halves still come off the payload.**
  The adapter builds a binding key of `<participantId>|<capabilityCode>` and reads both
  halves out of the payload — by default from `message.contract.commitments[].offer.provider.id`
  and `…resourceAttributes.@type`. A `support` payload has no contract, so both re-path into
  the channel: `@type` for the capability, `providerId` for the participant. `Support` itself
  is sealed at three fields and none names a participant, but `channels` is an array of
  `Attributes` — the spec's extensibility container, `additionalProperties: true` — so the
  pack declares `providerId` there. `scheme.code` is not a substitute: it reads `PMFBY`, not
  `pmfby`, the lookup is an exact string match, and a scheme is not a participant.
  The objection to this is that it pushes a registry identifier into the experience layer,
  which is the thing the registry exists to hide. It does — but the experience layer already
  sends that identifier on `init` and `status`, in `offer.provider.id`, and it has to,
  because that is the value the binding key is built from. `support` was the only leg
  pretending otherwise. Naming the provider statically in the row would hide it on that one
  leg, at the cost of a third case in `BindingPaths` and of the one-channel guard below —
  which counts an array on the provider path that a static value would not have.
- **The path grammar has no indices.** It is segments separated by `.`, with `[]` meaning
  "look in each element" — no wildcards, filters or `[0]`. A row written `channels[0].@type`
  would not fail at startup; it would be read as a field literally named `channels[0]`, match
  nothing, and leave every lodge request unserved with nothing said. Hence `channels[]`.
- **`channels` carries exactly one entry on the lodge leg.** The adapter counts the first
  array on the provider path and refuses a payload with more than one, because one request
  maps to one upstream call. That suits us — the ask is a single complaint — but the refusal
  message is worded for commitments and will read oddly on a support payload.
- **`retryMax` is missing on `init` and `support`, deliberately.** It defaults to `0`.
  Retrying a lodge that timed out *after* the portal filed the grievance lodges a duplicate;
  retrying an `init` texts a second OTP that invalidates the one the farmer is typing. Only
  `status` retries. Duplicates hurt more on PM-KISAN, which issues no ticket number: two
  grievances filed on one identity on one date are indistinguishable, including to `status`,
  which matches on `filedOn`.
- **`path` is the lodge endpoint only.** PMFBY's `support` makes two upstream calls — the
  OTP verify belongs to the plugin, is selected by `bindingKey`, and is named in no field.
  `timeoutMs` covers the mapped call only, so the wall-clock a farmer waits on `support` is
  the sum of both budgets, not the 30 s in the row.
- **No credential is here, or anywhere in the registry.** Auth schemes and the `*Env`
  variable names that locate secrets live in the adapter config; the values live in its
  environment. A registry read cannot leak one — not because it is filtered, but because
  none of the three schemas has a field to hold one, which is why none carries
  `_osConfig.privateFields`.
- **Nothing goes `active` early.** The pack has to be published before its row is seeded,
  the plugin has to be deployed before PMFBY's `support` is enabled, and PM-KISAN's
  `bodyCodec` binding must wait for the codec — or every call ships plaintext to a portal
  that accepts only envelopes.

## Open

- **`/status` asks about a contract that was never confirmed.** Its precondition is
  explicit: a `/confirm` must have completed and an `/on_confirm` carrying a confirmed
  contract id must have been received. Lodging with `support` means that is never true, and
  on PM-KISAN there is no `init` either, so the `contract.id` presented at `status` is a
  UUID the provider has never seen. Take this to the network's spec authority.
- **`channels` is carrying something that is not a channel.** The spec defines it as
  "available support channels … such as phone, email, or chat endpoints", and we put a case
  record in it. It validates — `Attributes` requires only `@context` and `@type` — but it is
  a repurposing, and the reason is that `Support` has no `supportAttributes` the way
  `Commitment` has `commitmentAttributes`. There is no other slot. Raise it with the same
  authority, together with the item above.
- **No helpline is published by either scheme.** PMFBY's 14447 comes from the legacy voice
  prompts, not from a PMFBY page; PM-KISAN has none on file. No contact channel ships until
  one is sourced from the scheme itself.
- **The PMFBY case read is unverified** — see the note at the end of Step 3. It is the
  largest open item on this page and is independent of the lodge action.
