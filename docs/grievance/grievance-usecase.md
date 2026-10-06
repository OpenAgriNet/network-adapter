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
| `provider` | the portal — filed with on the way in, came from on the way out | in the attributes, because a `SupportAction` has no `Contract` | `commitments[].offer.provider`; the attributes do **not** repeat it |
| attributes | who is asking, and what the case record says | `channels`, selected by `@type` | `commitmentAttributes` |

`descriptor` is absent where there is nothing to describe: `init` asks for a challenge before
the farmer has stated anything, and a `status` request names a ticket rather than restating
the case.

One field covers both directions because on a grievance they are always the same party, and
it is the same shape in both places — `{ "id": …, "descriptor": { "name": … } }` — so
`.provider.id` reads the same on every leg. The adapter routes on `id` and ignores `name`,
so a stale name still routes correctly; the registry owns that word.

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

The OTP never reaches the lodge call and is never returned. PMFBY's ticket is not the
`orderId`: it arrives separately, in `ticketNo` on the channel. PM-KISAN issues no ticket at
all, which is why its case is read back by identity and the date it was filed.

---

## Reading `code` and `name`

Three fields below are a `code` + `name` pair: `scheme`, `grievanceCategory` and
`caseStatus`. They all follow one rule.

- **`code` is the network's word.** It comes from a list this network governs.
  Branch on it.
- **`name` is the Portal's own phrase, carried through untouched.** Show it. It
  is what the farmer would read on the Portal's own screen, so it may well say
  `"Open"` where the code says `UnderReview`.
- **A missing `name` means the Portal said nothing**, and the adapter derived the
  code rather than quoting a phrase. That is information, not an omission.

One field looks exactly like these and is not: a commitment's `status.descriptor`.
It is a `Descriptor` too, and it sits in the same payload as `caseStatus`.

It answers a different question. `caseStatus` says how the grievance is going.
`status.descriptor.code` is Beckn's own contract lifecycle — `DRAFT` while a
challenge is outstanding, `ACTIVE` from the moment the grievance is lodged — and
nothing the portal reports ever moves it.

So: `caseStatus` to tell the farmer anything; `status.descriptor` never.

---

## PMFBY

### Step 1 — `init`: ask for a challenge

**Request**

- Carries the farmer's phone in `applicantPhone`, and nothing else.
- No `descriptor`: the farmer has not stated a complaint yet.
- Commitment status is `DRAFT`. The caller mints `contract.id` here and reuses it on `status`.

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
      "status": { "descriptor": { "code": "DRAFT" } },
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

**`on_init`**

- Commitment stays `DRAFT`.
- The OTP is never returned. `challengeIssued` gives the mechanism and a masked destination.
- `method`, `sentTo` and `expiresAt` are all required on PMFBY, so all three can be relied on.
- Branch on `method`; do not hard-code "six digits". `AADHAAR_OTP` and `DEVICE_TOKEN` are in
  the same enum, and a portal that starts offering one answers with that method instead.
- `informationMode` is still `OnDemand`, even on a reply. `Direct` means a real case, and
  the pack requires `ticketNo`, `caseStatus` and `filedOn` wherever `Direct` appears — an
  acknowledgement has none of them.

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

**Next** — collect the OTP from the farmer, then `support`.

### Step 2 — `support`: lodge the grievance

**Request**

- Same `transactionId`, new `messageId`. No contract travels; a `SupportAction` composes none.
- `orderId` is `applicationNo` — the enrolment the complaint is against.
- `descriptor` is the whole complaint. `code` is load-bearing: `3.10` is the portal's
  category and sub-category joined by a dot, and the adapter splits it there into two
  upstream fields.
- `channels[0]` carries the `provider` to route to, what identifies the farmer, and the OTP
  in `challenge`.
- Exactly one channel. The adapter refuses a second: one request, one upstream call.

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
        "provider": {
          "id": "pmfby",
          "descriptor": { "name": "PMFBY Grievance Portal" }
        },
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

