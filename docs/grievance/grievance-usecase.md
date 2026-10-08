# Grievance and application status — flow of execution

Three capabilities across two schemes, written as the journeys a farmer actually runs
into. Each scenario below is complete on its own: the calls in order, and the payloads
they carry. Everything that explains *why* a field is shaped the way it is sits in the
appendices.

> What the portals actually accept and return — one line of provenance per field — is
> `grievance-upstream-contracts.md`. Where this page and that one disagree, that one wins.

---

## Contents

1. [What this is](#1-what-this-is)
2. [Before any scenario — `discover`](#2-before-any-scenario--discover)
3. [PMFBY grievance](#3-pmfby-grievance) — `support`, then `status`
   - 3.1 [A farmer cannot get into the portal](#31-scenario--a-farmer-cannot-get-into-the-portal)
   - 3.2 [The same farmer checks his complaint four days later](#32-scenario--the-same-farmer-checks-his-complaint-four-days-later)
4. [PM-KISAN grievance](#4-pm-kisan-grievance) — `init` → `support`, then `init` → `status`
   - 4.1 [Filing a complaint, with the registration proved first](#41-scenario--filing-a-complaint-with-the-registration-proved-first)
   - 4.2 [Checking it four days later](#42-scenario--checking-it-four-days-later)
5. [PM-KISAN application status](#5-pm-kisan-application-status) — `init` → `status`
   - 5.1 [The instalment has not arrived](#51-scenario--the-instalment-has-not-arrived)

Appendices — the reference material the scenarios point at:

- A. [Where every field sits](#appendix-a--where-every-field-sits)
- B. [Reading `code`, `name` and the contract lifecycle](#appendix-b--reading-code-name-and-the-contract-lifecycle)
- C. [Required fields by call](#appendix-c--required-fields-by-call)
- D. [Rules that apply everywhere](#appendix-d--rules-that-apply-everywhere)
- E. [Errors](#appendix-e--errors)
- F. [What the adapter publishes](#appendix-f--what-the-adapter-publishes)
- G. [Registry](#appendix-g--registry)

---

## 1. What this is

Three capabilities, five scenarios:

| what | scheme | the farmer wants to | scenarios |
|---|---|---|---|
| File a grievance | PMFBY | complain about a crop-insurance enrolment or claim | §3.1, §3.2 |
| File a grievance | PM-KISAN | complain about an income-support registration | §4.1, §4.2 |
| Check application status | PM-KISAN | find out why an instalment has not arrived | §5.1 |

Three parties take part:

- **Experience layer** — the app or chatbot the farmer uses.
- **Adapter** — this network's piece. It turns Beckn calls into portal calls, and the
  answers back into Beckn.
- **Portal** — the scheme's own system. PMFBY and PM-KISAN each have one.

Every call is synchronous. You send a request and get the answer on the same HTTP
response. Nothing arrives later on a callback.

One rule shapes everything below: **the catalog says whether a desk asks for an OTP.**
PM-KISAN's two desks publish `["SMS_OTP"]` and PMFBY's publishes `[]`, which is why the
PM-KISAN scenarios have an extra call at the front. Branch on the published field rather
than on the scheme name — [§2](#2-before-any-scenario--discover).

At a glance, the three differ like this:

| | PMFBY grievance | PM-KISAN grievance | PM-KISAN application status |
|---|---|---|---|
| Calls | `support` → `status` | `init` → `support`, then `init` → `status` | `init` → `status` |
| Identity proof | none — PMFBY publishes no challenge | `SMS_OTP`, four digits, on the lodge **and** on the read | `SMS_OTP`, four digits, on the read |
| Farmer is known by | application number on the way in, ticket number on the way back; the phone is an unverified contact | registration number | registration, mobile or Aadhaar number — the caller says which |
| What comes back | ticket number, status, crop | ticket number, status, remarks | instalments paid, eKYC, what is blocking payment |
| `@type` | `openagrinet:PMFBYGrievance` | `openagrinet:PMKISANGrievance` | `openagrinet:PMKISANApplicationStatus` |

The two PM-KISAN capabilities share a desk, an OTP service and a base schema, and share no
subject. Keeping them apart is deliberate: a status read has no case, no ticket and no
lifecycle, and folding it into the grievance pack would have meant a payload whose `case`
band is permanently empty.

![Grievance flow of execution](grievance-flow.svg)

Both schemes are on that one page. Follow the `[PMFBY]` branches for §3, and the
`[PM-KISAN]` branches for §4 and §5.

---

## 2. Before any scenario — `discover`

`discover` runs once. It is not part of any scenario and you do not repeat it per
grievance. It asks the network what exists and returns a catalog.

```json
POST /discover
{
  "context": {
    "version": "2.0.0", "action": "discover", "networkId": "openagrinet",
    "transactionId": "0f3b7d21-8c45-4e96-b2a7-5d1c8e0f4a93",
    "messageId": "6a9e2c58-1d74-4b03-8f5a-7c2b9e6d0a14",
    "timestamp": "2026-09-20T09:12:00Z"
  },
  "message": {
    "intent": {
      "textSearch": "grievance",
      "filters": {
        "type": "jsonpath",
        "expression": "$.catalogs[*].resources[*] ? (@.resourceAttributes.scheme.code == \"PMFBY\")"
      }
    }
  }
}
```

The filter above asks for PMFBY. Put `PM-KISAN` in place of `PMFBY` for either PM-KISAN
scenario, or leave the filter out to get everything.

Filters are JSONPath, and only `resourceAttributes` is reachable from an expression:

```
"$.catalogs[*].resources[*] ? (@.resourceAttributes.scheme.code == \"PM-KISAN\")"
"$.catalogs[*].resources[*] ? (@.resourceAttributes.challengeMethods[*] == \"SMS_OTP\")"
```

No expression separates the two PM-KISAN desks: both carry `scheme.code: PM-KISAN` and
both publish `["SMS_OTP"]`. Tell them apart by `resourceAttributes.@type` in the reply.

`on_discover` returns the matching catalogs whole, `resourceAttributes` included, on the
same HTTP response. Shortened here to the parts the scenarios use:

```json
{
  "message": { "catalogs": [{
    "provider": {
      "id": "pmfby",
      "descriptor": { "code": "PMFBY", "name": "PMFBY Grievance Portal" }
    },
    "resources": [{
      "id": "res:pmfby:grievance",
      "resourceAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "challengeMethods": []
      }
    }],
    "offers": [{ "id": "off:pmfby:grievance", "resourceIds": ["res:pmfby:grievance"] }]
  }]}
}
```

What the scenarios take from that reply:

| from the reply | what it is for |
|---|---|
| `provider.id` | becomes `offer.provider.id`, and `channels[].provider.id` on `support` |
| `offers[].id`, `resources[].id` | quoted verbatim in every commitment |
| `resourceAttributes.@context` | the pack URL. Swap `context.jsonld` for `attributes.yaml` and it states every field, bound and value space the later payloads are held to |
| `resourceAttributes.challengeMethods` | whether this desk asks for an OTP |

Quote the ids exactly as they came. The full catalogs are
[Appendix F](#appendix-f--what-the-adapter-publishes).

`challengeMethods`, in that same excerpt, tells you which shape the scenario takes:

| `challengeMethods` | what it means | what you do |
|---|---|---|
| `[]` | no OTP | go straight to the first real call |
| `["SMS_OTP"]` | an OTP is needed | call `init` first, every time |

PMFBY publishes `[]`. Both PM-KISAN desks publish `["SMS_OTP"]`. **Branch on this field,
not on the scheme name.** A portal can change what it needs, and the catalog is where the
adapter states it on the portal's behalf. Branch on the values rather than on the length, too: a portal that adds a second
mechanism widens the list, and a caller that reads the method keeps working. An empty array
is not an oversight — it is this desk published as having nothing to ask for.

It does not say *which* calls are challenged. PM-KISAN challenges the lodge and the read
alike; that, and everything else a particular call must carry, is
[Appendix C](#appendix-c--required-fields-by-call). Why PMFBY has no challenge at all is
[Appendix D.1](#d1-why-pmfby-publishes-no-challenge).

---

## 3. PMFBY grievance

### 3.1 Scenario — a farmer cannot get into the portal

Ramesh enrolled a Kharif paddy crop in 2026. He cannot sign in to the PMFBY portal to look
at that enrolment, so he raises a complaint about it.

He has two things: his application number, and the mobile number he enrolled with. He has
no ticket number yet.

He sends one call and gets a ticket number back. That ticket, together with the phone
number he filed with, is the only way to find the complaint again later, which is §3.2.

    support → on_support          one round trip, no OTP

#### The call — `support`

- This is the first call, so you create the `transactionId` here. There is no contract:
  `support` does not use one.
- `orderId` holds the application number — the enrolment the complaint is about.
- The `grievance` band is the complaint itself. `category.code` and `subCategory.code` are
  two separate fields. Never join them into one string like `3.10`. Send the codes on
  their own; the portal reads ids only, and a `name` sent beside them is dropped.
- `channels[0]` carries the provider to route to, plus everything that identifies the
  enrolment. There is no `challenge` here. PMFBY's grievance service has no OTP endpoint,
  so `applicantPhone` is just a contact number the portal files on the ticket. Nobody has
  proved it belongs to the caller.
- `cropYear` and `season` say *which enrolment*, not what went wrong. Both are required
  when filing. `season` is one of `Kharif`, `Rabi` or `Zaid`, and the adapter maps the name
  to the number PMFBY wants. Neither field comes back on a read.
- `complaintDate` is optional. Leave it out and the adapter stamps today's date in IST,
  which is what a live submission wants. Send it only when the farmer filed earlier — a
  form taken offline and replayed, say. It comes back as `case.filedOn`, not under this
  name. Ramesh is filing live, so the payload below leaves it out.
- `receiptSourceId` is left out, which is normal. The adapter sends the id configured for
  itself. Send your own only if PMFBY gave you one directly.
- Send one channel. The adapter rejects a second. Beckn sets no limit on `channels`, so
  this is the adapter's rule, not the schema's.

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
      "channels": [{
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
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
        "grievance": {
          "category": { "code": "3" },
          "subCategory": { "code": "10" },
          "description": "Cannot log in to the PMFBY portal to view my Kharif 2026 enrolment."
        }
      }]
    }
  }
}
```

#### What comes back — `on_support`

- `orderId` comes back unchanged.
- `case.ticketNo` is the ticket the portal just issued.
- `informationMode` flips to `Direct`.
- `case.status`, `case.filedOn` and `provider` are the adapter's words, not the portal's.
  The `grievance` band is the caller's own text echoed back.
- `applicantPhone` is dropped. The response mapping is an allow-list, not a passthrough.

```json
{
  "context": { "action": "on_support", "messageId": "9d44…", "…": "…" },
  "message": {
    "support": {
      "orderId": "KA2026KH00123456",
      "channels": [{
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "Direct",
        "provider": {
          "id": "pmfby",
          "descriptor": { "name": "PMFBY Grievance Portal" }
        },
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "grievance": {
          "category": { "code": "3" },
          "subCategory": { "code": "10" },
          "description": "Cannot log in to the PMFBY portal to view my Kharif 2026 enrolment."
        },
        "case": {
          "ticketNo": "100626000099001",
          "status": { "code": "Registered" },
          "filedOn": "2026-09-28"
        }
      }]
    }
  }
}
```

Neither scheme publishes a helpline channel, so only one channel comes back. If one is
added later it becomes a second entry in `channels`. **Pick the case record by `@type`,
never by position.**

**Keep for later:** the ticket number from `case.ticketNo`, and the phone number you sent.
The phone is not in the response, so the app has to remember what it submitted. Those two
are all §3.2 needs from this call; the ids it quotes come from the catalog.

### 3.2 Scenario — the same farmer checks his complaint four days later

Ramesh wants to know what happened. He has closed the app since. All he has is the ticket
number and the phone number he filed with.

This is a new session. Nothing of Ramesh's carries over from §3.1 except those two values;
the offer and resource ids below come from the catalog, as they do on every call.

    status → on_status            one round trip, no OTP

#### The call — `status`

- New `transactionId`, because this is a separate session four days later, and a new
  `contract.id` made with it. The filing call made no contract, so there is nothing to
  carry over. Nothing upstream reads either value.
- No `grievance` band. This call names a ticket; it does not repeat the complaint.
- It sends `case.ticketNo` and `applicantPhone`. Those two are the entire upstream request.
- No `challenge`. There is no OTP anywhere in PMFBY.
- `status.descriptor.code` is the *contract's* lifecycle, not the grievance's. You open at
  `ACTIVE` because the thing the contract is for already exists. PMFBY never sends `DRAFT`
  — that state means a challenge is waiting to be answered, and PMFBY issues none.
  Appendix B.

> **Nothing in PMFBY is authenticated.** Anyone holding a ticket number and the filing
> phone number can read the case. Reading is a little safer than filing, because you cannot
> guess a ticket number from a phone number. Why this network publishes no challenge rather
> than borrowing the OTP PMFBY does operate is
> [Appendix D.1](#d1-why-pmfby-publishes-no-challenge).

```json
POST /status
{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "transactionId": "b7e0…", "messageId": "2f8c…",
    "timestamp": "2026-10-02T09:02:11Z"
  },
  "message": { "contract": {
    "id": "c4a9e7d2-6b18-4f35-9a80-1e5d3c7b0f24",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE" } },
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
        "applicantPhone": "9876543210",
        "case": { "ticketNo": "100626000099001" }
      }
    }]
  }}
}
```

#### What comes back — `on_status`

- The `grievance` band is the complaint as the portal now holds it. The `case` band is the
  record the portal keeps against it.
- `enrolmentId` is the application number, and this is the first time you see it under that
  name. On `support` the same value travelled as `orderId`, because a `support` message has
  an `orderId` field and a `Contract` does not. One value, two places. The request did not
  send it — the portal returns it.
- `case.status.code` is this network's word. `case.status.name` is the portal's own phrase,
  when it gives one. Appendix B explains which to use.
- There is no remark. PMFBY returns a status phrase and no reply text, so the schema
  refuses both `case.remark` and `case.remarkedOn`.
- `case.cropName` is the insured crop. PMFBY returns it and no other scheme has it.
- The category comes back as a **name with no code**, because the portal returns
  `TicketCategoryName` and no id. PMFBY publishes no category master, so the wording below
  is only an example. Show whatever the portal returns.
- The portal also returns the farmer's name, state, district, the insurer, and an internal
  ticket key. All of it is dropped.

```json
{
  "context": { "action": "on_status", "messageId": "2f8c…", "…": "…" },
  "message": { "contract": {
    "id": "c4a9e7d2-6b18-4f35-9a80-1e5d3c7b0f24",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE" } },
      "offer": {
        "id": "off:pmfby:grievance",
        "provider": { "id": "pmfby", "descriptor": { "name": "PMFBY Grievance Portal" } },
        "resourceIds": ["res:pmfby:grievance"]
      },
      "resources": [{ "id": "res:pmfby:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "enrolmentId": "KA2026KH00123456",
        "grievance": {
          "category": { "name": "Enrollment / Portal Issues" },
          "subCategory": { "name": "Login" },
          "description": "Cannot log in to the PMFBY portal to view my Kharif 2026 enrolment."
        },
        "case": {
          "ticketNo": "100626000099001",
          "status": { "code": "UnderReview", "name": "Open" },
          "filedOn": "2026-09-28",
          "cropName": "Paddy"
        }
      }
    }]
  }}
}
```

Ramesh can repeat this call whenever he likes. Nothing closes a grievance from his side.

---

## 4. PM-KISAN grievance

Filing a grievance and reading one back are two separate journeys, days apart, and both
are challenged. Nothing the second journey *needs* survives from the first but the
registration number and the date the grievance was filed.

- **No phone number crosses the network.** The caller never sends one, and the
  acknowledgement names none — not even masked. PM-KISAN does not disclose where it sent
  the OTP.
- **Encryption is handled by the adapter.** PM-KISAN exchanges encrypted envelopes with
  its portal. Callers send and receive the plain Beckn payloads shown below and see none
  of it.

### 4.1 Scenario — filing a complaint, with the registration proved first

Suresh is registered under PM-KISAN in Uttar Pradesh. The third instalment for 2026 never
reached his bank account, so he raises a complaint about it.

He has one thing: his registration number. He does not type a phone number anywhere — the
portal already holds one against that registration, and that is where the OTP goes.

Because PM-KISAN publishes `["SMS_OTP"]`, filing takes two calls instead of one. The first
asks for the OTP. The second carries it, together with the complaint.

    init(enrolmentId) → on_init(challengeIssued) → support(+ challenge) → on_support

#### The call — `init`

- Carries the registration number in `enrolmentId`, and nothing else.
- No phone number: the portal texts the mobile it already holds against that registration.
- No `grievance` band — the farmer has not stated a complaint yet.
- Commitment status is `DRAFT`. The caller mints `contract.id` here. The lodge that
  follows has no contract to carry it into, so this one ends with the `init`; the read
  later on opens its own.

```json
POST /init
{
  "context": {
    "version": "2.0.0", "action": "init", "networkId": "openagrinet",
    "transactionId": "7f3a…", "messageId": "1b85…",
    "timestamp": "2026-09-28T10:58:00Z"
  },
  "message": { "contract": {
    "id": "5e2704c8-9d31-4f6a-b8c0-1a73e6d2f094",
    "commitments": [{
      "status": { "descriptor": { "code": "DRAFT" } },
      "offer": {
        "id": "off:pmkisan:grievance",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:grievance"]
      },
      "resources": [{ "id": "res:pmkisan:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "enrolmentId": "UP12345678A"
      }
    }]
  }}
}
```

#### What comes back — `on_init`

- Commitment stays `DRAFT`. The OTP is never returned.
- `challengeIssued` carries `method` and `expiresAt` only. **There is no `sentTo`** — PM-KISAN
  does not disclose the number it texted, and the pack refuses the field rather than invent a
  mask the farmer could not recognise. No capability on this page returns one.
- **`expiresAt` is the adapter's, not the portal's.** The OTP call answers with a success flag
  and a sentence, and names no expiry. The adapter stamps its own window so a caller has
  something to count down against; treat it as this network's promise about when it will stop
  accepting the OTP, not as a deadline the portal published.
- Branch on `method`; do not hard-code "four digits".
- `informationMode` stays `OnDemand` — an acknowledgement is not a case.

```json
"commitmentAttributes": {
  "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
  "@type": "openagrinet:PMKISANGrievance",
  "informationMode": "OnDemand",
  "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
  "challengeIssued": {
    "method": "SMS_OTP", "expiresAt": "2026-09-28T11:08:00Z"
  }
}
```

#### The call — `support`

- `orderId` is the farmer's PM-KISAN registration number — the same one that went up on `init`.
- `grievance.category.code` is one of the pack's ten codes. Send the code on its own: the
  portal reads `GrievanceType` and nothing else, and a `name` beside it is dropped, exactly
  as on PMFBY.
- `challenge.value` is the four-digit OTP from `on_init`. It never reaches the portal's
  lodge call and is never echoed back.
- `channels[0]` carries the `provider` to route to, the scheme, the `grievance` band and the
  `challenge`. There is no phone number and no contract — `support` composes neither.

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
      "channels": [{
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "provider": {
          "id": "pmkisan",
          "descriptor": { "name": "PM-KISAN Grievance Portal" }
        },
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "grievance": {
          "category": { "code": "G003" },
          "description": "Third instalment for 2026 has not been credited."
        },
        "challenge": { "method": "SMS_OTP", "value": "4821" }
      }]
    }
  }
}
```

`grievance.category.code` is a closed list, `G001`–`G010`, and the pack holds it as an enum.
There is no `subCategory`: PM-KISAN classifies one level deep, and the pack refuses the
field outright.

```
G001 account number not correct        G006 gender not correct
G002 online application pending        G007 payment related
G003 installment not received          G008 problem in OTP-based eKYC
G004 transaction failed                G009 problem in biometric eKYC
G005 problem in Aadhaar correction     G010 problem in facial eKYC
```

#### What comes back — `on_support`

- `orderId` is echoed as it went up, exactly as on PMFBY.
- `case.ticketNo` is the portal's — the lodge reply carries `GrievanceID`, or `GrievanceNo`
  where that is absent. The portal uses both names for the same handle; whichever arrives
  lands here.
- The success sentinel arrives under `Status`, `Responce` or `Rsponce`; `"False"` is a
  refusal and becomes a NACK. The prose arrives under `Message`, `message` or `Remark`, and
  is logged redacted, never returned.
- The portal sends no date and no status on a lodge, so `case.status` and `case.filedOn`
  are the adapter's assertions, and the `grievance` band is the caller's own words echoed
  back.

```json
{
  "context": { "action": "on_support", "messageId": "4a1e…", "…": "…" },
  "message": {
    "support": {
      "orderId": "UP12345678A",
      "channels": [{
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "Direct",
        "provider": {
          "id": "pmkisan",
          "descriptor": { "name": "PM-KISAN Grievance Portal" }
        },
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "grievance": {
          "category": { "code": "G003" },
          "description": "Third instalment for 2026 has not been credited."
        },
        "case": {
          "ticketNo": "PMK2026091234",
          "status": { "code": "Registered" },
          "filedOn": "2026-09-28"
        }
      }]
    }
  }
}
```

**Keep for later:** the registration number, and `case.filedOn`. The read four days later
is matched on those two. Keep `case.ticketNo` as well, to show the farmer. `support`
composes no contract of its own, so there is nothing else to carry forward.

### 4.2 Scenario — checking it four days later

Suresh wants to know what came of it. He closed the app four days ago and comes back
holding two things: his registration number, and the date he kept from `case.filedOn`.
Everything else is asked for again — a fresh `init`, a fresh OTP, a fresh `contract.id`.

A read is challenged exactly as the filing was. A registration number on its own would
return every grievance ever lodged against it, so PM-KISAN asks for an OTP here too.

    init(enrolmentId) → on_init(challengeIssued) → status(+ challenge) → on_status

#### The first call — `init`, again

The same request as in
[§4.1](#41-scenario--filing-a-complaint-with-the-registration-proved-first) — the registration number
and nothing else, status `DRAFT`, no `grievance` band. Two things differ, and both show the
session is new: the `transactionId`, and `contract.id`, which is minted here and is the one
the `status` below carries.

```json
POST /init
{
  "context": {
    "version": "2.0.0", "action": "init", "networkId": "openagrinet",
    "transactionId": "c04b…", "messageId": "a7d2…",
    "timestamp": "2026-10-02T09:09:30Z"
  },
  "message": { "contract": {
    "id": "c9b31a45-0f78-4e2d-9a60-84b7d3e15c02",
    "commitments": [{
      "status": { "descriptor": { "code": "DRAFT" } },
      "offer": {
        "id": "off:pmkisan:grievance",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:grievance"]
      },
      "resources": [{ "id": "res:pmkisan:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "enrolmentId": "UP12345678A"
      }
    }]
  }}
}
```

`on_init` answers exactly as in §4.1: `method` and `expiresAt`, no `sentTo`.

#### The call — `status`

- Carries the OTP from the `init` above, not the one from §4.1. That one was spent on the
  lodge, and its window closed four days ago.
- `contract.id` is the one that `init` minted — a new value, not the one from the session
  that filed.
- The registration number rides in `commitmentAttributes.enrolmentId` rather than
  `orderId`, because a `Contract` has no `orderId`.
- `case.filedOn` is the one from `on_support`, and it is required: the portal has no
  per-grievance endpoint, so the date is what picks this grievance out of the farmer's list.

> **Two grievances filed on the same identity on the same day are indistinguishable.** The
> portal issues nothing that would tell them apart.

> **Open with PM-KISAN — is an OTP single-use?** This network assumes it is, which is why
> a read always opens its own `init`. The portal's request shapes do not say either way,
> and the v1 adapter's error text — *"incorrect, expired, or already used"* — only shows
> that v1 assumed it too. If the portal does accept an OTP more than once inside its
> validity window, a farmer who files and then checks straight away could be spared a
> second SMS.

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
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "enrolmentId": "UP12345678A",
        "case": { "filedOn": "2026-09-28" },
        "challenge": { "method": "SMS_OTP", "value": "7390" }
      }
    }]
  }}
}
```

#### What comes back — `on_status`

- One commitment — the grievance this contract is about. The farmer's other grievances are
  filtered out; each has its own contract.
- `enrolmentId` is not echoed, and neither is the `challenge`.
- The farmer's name, father's name, gender, mobile number and address are dropped by the
  allow-list, and `Reg_No` with them: five of the record's fourteen fields survive.
- The record carries no category, so the `grievance` band comes back with `description`
  alone.
- **No `case.ticketNo` comes back.** The portal's status call answers per identity rather
  than per ticket and does not repeat the handle on each record. The ticket Suresh kept
  from `on_support` is still the one to show him; this read will not return it.
- **`"Replied"` under `"Disposed"` is the rule working, not a mapping error.** The portal's
  phrase reads finished; the code does not, because an adapter may infer a state but never
  that a case is over. Show Suresh the word `Disposed`, branch on `Replied`, and leave the
  contract `ACTIVE` — Appendix B.

```json
{
  "context": { "action": "on_status", "messageId": "e91f…", "…": "…" },
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
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "grievance": {
          "description": "Third instalment for 2026 has not been credited."
        },
        "case": {
          "status": { "code": "Replied", "name": "Disposed" },
          "filedOn": "2026-09-28",
          "remark": "Bank account seeded with Aadhaar; payment in next cycle.",
          "remarkedOn": "2026-10-01"
        }
      }
    }]
  }}
}
```

Suresh can repeat this call whenever he likes. Nothing closes a grievance from his side,
and every read needs an OTP of its own.

---

## 5. PM-KISAN application status

The same portal and the same OTP service as the grievance capability, asked a different
question. Nothing is filed here: there is no `support` leg, no ticket and no lifecycle,
and the pack refuses the `grievance` and `case` bands outright — a payload carrying either
is told so rather than quietly ignored. [Appendix A](#appendix-a--where-every-field-sits)
has the two bands it uses instead.

- **The OTP is shared with the grievance capability.** The same service issues it and the
  same four digits satisfy it. Whether one already issued can be spent twice is the
  portal's business and undocumented; this network assumes single use and opens a fresh
  `init` for every read — [§4.2](#42-scenario--checking-it-four-days-later).
- **Aadhaar is accepted here and refused on the grievance capability.** This is a read of
  the farmer's own record behind an OTP, and the upstream genuinely takes an Aadhaar number
  as a lookup key. Neither it nor any token derived from it is ever logged, traced or
  returned.

### 5.1 Scenario — the instalment has not arrived

Lakshmi is registered under PM-KISAN. Her last instalment has not arrived and she does not
know why. She is not complaining about anything — she wants to see what the portal holds
against her registration.

She knows her mobile number but not her registration number, and that is enough. This
capability accepts a registration number, a mobile number or an Aadhaar number, and the
caller says which of the three it is sending. The registration number comes back in the
answer.

    init(applicant) → on_init(challengeIssued) → status(+ challenge) → on_status

#### The call — `init`

- Carries `applicant` and nothing else. Where the OTP goes follows from `idType`: for
  `Registration` and `Aadhaar` the portal looks up the mobile it holds and the caller never
  learns it; for `Mobile` the identifier *is* a phone number and the OTP goes there. The
  example below is a `Mobile` lookup, which is why one appears in the payload.
- `applicant.idType` is mandatory. The pack checks `applicant.id` against the shape that
  type requires, and refuses a mismatch before the portal is called.
- Commitment status is `DRAFT`. The caller mints `contract.id` here and quotes the same
  value on `status`.

```json
POST /init
{
  "context": {
    "version": "2.0.0", "action": "init", "networkId": "openagrinet",
    "transactionId": "4c81…", "messageId": "9e27…",
    "timestamp": "2026-10-07T10:58:00Z"
  },
  "message": { "contract": {
    "id": "a9f3b062-5e17-4c84-9d20-6b48f1c7e053",
    "commitments": [{
      "status": { "descriptor": { "code": "DRAFT" } },
      "offer": {
        "id": "off:pmkisan:application-status",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:application-status"]
      },
      "resources": [{ "id": "res:pmkisan:application-status", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANApplicationStatus",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "applicant": { "idType": "Mobile", "id": "9812345670" }
      }
    }]
  }}
}
```

#### What comes back — `on_init`

- Commitment stays `DRAFT`. The OTP is never returned.
- `challengeIssued` carries `method` and `expiresAt` only. **No `sentTo`** — PM-KISAN does
  not disclose the number it texted, on either capability. `expiresAt` is the adapter's
  window, not a portal deadline — see §4.1.
- `applicant` is **not** echoed. The caller already knows what they sent, and on a `Mobile`
  lookup echoing it would put the number back on the wire for no gain.
- `informationMode` stays `OnDemand` — an acknowledgement carries no record.

```json
"commitmentAttributes": {
  "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
  "@type": "openagrinet:PMKISANApplicationStatus",
  "informationMode": "OnDemand",
  "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
  "challengeIssued": {
    "method": "SMS_OTP", "expiresAt": "2026-10-07T11:08:00Z"
  }
}
```

#### The call — `status`

- The same `applicant` that went up on `init`, plus the four-digit OTP.
- `contract.id` is the one minted on `init`. Commitment moves to `ACTIVE`.
- `challenge.value` never reaches the status call upstream: the adapter spends it on the
  portal's verify endpoint first and sends nothing of it onward. It is never logged,
  traced or echoed.

```json
POST /status
{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "transactionId": "4c81…", "messageId": "b530…",
    "timestamp": "2026-10-07T11:02:00Z"
  },
  "message": { "contract": {
    "id": "a9f3b062-5e17-4c84-9d20-6b48f1c7e053",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE" } },
      "offer": {
        "id": "off:pmkisan:application-status",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:application-status"]
      },
      "resources": [{ "id": "res:pmkisan:application-status", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANApplicationStatus",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "applicant": { "idType": "Mobile", "id": "9812345670" },
        "challenge": { "method": "SMS_OTP", "value": "4827" }
      }
    }]
  }}
}
```

#### What comes back — `on_status`, payment blocked

- `informationMode` flips to `Direct`: this one carries a real record.
- `enrolmentId` is the registration number the portal **resolved the lookup to**. The
  caller sent a mobile number; the registration comes back. This is the only identifier
  returned, and the only capability on this page where `enrolmentId` is answer-only.
- `applicant` is not echoed.
- `blockers` is a **set**. Two blockers are two separate things for the farmer to fix.
- Commitment moves to `CLOSED` — the question is answered and nothing remains open.

```json
{
  "context": { "action": "on_status", "messageId": "b530…", "…": "…" },
  "message": { "contract": {
    "id": "a9f3b062-5e17-4c84-9d20-6b48f1c7e053",
    "commitments": [{
      "status": { "descriptor": { "code": "CLOSED" } },
      "offer": {
        "id": "off:pmkisan:application-status",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:application-status"]
      },
      "resources": [{ "id": "res:pmkisan:application-status", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANApplicationStatus",
        "informationMode": "Direct",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "enrolmentId": "UP45678901C",
        "application": {
          "registeredOn": "2021-06-14T00:00:00+05:30",
          "latestInstallmentPaid": 15,
          "ekyc": { "code": "Pending", "name": "eKYC not completed" },
          "blockers": [
            { "code": "LandSeedingPending", "name": "Land Seeding, KYS" },
            { "code": "Other", "name": "Bank account details could not be verified" }
          ]
        }
      }
    }]
  }}
}
```

#### What comes back — `on_status`, nothing blocking

`blockers` present and empty is the clear answer. Same shape, the array is `[]` — another
farmer's record, which is why the other values differ too:

```json
"commitmentAttributes": {
  "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
  "@type": "openagrinet:PMKISANApplicationStatus",
  "informationMode": "Direct",
  "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
  "enrolmentId": "UP77554433D",
  "application": {
    "registeredOn": "2021-02-08T00:00:00+05:30",
    "latestInstallmentPaid": 16,
    "ekyc": { "code": "Done", "name": "eKYC completed" },
    "blockers": []
  }
}
```

#### Reading the answer

| | meaning |
|---|---|
| `blockers` **absent** | the check was never run — the record came back and the blocker lookup did not |
| `blockers: []` | the check ran and found nothing. Payment is clear |
| `blockers: [ … ]` | these are the reasons payment is not arriving |

The first two are different answers and v1 rendered them the same. Do not treat an absent
array as an empty one.

One Beckn `status` is two reads upstream: the record, then the blockers. Only the first is
mandatory, so a reply can carry a complete `application` with no `blockers` key — the
registration was found, but why payment is held up could not be established. Say so;
do not report it as clear.

`blockers[].code` is governed by the network — `IncomeTaxPayee`, `LandSeedingPending`,
`Other` — and is what you branch on. `blockers[].name` is the portal's phrase, verbatim,
and is what you show. Only two codes are governed because only two were ever observed
upstream; everything else arrives as `Other` with the portal's own wording intact, so an
`Other` blocker is still fully readable.

`application.ekyc.code` is `Done` or `Pending`. The upstream sends `Y` or `N`; anything the
adapter does not recognise becomes `Pending`, never a guess at `Done`.

`latestInstallmentPaid` is a count of instalments paid, not an instalment number. `0` is an
answer, not a missing value.

`registeredOn` is a date-time because the portal's `DateOfRegistration` carries a time, but it
carries no offset — it is a bare local timestamp, in practice midnight. The adapter reads it as
IST and emits a fully-qualified instant, so the `+05:30` in the examples is the adapter's
conversion and not something the portal stated. Render the calendar date; do not present the time as
meaningful.

#### What the portal returns and this capability does not

The upstream record also carries the farmer's name, father's name, date of birth, gender,
full address, and state, district, sub-district and village. **None of it is modelled and
none of it is emitted.** The caller already knows who they asked about, and returning it
would disclose more than the identity the design goes to some trouble to withhold. The
response mapping is an allow-list, not a passthrough.

This is a deliberate break from the v1 flow, which showed the farmer their own name and
village back to them.

Lakshmi can ask again whenever she likes. Each read opens a contract of its own and needs
a fresh OTP: `CLOSED` closes that one question, not the capability.

---

## Appendix A — Where every field sits

**The band a field sits in says who wrote it.**

Everything the grievance is about travels in the attributes, on every call. Learn these five
bands once and every grievance example on this page reads the same way; application status
swaps two of them, and that is at the foot of this appendix.

| band | holds | written by |
|---|---|---|
| top level | who is asking and about what: `informationMode`, `provider`, `scheme`, `enrolmentId`, and the PMFBY-only `applicantPhone`, `cropYear`, `season` | the caller |
| `grievance` | what the farmer submitted: `category`, `subCategory`, `description` | the farmer |
| `case` | the record the portal holds: `ticketNo`, `status`, `filedOn`, and, by scheme, `cropName` or `remark` + `remarkedOn` | the portal |
| `challenge` | proof of identity, on the way in only — PM-KISAN only | the caller |
| `challengeIssued` | the acknowledgement of that proof, on the way out only — PM-KISAN only | the portal |

The `case` band differs by scheme. PMFBY returns `cropName` and never a remark — its pack
refuses `remark` and `remarkedOn`. PM-KISAN returns `remark` and `remarkedOn` and has no
crop. Both return `status` and `filedOn`. `ticketNo` comes back on every PMFBY call and on
PM-KISAN's lodge, but not on a PM-KISAN read — that call answers per identity, not per
ticket, and does not repeat the handle on each record.

The two challenge bands belong to PM-KISAN alone. PMFBY's grievance service has no OTP
endpoint, so its pack carries neither band and its catalog publishes no challenge —
[§2](#2-before-any-scenario--discover). PM-KISAN's OTP is four digits and its
`challengeIssued` has no `sentTo` at all — the portal does not say where it sent the OTP.

**PM-KISAN application status swaps two bands for two others.** It refuses `grievance` and
`case` — nothing is filed there and nothing has a lifecycle — and adds `applicant`, which
is who to look up and by what kind of identifier, written by the caller, and `application`,
which is what the portal holds, written by the portal. The three bands it keeps unchanged
are the top level, `challenge` and `challengeIssued`. One top-level field changes hands:
`enrolmentId` is written by the *portal* there, not the caller. [§5](#5-pm-kisan-application-status).

Nothing rides in a Beckn `descriptor`. The category, the sub-category and the farmer's
words are attribute fields, so the pack bounds each one.

| | on `support` | on `init` / `status` |
|---|---|---|
| the bands above | `support.channels[0]`, selected by `@type` | `commitmentAttributes` |
| `orderId` | `enrolmentId`, echoed unchanged on the reply | — (`Contract` has no `orderId`) |
| `provider` | in the attributes, because a `SupportAction` has no `Contract` | `commitments[].offer.provider`; the attributes do **not** repeat it |

A band is absent when there is nothing in it. PM-KISAN's `init` asks for a challenge before
the farmer has stated anything, so it carries no `grievance`; an ask carries no `case`, because only the
portal writes one.

`provider` is the same shape everywhere — `{ "id": …, "descriptor": { "name": … } }`. The
adapter routes on `id`; `name` is display text.

**On `init` and `status` the envelope is a Beckn `Contract` holding one commitment**, and
the payload rides on `commitmentAttributes`. The commitment's one resource stays thin: the
resource id from the catalog, plus the `quantity` the spec requires — send `{"count": 1}`
in both directions. That id is fixed per provider and names the catalog entry, never the case.

**On `support` there is no contract** — a `SupportAction` has no `contract` property. The
payload rides in `support.channels[0]`. On the ask there is exactly one channel and it is
the complaint; the adapter refuses a second. On a reply, address the entry by `@type` rather
than by index — a helpline channel could sit beside the case.

---

## Appendix B — Reading `code`, `name` and the contract lifecycle

Six fields are a `code` + `name` pair: `scheme`, `grievance.category`,
`grievance.subCategory`, `case.status`, and — on application status — `application.ekyc`
and each entry of `application.blockers`. They all follow one rule.

- **`code` is the network's word.** It comes from a list this network governs.
  Branch on it.
- **`name` is the portal's own phrase, carried through untouched.** Show it. It
  is what the farmer would read on the portal's own screen, so it may well say
  `"Open"` where the code says `UnderReview`.
- **A missing `name` means the portal said nothing**, and the adapter derived the
  code rather than quoting a phrase. That is information, not an omission.

One field looks exactly like these and is not: a commitment's `status.descriptor`.
It sits in the same payload as `case.status`.

It answers a different question. `case.status` says how the grievance is going.
`status.descriptor.code` is Beckn's own contract lifecycle — `DRAFT` while a
challenge is outstanding, `ACTIVE` once the thing the contract is for is under way,
`CLOSED` when it can go no further — and nothing the portal reports ever moves it.
PMFBY, having no challenge, never sends `DRAFT`: its only contract leg is `status`,
and that one opens `ACTIVE`.

A grievance contract stops at `ACTIVE` and stays there: the case is live, the portal
may reply again, and a later read reopens nothing. Only the application-status read
reaches `CLOSED`, because it is a single question with a single answer — §5.1.

So: `case.status` to tell the farmer anything; `status.descriptor` never.

---

## Appendix C — Required fields by call

The schema pack states the shape of every field — its type, its pattern, its closed value
space. What a *particular* call must carry is declared in the pack as
`x-oan-required-by-action` and enforced by the adapter; the tables below are the readable
form of it. A missing field is a `400` NACK with `SCH_REQUIRED_FIELD_MISSING`, returned
before the portal is called.

Two things are required on every call and are not repeated in the tables:

- **The attribute object** — `commitmentAttributes` on `init` and `status`,
  `support.channels[0]` on `support` — always carries `@context`, `@type`,
  `informationMode` and `scheme`.
- **The provider id** — `commitments[0].offer.provider.id`, or `channels[0].provider.id`
  on `support`. It routes the request; a payload without it is rejected as unroutable.

On `init` and `status` the `Contract` skeleton is also required: `contract.id`,
`commitments[0].status.descriptor.code`, `offer.id`, `offer.resourceIds`,
`resources[0].id` and `resources[0].quantity`. The offer and resource ids come from the
catalog and never change. `contract.id` is the network's own requirement — Beckn asks only
for `commitments` — and it is a correlation handle for the caller's own use. No adapter
forwards it and no portal reads it.

### C.1 PMFBY grievance

There is no `init` column: PMFBY publishes no challenge, so the flow opens at `support`.

| field | `support` | `status` |
|---|---|---|
| `contract.id` | — no contract | required — caller-minted |
| `support.orderId` | required — the application number | — |
| `applicantPhone` | required | required |
| `cropYear` | required — four digits | — |
| `season` | required — `Kharif`, `Rabi` or `Zaid` | — |
| `complaintDate` | optional — `YYYY-MM-DD`; defaults to today in IST | — |
| `receiptSourceId` | optional — your PMFBY channel id; defaults to the adapter's | — |
| `grievance.category.code` | required | — |
| `grievance.subCategory.code` | required | — |
| `grievance.description` | required — 10 to 2000 characters | — |
| `case.ticketNo` | — | required — from `on_support` |

### C.2 PM-KISAN grievance

| field | `init` | `support` | `status` |
|---|---|---|---|
| `contract.id` | required — caller-minted | — no contract | required — the same value as the `init` before it |
| `support.orderId` | — | required — the registration number | — |
| `enrolmentId` | required — the registration number | — | required — the same registration number |
| `grievance.category.code` | — | required — one of `G001`–`G010` | — |
| `grievance.description` | — | required — 10 to 2000 characters | — |
| `challenge.method` | — | required — `SMS_OTP` | required — `SMS_OTP` |
| `challenge.value` | — | required — the OTP the farmer received | required — a fresh OTP |
| `case.filedOn` | — | — | required — from `on_support` |

`challenge` is the real difference from PMFBY, which has none on either leg. PM-KISAN
challenges the read as well as the lodge because its read is matched on a registration
number, which returns everything filed against it; PMFBY's is matched on a ticket number
only the filer holds.

PM-KISAN uses no `applicantPhone`, `cropYear` or `season`, and its pack refuses
`grievance.subCategory` outright.

### C.3 PM-KISAN application status

| field | `init` | `status` |
|---|---|---|
| `contract.id` | required — caller-minted | required — the same value as the `init` before it |
| `applicant.idType` | required — `Registration`, `Mobile` or `Aadhaar` | required — the same value |
| `applicant.id` | required — in the shape its type names | required — the same value |
| `challenge.method` | — | required — `SMS_OTP` |
| `challenge.value` | — | required — the four-digit OTP from `on_init` |

There is no `support` row: this capability has no `support` leg, so nothing moves into
`Support.orderId` and no field carries an `x-beckn-path`. `enrolmentId` appears only on the
way back, as the registration the portal resolved the lookup to — a caller never sends it.

`applicant.idType` is required because the portal infers the kind of identifier from the
*shape* of the value: ten digits beginning 6–9 is read as a mobile number, twelve digits as
an Aadhaar number, anything else as a registration number. A registration number that
happens to look like a phone number therefore becomes a phone lookup, silently, and returns
nothing or somebody else's record. Declaring the type closes that off, and a value that does
not match its declared type is rejected before the portal is called:

| `idType` | the shape required | sent upstream as |
|---|---|---|
| `Registration` | ASCII alphanumeric | `Ben_id` |
| `Mobile` | `^[6-9][0-9]{9}$` | `Mobile` |
| `Aadhaar` | twelve digits | `Aadhar` — the upstream's spelling, not ours |

Everything else is optional. The `name` beside any `code`, `provider.descriptor.name` and
`scheme.name` are display text: send them or omit them, the adapter matches on `code` and
`id`.

---

## Appendix D — Rules that apply everywhere

- **`informationMode` tells you the direction.** `OnDemand` is a request, `Direct` is an
  answer carrying a real record. Read it rather than the action name. An OTP
  acknowledgement is `OnDemand` on both capabilities that issue one, even though it comes
  back from the portal: it carries no record, and `Direct` is reserved for one that does.
- **Secrets go one way; identifiers do not.** On PM-KISAN — the only scheme here that has a
  challenge — send `challenge`, never expect it back, never log it; `challenge.value` never
  reaches the lodge call. `challengeIssued` is the other
  half: returned, never sent, never secret. `enrolmentId` is an identifier rather than a
  secret and does come back, echoed in `orderId` on `on_support`; PMFBY's returns again in
  `commitmentAttributes` on `on_status`. It stays `no-log` and `no-trace` either way. On
  application status it is not sent at all and comes back as the portal's resolution of the
  lookup, while `applicant.id` — which may be an Aadhaar number — is never echoed. Every
  field carrying personal data is marked `x-oan-pii` in the pack, with a class and a
  handling list — documentation today, not something the validator enforces.
- **Nothing is closed yet.** Neither portal documents a terminal status, so no case read
  yields `Resolved`, `Rejected` or `Closed` today: an unrecognised phrase maps to
  `UnderReview`, never to a terminal code. This is a grievance rule; application status has
  no lifecycle to close.
- **The response is an allow-list, not a passthrough.** Both portals return far more about
  the farmer than any of these capabilities surface — name, parentage, gender, phone, full
  address. The adapter maps the fields the pack names and drops the rest. A field that is
  absent here is withheld on purpose, not overlooked.
- **The portal's own message is never returned.** It may carry a stack trace, an internal
  hostname or a quoted-back credential. Log it redacted and return our own message.

### D.1 Why PMFBY publishes no challenge

PM-KISAN challenges every call and PMFBY challenges none — yet PMFBY does operate an OTP
service. The difference is not that one scheme has one and the other does not.

Both schemes run their OTP on a realm other than the one that takes the grievance, so the
difference is not where the OTP lives. It is **what the OTP is keyed on**.

| | the OTP call takes | so proving it shows |
|---|---|---|
| PM-KISAN | the registration number — the portal looks up the mobile it holds against it | the caller controls the number on file for *this registration*, which is the subject of the grievance |
| PMFBY | a mobile number the caller supplies; the application number is never passed | the caller controls *some* phone. Nothing ties it to the policy |

PM-KISAN's challenge therefore binds the caller to the record. PMFBY's would bind them to
nothing, which is why this network does not borrow it and publishes `[]` instead. The one
PMFBY operates belongs to its policy flow, on a different realm with different credentials.

> **An unchallenged desk is an unauthenticated one.** Nothing proves a PMFBY caller is the
> farmer. Anyone holding an application number can lodge a grievance against it, and PMFBY
> application numbers have visible structure. That is the portal's own posture, not a gap
> this network introduces — but it is worth raising with PMFBY, and any control it wants
> belongs on FGMS beside the lodge rather than borrowed from another realm.

---

## Appendix E — Errors

Every error comes back on the same HTTP response, never on a later `on_*`.

| What went wrong | HTTP | `status` | `error.code` |
|---|---|---|---|
| A required field is missing | `400` | NACK | `SCH_REQUIRED_FIELD_MISSING` |
| Wrong or expired OTP — PM-KISAN; the verify fails and the call it guards is never made | `400` | NACK | `BIZ_GENERIC_ERROR` |
| Portal rejects the request, or is unreachable | `502` | NACK | `NET_DOWNSTREAM_UNAVAILABLE` |
| Envelope will not decrypt — PM-KISAN | `500` | NACK | `NET_INTERNAL_ERROR` |
| No grievance found — the portal answered, and has no case matching the ticket or `case.filedOn` | `202` | ACK | `BIZ_NO_RESULTS_FOUND` |
| No registration found — the portal answered, and holds nothing for that applicant | `202` | ACK | `BIZ_NO_RESULTS_FOUND` |
| `applicant.id` does not match its declared `idType` | `400` | NACK | `SCH_SCHEMA_VALIDATION_FAILED` |

### E.1 A failure

`NACK`, with `details.path` naming the field:

```json
{
  "message": {
    "status": "NACK",
    "messageId": "<the messageId from the request context>",
    "error": {
      "code": "SCH_REQUIRED_FIELD_MISSING",
      "message": "grievance.description is required",
      "details": { "path": "$.message.support.channels[0].grievance.description" }
    }
  }
}
```

### E.2 No grievance found

The portal answered and has no matching case. Nothing failed and
there is nothing for the caller to correct, so this is an `ACK`, not a `NACK`: request
accepted, no result, nothing further coming.

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

Three things to know, about every error on this page and not only this one:

- **`details.path` follows the field, not the action.** A missing phone is
  `$.message.support.channels[0].applicantPhone` on `support` and
  `$.message.contract.commitments[0].commitmentAttributes.applicantPhone` on `status`. The
  pack's `x-beckn-container-by-action` names each of those two roots. `x-beckn-path` is a
  different keyword and sits on one field only, `enrolmentId` — the one field that moves.
- **The portal's own message is never passed through.** It may hold a stack trace, an
  internal hostname, or a quoted-back credential. Logged redacted; we return our own.
- **A wrong OTP is not a `401`.** `401` means the Beckn signature failed to verify. The
  enum has no OTP-specific value, and PM-KISAN publishes no clean success signal for the
  verify step, so that row is provisional. It cannot arise on PMFBY, which has no challenge.

---

## Appendix F — What the adapter publishes

The adapter publishes one catalog per capability, once — three in all, two of them under
the PM-KISAN provider. A caller never sends a `catalog/publish`; it reads the ids out of
an `on_discover` reply. Discovery itself is [§2](#2-before-any-scenario--discover).

### F.1 PMFBY grievance

The grievance desk, under a provider of its own.

```json
POST /catalog/publish
{
  "context": {
    "action": "catalog/publish",
    "version": "2.0.0",
    "senderId": "grievance.adapter.openagrinet.org",
    "receiverId": "discovery.openagrinet",
    "transactionId": "7c2e1a94-3b6d-4f82-a1c5-9d0e4b7f2a61",
    "messageId": "d4f81b03-9e27-4a5c-8b16-3c7a0f5e9d82",
    "timestamp": "2026-09-20T06:30:00Z",
    "schemaContext": [
      "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld#openagrinet:PMFBYGrievance"
    ]
  },
  "message": {
    "catalogs": [{
      "id": "cat-pmfby-grievance",
      "isActive": true,
      "descriptor": {
        "code": "PMFBY-GRIEVANCE",
        "name": "PMFBY Grievance Desk",
        "shortDesc": "File and track crop-insurance grievances",
        "longDesc": "Lodge a grievance against a PMFBY enrolment and read the case back. No challenge: the portal's grievance service asks for none."
      },
      "provider": {
        "id": "pmfby",
        "descriptor": { "code": "PMFBY", "name": "PMFBY Grievance Portal" }
      },
      "resources": [{
        "id": "res:pmfby:grievance",
        "descriptor": { "code": "PMFBY-GRV", "name": "PMFBY grievance filing and status" },
        "resourceAttributes": {
          "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
          "@type": "openagrinet:PMFBYGrievance",
          "informationMode": "OnDemand",
          "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
          "challengeMethods": []
        }
      }],
      "offers": [{
        "id": "off:pmfby:grievance",
        "resourceIds": ["res:pmfby:grievance"]
      }]
    }],
    "publishDirectives": [
      { "catalogId": "cat-pmfby-grievance", "catalogType": "REGULAR" }
    ]
  }
}
```

- `@type` and `@context` are the ones every PMFBY payload in §3 carries: a capability
  declaration is the pack in `OnDemand` mode.
- `challengeMethods: []` is what tells a caller to open with `support`. The field is
  published empty rather than omitted: empty is an answer, absent is a question.
- `provider` carries no `availableAt`: a grievance desk has no premises.
- `visibleTo` is omitted, so every caller can find it.

The `discover` that finds it is shown in full in [§2](#2-before-any-scenario--discover).

### F.2 PM-KISAN grievance

The grievance desk. Its provider carries the application-status desk in F.3 as well.

```json
POST /catalog/publish
{
  "context": {
    "action": "catalog/publish",
    "version": "2.0.0",
    "senderId": "grievance.adapter.openagrinet.org",
    "receiverId": "discovery.openagrinet",
    "transactionId": "b15f0d83-2a47-4c91-85e6-3f7d2c9a0b64",
    "messageId": "4e8c17a6-9b30-42fd-a157-6d02b8e3c975",
    "timestamp": "2026-09-20T06:31:00Z",
    "schemaContext": [
      "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld#openagrinet:PMKISANGrievance"
    ]
  },
  "message": {
    "catalogs": [{
      "id": "cat-pmkisan-grievance",
      "isActive": true,
      "descriptor": {
        "code": "PMKISAN-GRIEVANCE",
        "name": "PM-KISAN Grievance Desk",
        "shortDesc": "File and track PM-KISAN grievances",
        "longDesc": "Lodge a grievance against a PM-KISAN registration and read the case back. An OTP is required to file and to read."
      },
      "provider": {
        "id": "pmkisan",
        "descriptor": { "code": "PMKISAN", "name": "PM-KISAN Grievance Portal" }
      },
      "resources": [{
        "id": "res:pmkisan:grievance",
        "descriptor": { "code": "PMKISAN-GRV", "name": "PM-KISAN grievance filing and status" },
        "resourceAttributes": {
          "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
          "@type": "openagrinet:PMKISANGrievance",
          "informationMode": "OnDemand",
          "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
          "challengeMethods": ["SMS_OTP"]
        }
      }],
      "offers": [{
        "id": "off:pmkisan:grievance",
        "resourceIds": ["res:pmkisan:grievance"]
      }]
    }],
    "publishDirectives": [
      { "catalogId": "cat-pmkisan-grievance", "catalogType": "REGULAR" }
    ]
  }
}
```

- Identical in shape to PMFBY's. What differs is the ids, the pack it points at, and
  `challengeMethods`.
- `challengeMethods: ["SMS_OTP"]` tells a caller to open with `init`. What it does not say
  is that PM-KISAN challenges the read as well as the lodge; [Appendix C.2](#c2-pm-kisan-grievance) does.

#### Finding it — `discover`

```json
POST /discover
{
  "context": {
    "version": "2.0.0", "action": "discover", "networkId": "openagrinet",
    "transactionId": "9d4a6f12-7e58-4b03-a2c6-1f8b3e5d7c40",
    "messageId": "2c7e9b40-5a13-46d8-91f2-8e0d4a6c3b57",
    "timestamp": "2026-09-20T09:14:00Z"
  },
  "message": {
    "intent": {
      "textSearch": "grievance",
      "filters": {
        "type": "jsonpath",
        "expression": "$.catalogs[*].resources[*] ? (@.resourceAttributes.scheme.code == \"PM-KISAN\")"
      }
    }
  }
}
```

Filtering on the scheme returns **both** PM-KISAN catalogs — this one and the
application-status desk in [F.3](#f3-pm-kisan-application-status). Pick this one out of the
reply by `resourceAttributes.@type`, then take `provider.id`, `offers[].id` and
`resources[].id` from it — [§4.1](#41-scenario--filing-a-complaint-with-the-registration-proved-first)
and [§4.2](#42-scenario--checking-it-four-days-later) quote them verbatim.

### F.3 PM-KISAN application status

A separate catalog from the grievance desk, under the same provider.

```json
POST /catalog/publish
{
  "context": {
    "action": "catalog/publish",
    "version": "2.0.0",
    "senderId": "grievance.adapter.openagrinet.org",
    "receiverId": "discovery.openagrinet",
    "transactionId": "c7a4e912-6b38-4d05-9f12-8e3b0a7c5d26",
    "messageId": "5f19b283-4c70-41ae-b936-2d847e1a0c93",
    "timestamp": "2026-09-20T06:32:00Z",
    "schemaContext": [
      "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld#openagrinet:PMKISANApplicationStatus"
    ]
  },
  "message": {
    "catalogs": [{
      "id": "cat-pmkisan-application-status",
      "isActive": true,
      "descriptor": {
        "code": "PMKISAN-APPLICATION-STATUS",
        "name": "PM-KISAN Application Status",
        "shortDesc": "Check a PM-KISAN registration",
        "longDesc": "Read how far a PM-KISAN registration has got: instalments paid, eKYC, and what is blocking payment. An OTP is required to read."
      },
      "provider": {
        "id": "pmkisan",
        "descriptor": { "code": "PMKISAN", "name": "PM-KISAN Grievance Portal" }
      },
      "resources": [{
        "id": "res:pmkisan:application-status",
        "descriptor": { "code": "PMKISAN-APP", "name": "PM-KISAN application status" },
        "resourceAttributes": {
          "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
          "@type": "openagrinet:PMKISANApplicationStatus",
          "informationMode": "OnDemand",
          "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
          "challengeMethods": ["SMS_OTP"]
        }
      }],
      "offers": [{
        "id": "off:pmkisan:application-status",
        "resourceIds": ["res:pmkisan:application-status"]
      }]
    }],
    "publishDirectives": [
      { "catalogId": "cat-pmkisan-application-status", "catalogType": "REGULAR" }
    ]
  }
}
```

- Same `provider.id` as the grievance desk — one participant, two capabilities. What tells
  them apart is `@type`, and the registry binds on it.
- `challengeMethods: ["SMS_OTP"]` tells a caller to open with `init`.

#### Finding it — `discover`

Filtering on the scheme returns **both** PM-KISAN catalogs — the grievance desk and this
one. Pick this one out of the reply by `resourceAttributes.@type`; the filter expression
reaches `resourceAttributes` but is not the place to match a key beginning with `@`.

```json
POST /discover
{
  "context": {
    "version": "2.0.0", "action": "discover", "networkId": "openagrinet",
    "transactionId": "d0f6c418-9b25-4e73-85a1-7c2e4b60f9d8",
    "messageId": "8a2c5f90-3d41-47b6-a0e8-5c9170b4d2e3",
    "timestamp": "2026-09-20T09:16:00Z"
  },
  "message": {
    "intent": {
      "textSearch": "application status",
      "filters": {
        "type": "jsonpath",
        "expression": "$.catalogs[*].resources[*] ? (@.resourceAttributes.scheme.code == \"PM-KISAN\")"
      }
    }
  }
}
```

Take `provider.id`, `offers[].id` and `resources[].id` from the reply —
[§5.1](#51-scenario--the-instalment-has-not-arrived) quotes them verbatim.

---

## Appendix G — Registry

Nothing above works until the providers are registered. Three kinds of record — what the
data is, who we call, and how. Two participants, three capabilities: PM-KISAN carries two,
so it gets one `Participant` record and two each of the other kinds.

### G.1 PMFBY grievance

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
  "baseUrl": "https://pmfbydemo.amnex.co.in"   // demo host; confirm production
} }

// One realm, one login. Every grievance call lives under /krphapi/FGMS and
// logs in at POST /krphapi/FGMS/NICUsersLogin, which answers with a token sent
// back as a bare Authorization header -- no "Bearer".
//
// The host also serves a PMFBY core realm under /api/v*, with its own login at
// POST /api/v2/external/service/login and its own OTP pair at
// /api/v1/services/nic/getOtp and /verifyMobile. That realm belongs to the
// policy flow and none of it is used here: the tokens are not interchangeable,
// and the grievance service asks for no OTP. Paths are written full from the
// host so the boundary stays visible.

{ "ProviderSchema": {
  "bindingKey":     "pmfby|openagrinet:PMFBYGrievance",
  "participantId":  "pmfby",
  "capabilityCode": "openagrinet:PMFBYGrievance",
  "status": "active",
  "actions": [
    // No init action. PMFBY's grievance service issues no challenge, so the
    // flow opens at support.
    { "action": "support", "method": "POST", "path": "/krphapi/FGMS/AddKRPHNCIPGrievenceSupportTicket",
      // The lodge takes no OTP -- FGMS trusts the service token, not the farmer --
      // and applicationNo is accepted unverified. Filing is unauthenticated.
      "mappings": "mappings/pmfby/grievance.support.yaml",
      "providerIdAt":     "message.support.channels[].provider.id",  // [] is the grammar's
                                                                      // only plural: no index
      "capabilityCodeAt": "message.support.channels[].@type",
      "timeoutMs": 30000, "status": "active" },
    { "action": "status",  "method": "POST", "path": "/krphapi/FGMS/GetGrievenceTicketsStatus",
      "mappings": "mappings/pmfby/grievance.status.yaml",
      "timeoutMs": 30000, "retryMax": 2, "status": "active" }
  ] } }
```

`receiptSourceID`, `ticketCategoryID`, `ticketSubCategoryID` and `requestYear` travel
upstream as numbers while the network carries all four as strings, so the `support` mapping
coerces them. The portal reads a non-numeric value as `0` instead of rejecting it, which
files the ticket under the wrong head and reports success — so the mapping validates before
it coerces rather than trusting the cast. `requestSeason` is the one numeric field that is
not a cast: `season` is a closed enum and the mapping looks `Kharif`, `Rabi` and `Zaid` up
to 1, 2 and 3, so an unknown value has nowhere to land.

### G.2 PM-KISAN grievance

Same shape as PMFBY, with an `init` PMFBY does not have — three actions rather than two.
The `Participant` record below is shared with §G.3 — register it once.

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
  // staging host; the production one is not recorded anywhere -- confirm it
  // with PM-KISAN rather than deriving it from the "Test" in this name
  "baseUrl": "https://pmkisanstaging.amnex.co.in/exlinkstaging/services/GrievanceServiceTest.asmx"
} }

// Two hosts, two tokens, two body encodings. The grievance calls and the OTP
// calls are separate PM-KISAN services that happen to answer for the same
// registration number.
//   grievance  /LodgeGrievance, /GrievanceStatusCheck
//              .../GrievanceServiceTest.asmx   AES-256-GCM
//   OTP        /ChatbotOTP, /ChatbotOTPVerified
//              .../chatbotservice.asmx         AES-128-CBC
// The action below that lives on the second host names it, because a
// Participant carries one baseUrl and these two are not the same host.

{ "ProviderSchema": {
  "bindingKey":     "pmkisan|openagrinet:PMKISANGrievance",
  "participantId":  "pmkisan",
  "capabilityCode": "openagrinet:PMKISANGrievance",
  "status": "active",
  "actions": [
    { "action": "init",    "method": "POST", "path": "/ChatbotOTP",
      // different host, token and cipher from the two actions below.
      // /ChatbotOTPVerified on the same host runs before support and status,
      // so it is a precondition of those, not an action of its own.
      "baseUrl": "https://exlink.pmkisan.gov.in/services/chatbotservice.asmx",
      "mappings": "mappings/pmkisan/grievance.init.yaml",
      "timeoutMs": 20000, "status": "active" },
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

### G.3 PM-KISAN application status

A second capability on the **same** `Participant`. No new participant record: the
`pmkisan` one in §G.2 serves both, and the binding key is what tells the two apart.

```jsonc
{ "SchemaRegistry": {
  "capabilityCode": "openagrinet:PMKISANApplicationStatus",
  "name": "PM-KISAN Application Status",
  "version": "v0.1",
  "schemaUrl": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/attributes.yaml",
  "status": "active"
} }

// Both actions live on the chatbot service, not the grievance service, so both
// name a baseUrl rather than inheriting the Participant's. That is the opposite
// split from §G.2, where only init sits there.
//
// Four upstream paths, all AES-128-CBC with the key in band:
//   /ChatbotOTP, /ChatbotOTPVerified     the OTP pair
//   /ChatbotUserDetails                  the record
//   /ChatbotBeneficiaryStatus            what is blocking payment
//
// CONFIRM THE HOST. The value below is the legacy client's hardcoded fallback,
// used only when neither PM_KISAN_BASE_URL nor PM_KISAN_BASE_OTP_URL is set;
// the API collection leaves both as unresolved variables and notes the two may
// differ. If they do, otpBaseUrl and baseUrl below are two different hosts.

{ "ProviderSchema": {
  "bindingKey":     "pmkisan|openagrinet:PMKISANApplicationStatus",
  "participantId":  "pmkisan",
  "capabilityCode": "openagrinet:PMKISANApplicationStatus",
  "status": "active",
  "actions": [
    { "action": "init",   "method": "POST", "path": "/ChatbotOTP",
      "baseUrl": "https://exlink.pmkisan.gov.in/services/chatbotservice.asmx",
      "mappings": "mappings/pmkisan/application-status.init.yaml",
      "timeoutMs": 20000, "status": "active" },
    { "action": "status", "method": "POST", "path": "/ChatbotUserDetails",
      // One Beckn status, three upstream calls, in order:
      //   /ChatbotOTPVerified       spend the OTP -- a precondition, not an
      //                             action of its own, as in the grievance
      //                             capability
      //   /ChatbotUserDetails       enrolmentId, registeredOn,
      //                             latestInstallmentPaid, ekyc
      //   /ChatbotBeneficiaryStatus blockers
      // The second is the path above; the third is the chained call below.
      // Only the second is mandatory: if the third fails, return the record
      // with blockers ABSENT rather than empty -- absent means not asked, and
      // an empty array would read as "payment is clear".
      "baseUrl": "https://exlink.pmkisan.gov.in/services/chatbotservice.asmx",
      "chainedPath": "/ChatbotBeneficiaryStatus",
      "mappings": "mappings/pmkisan/application-status.status.yaml",
      "timeoutMs": 30000, "retryMax": 2, "status": "active" }
  ] } }
```

There is no `support` action, no `providerIdAt` and no `capabilityCodeAt`: both legs
compose a `Contract`, so the adapter reads the participant from
`commitments[].offer.provider.id` and the capability from `commitmentAttributes.@type` on
the Beckn v2 defaults.

The legacy client fails **open** on the record call — it treats any response whose
`Rsponce` is not the literal string `"False"` as a success, so a malformed or empty reply
is read as a valid record. v2 fails closed: `Rsponce` must equal `"True"`.