**`on_support`**

- `orderId` is echoed unchanged — a reply does not change what the complaint is against.
- `ticketNo` is the ticket the portal just issued, on the channel. The spec names no field
  for it, and the channel is where the scheme's own fields live.
- `informationMode` flips to `Direct`.
- `caseStatus`, `filedOn` and `provider` are the adapter's assertions, not the portal's;
  `descriptor` is the caller's own words echoed back.
- `challenge` and `applicantPhone` are dropped by the response allow-list.

The portal's lodge reply carries four things:

| upstream | what we do with it |
|---|---|
| success flag | decides whether this is an ACK at all |
| `ticket-no` | becomes `ticketNo` — the number the farmer quotes back |
| `ticket-id` | dropped; the portal's own row id, nothing later needs it |
| message | never returned, logged redacted |

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
        "provider": {
          "id": "pmfby",
          "descriptor": { "name": "PMFBY Grievance Portal" }
        },
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "ticketNo": "100626000099001",
        "caseStatus": { "code": "Registered" },
        "filedOn": "2026-09-28"
      }]
    }
  }
}
```

No helpline channel is emitted — neither scheme publishes one we have on file. If one is
confirmed later it becomes a second member of `channels`, which is why a consumer must
select the case record by `@type` rather than by position.

**Next** — store `contract.id`, `ticketNo` and the phone number. `status` needs all three.

### Step 3 — `status`: check the ticket

**Request**

- New `transactionId` — a separate session, days later. Same `contract.id` as `init`.
- No `descriptor`: this names a ticket, it does not restate the complaint.
- Sends `ticketNo` and `applicantPhone`.
- No OTP — it is spent once, at filing.

> **The read is not authenticated.** Anyone holding both the ticket number and the filing
> phone number can read the case. It is not enumerable, since a ticket is not guessable from
> a phone, but nothing proves the caller is the farmer.

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
      "status": { "descriptor": { "code": "ACTIVE" } },
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

**`on_status`**

- `contract.descriptor` carries the complaint as the portal holds it.
- `commitmentAttributes` carries the case record.
- `caseStatus.code` is the network's word; `caseStatus.name` is the portal's phrase, when it
  gives one.
- `caseRemark` is about the case rather than the complaint, so it stays in attributes. It is
  absent while nothing has been recorded, and PMFBY publishes no date against it, so there
  is no `remarkedOn`.
- The portal also returns the farmer's name, mobile number, email, full address down to the
  village, the insurance policy number and the insurer. All dropped — the response mapping
  is an allow-list, not a passthrough.

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
      "status": { "descriptor": { "code": "ACTIVE" } },
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
        "caseStatus": { "code": "UnderReview", "name": "Open" },
        "filedOn": "2026-09-28",
        "caseRemark": "Claim file reopened, awaiting surveyor report."
      }
    }]
  }}
}
```

> **This one reply is not verified.** Everything PMFBY *accepts* is confirmed against the
> live system; what it *returns on a case read* is not — the field names behind this payload
> appear in no source we hold. The Beckn side above will not change, but which upstream
> field feeds which value may. Confirm against PMFBY's own API document before building on
> it; `grievance-upstream-contracts.md` records what is verified and what is not, field by
> field. PMFBY's `init` and `support`, and all of PM-KISAN, are unaffected.

**Next** — repeat `status` on demand.

---

## PM-KISAN

- **No OTP leg.** The portal proves nothing about the caller, so neither does the network.
  `support` is the first call.
- **`orderId` is the registration number.** PM-KISAN has no application number and no case
  number — the upstream takes exactly one reference, `IdentityNo`. So the farmer's enrolment
  is both the identity the portal authenticates on and the thing the complaint is against,
  which is what `orderId` means. It goes in the slot PMFBY fills with `applicationNo` and is
  echoed unchanged on the reply. It stays `no-log` and `no-trace` either way.
- **Encryption is not yours to do.** PM-KISAN accepts and returns AES-GCM envelopes, not
  JSON: `{"EncryptedRequest": "…"}` in, `{"d": {"output": "…"}}` back. The adapter
  encrypts after the request mapping runs and decrypts before the response mapping runs, so
  the experience layer sends and receives only the plain Beckn payloads below. The key and
  IV are named by environment variable and held nowhere near the network.

> **The codec does not exist yet.** It is the one piece of new adapter machinery PM-KISAN
> needs, and until it ships nothing in this section can run. PMFBY is unaffected; it speaks
> ordinary JSON.

### Step 1 — `support`: lodge the grievance

**Request**

- `orderId` is the farmer's PM-KISAN registration number.
- `descriptor.code` is one of the pack's ten codes.
- `channels[0]` carries the `provider` to route to and the scheme, and nothing else:
  PM-KISAN asks for no phone and issues no challenge.

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
        "provider": {
          "id": "pmkisan",
          "descriptor": { "name": "PM-KISAN Grievance Portal" }
        },
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

**`on_support`**

- `orderId` is echoed as it went up, exactly as on PMFBY.
- No ticket — the portal issues none, so the case record comes back without a case
  identifier.
- The portal's lodge reply is `{ Responce, message }` and nothing more: no identifier, no
  date, no status. `Responce` becomes the ACK; `message` is logged redacted and never
  returned.
- So **nothing in the reply comes from the portal.** `descriptor` is the caller's own words
  echoed back, `caseStatus` and `filedOn` are the adapter's assertions, and `provider` is
  the registry entry the adapter routed to. The reply confirms receipt and nothing more.

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
        "provider": {
          "id": "pmkisan",
          "descriptor": { "name": "PM-KISAN Grievance Portal" }
        },
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "caseStatus": { "code": "Registered" },
        "filedOn": "2026-09-28"
      }]
    }
  }
}
```

**Next** — there is no ticket number to keep, and no `contract.id` either, since `support`
composes no contract. Keep the registration number and `filedOn`.

### Step 2 — `status`: read the replies

**Request**

- `contract.id` is a UUID the caller mints here — PM-KISAN has no earlier leg to mint one.
- The registration number rides in `commitmentAttributes.registrationNo` rather than
  `orderId`, because a `Contract` has no `orderId`.
- `filedOn` is the one from `on_support`, and it is required: the portal has no
  per-grievance endpoint, so the date is what picks this grievance out of the farmer's list.

> **Two grievances filed on the same identity on the same day are indistinguishable.** The
> portal issues nothing that would tell them apart.

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
      "status": { "descriptor": { "code": "ACTIVE" } },
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

**`on_status`**

- One commitment — the grievance this contract is about. The farmer's other grievances are
  filtered out; each has its own contract.
- `registrationNo` is not echoed.
- The farmer's name, father's name, gender, mobile number and address are dropped by the
  allow-list, and `Reg_No` with them: five of the record's fourteen fields survive.
- The record carries no category, so `descriptor` comes back with `longDesc` only — the
  description the portal stored — and no `code` or `name`.

```json
{
  "context": { "action": "on_status", "messageId": "e91f…", "…": "…" },
  "message": { "contract": {
    "id": "c9b31a45-0f78-4e2d-9a60-84b7d3e15c02",
    "descriptor": {
      "longDesc": "Third instalment for 2026 has not been credited."
    },
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE" } },
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
        "caseRemark": "Instalment released on 2026-10-01, credited to the linked account.",
        "remarkedOn": "2026-10-01"
      }
    }]
  }}
}
```

**Next** — repeat `status` on demand.

---

## Three rules that apply to both

- **`informationMode` tells you the direction.** `OnDemand` is a request, `Direct` is an
  answer carrying a real case. Read it rather than the action name.
- **Secrets go one way; identifiers do not.** `challenge` is `writeOnly` as a whole object —
  send it, never expect it back, never log it, and `challenge.value` never reaches the lodge
  call. `challengeIssued` is the `readOnly` other half: returned, never sent, never secret.
  `applicationNo` and `registrationNo` are identifiers rather than secrets and do come back,
  echoed in `orderId` on `on_support`; PMFBY's returns again in `commitmentAttributes` on
  `on_status`. Both stay `no-log` and `no-trace` either way. Every field carrying personal
  data is marked `x-oan-pii` in the pack, with a class and a handling list — documentation
  today, not something the validator enforces.
- **Nothing is closed yet.** Neither portal documents a terminal status, so no case read
  yields `Resolved`, `Rejected` or `Closed` today: an unrecognised phrase maps to
  `UnderReview`, never to a terminal code.

## Errors

Every error comes back on the same HTTP response, never on a later `on_*`.

| What went wrong | HTTP | `status` | `error.code` |
|---|---|---|---|
| A required field is missing | `400` | NACK | `SCH_REQUIRED_FIELD_MISSING` |
| Wrong OTP — PMFBY; the lodge call is never made | `400` | NACK | `BIZ_GENERIC_ERROR` |
| Portal rejects the request, or is unreachable | `502` | NACK | `NET_DOWNSTREAM_UNAVAILABLE` |
| Envelope will not decrypt — PM-KISAN | `500` | NACK | `NET_INTERNAL_ERROR` |
| No grievance found — the portal answered, and has no case matching the ticket or `filedOn` | `202` | ACK | `BIZ_NO_RESULTS_FOUND` |

**A failure** — `NACK`, with `details.path` naming the field:

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

**No grievance found** — the farmer asks about a ticket the portal has never heard of: a
mistyped digit, a different phone, or it was never filed. The portal was reached and
answered, so nothing failed and there is nothing for the caller to correct — but there is
no case to return either. That is an `ACK`, not a `NACK`: request accepted, no result,
nothing further coming.

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

Four things to know:

- **`details.path` follows the field, not the action.** A missing phone is
  `$.message.support.channels[0].applicantPhone` on `support` and
  `$.message.contract.commitments[0].commitmentAttributes.applicantPhone` on `status`. The
  pack's `x-beckn-path` is the source for both.
- **The portal's own message is never passed through.** It may hold a stack trace, an
  internal hostname, or a quoted-back credential. Logged redacted; we return our own.
- **A wrong OTP is not a `401`.** That code means the Beckn signature failed to verify,
  which is a different fault. The enum has no OTP-specific value. PMFBY publishes no clean
  success signal for the verify step either, so this row is provisional.
- **`502` is not declared by the spec.** `/init`, `/support` and `/status` list only
  `200, 202, 400, 401, 403, 429, 500`. The adapter returns `502` for an upstream failure,
  so we inherit it.

## Registry

Nothing above works until the two providers are registered. Three records each — what the
data is, who we call, and how.

**PMFBY**

```jsonc
{ "SchemaRegistry": {
  "capabilityCode": "openagrinet:PMFBYGrievance",
  "name": "PMFBY Grievance",
  "version": "v0.1",
  "schemaUrl": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/attributes.yaml",
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
      "providerIdAt":     "message.support.channels[].provider.id",  // [] is the grammar's
                                                                      // only plural: no index
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
  "schemaUrl": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/attributes.yaml",
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
      "providerIdAt":     "message.support.channels[].provider.id",  // [] is the grammar's
                                                                      // only plural: no index
      "capabilityCodeAt": "message.support.channels[].@type",
      "timeoutMs": 30000, "status": "active" },
    { "action": "status",  "method": "POST", "path": "/GrievanceStatusCheck",
      "mappings": "mappings/pmkisan/grievance.status.yaml",
      "timeoutMs": 30000, "retryMax": 2, "status": "active" }
  ] } }
```
