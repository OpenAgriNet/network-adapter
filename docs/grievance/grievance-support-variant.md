# Grievance — the `/support` variant

Date: 2026-09-30 · Status: **adopted** — `grievance-usecase.md` now describes this flow

Why the grievance is lodged with Beckn's `/support` action rather than `confirm`. `init`
and `status` keep their actions. This page is the rationale and the cost of the change; the
live sequence and payloads are in `grievance-usecase.md`, which follows it.

> What the two portals actually accept and return — one line of provenance per field —
> is `grievance-upstream-contracts.md`. Where this page and that one disagree, that one
> wins.

```
PMFBY      init  →  support  →  status
PM-KISAN           support  →  status
```

## One rule for where every field sits

**`descriptor` is the complaint. Attributes are the caller and the case record.**

`Support` carries a `descriptor`, and so does `Contract` — so the same rule holds on all
three legs, and a reader who learns it once can read any example on this page.

| | holds | on `support` | on `init` / `status` |
|---|---|---|---|
| `orderId` | the enrolment the complaint is against | `applicationNo` on PMFBY, the registration number on PM-KISAN — echoed unchanged on the reply | — (`Contract` has no `orderId`) |
| `descriptor` | the complaint: category `code`/`name`, the farmer's words in `longDesc` | `support.descriptor` | `contract.descriptor` |
| attributes | who is asking, and what the case record says | `channels`, selected by `@type` | `commitmentAttributes` |

Two consequences worth stating plainly. `grievanceCategory` and `grievanceDescription` leave
the pack, because `Descriptor` already has the shape for both. And `descriptor` is absent
where there is nothing to describe: `init` asks for a challenge before the farmer has stated
anything, and a `status` request asks about a ticket rather than restating the case.

## What is the same

The pack values. Every enum, every `informationMode` rule, every PII marking is unchanged —
three fields move, and their markings move with them.

The transaction count. PMFBY is three calls either way, PM-KISAN two.

## What changes

`confirm` becomes `support`. On that leg the pack payload rides in `Support.channels` — the
same base `Attributes` schema `commitmentAttributes` extends, so it validates unchanged.

`applicationNo`, `grievanceCategory` and `grievanceDescription` move out of the pack into
`orderId` and `descriptor`, on every leg.

**Routing on the lodge leg.** The binding key is `<participantId>|<capabilityCode>`, read
off the payload. `init` and `status` keep the contract defaults. On `support` both halves
re-path into the channel:

```yaml
# support step only
providerIdAt:     "message.support.channels[].providerId"
capabilityCodeAt: "message.support.channels[].@type"
```

`Support` itself is sealed at three fields and none names a participant, but `channels` is
an array of `Attributes` — the spec's extensibility container, `additionalProperties: true`
— so the pack puts `providerId` there. `scheme.code` is not a substitute: it names a scheme
rather than a participant, and reads `PMFBY` where the registry holds `pmfby`.

An earlier draft named the provider statically in the step config instead, which would have
needed a third case in `BindingPaths`. Pathing it costs nothing extra and keeps two things
the static form loses: no change in shared adapter code, and an array on the provider path
for `countAt` to count, which is what refuses a second channel. Note `channels[]`, not
`channels[0]` — the path grammar has segments and `[]`, no indices; an index would be read
as a literal field name and silently match nothing.

### What adopting this costs

Eight changes, in the order they have to happen. Four are **done**, and the one that
was blocking turned out not to be a change at all.

1. ~~**Decide `confirm` vs `support`.**~~ Decided: `support`. The three spec deviations
   below still go to the network's spec authority, but as questions about an adopted flow
   rather than as a gate on the decision.
2. ~~**`x-beckn-container-by-action`.**~~ `confirm` is dropped from both packs. The map is
   now `init`/`status`/`support` on PMFBY and `status`/`support` on PM-KISAN.
3. ~~**Re-path the fields that leave the pack.**~~ `applicationNo` on PMFBY and
   `registrationNo` on PM-KISAN → `orderId`; `grievanceCategory` → `descriptor.code`/`.name`;
   `grievanceDescription` → `descriptor.longDesc`, each recorded in that field's
   `x-beckn-path`. Their `x-oan-pii` and `no-log`/`no-trace` markings stayed with them, and
   `registrationNo` lost the `writeOnly`/`no-echo` it carried while it had nowhere to land.
4. ~~**Adapter**~~ — none needed. Pathing `providerId` through the channel uses the
   both-halves-pathed case `BindingPaths` already accepts, so this flow requires no code
   change and nothing blocks the registry row below.
5. **Registry** — add the `support` step with the routing shown above. `init` and `status`
   are untouched.
6. **JSONata** — one mapping pair per provider (`support` out, `on_support` back). What
   goes to the portal is unchanged by this decision — same body, same fields — so only the
   Beckn half of each mapping reflects it.
7. **Error paths** — `details.path` changes on the lodge leg only. Derive it from
   `x-beckn-path` rather than hard-coding, so this falls out of step 3.
8. ~~**Examples and docs**~~ — `grievance-usecase.md` is on the adopted flow, both pack
   READMEs are updated, and the sequence diagram has been regenerated in all three formats.
   What remains is folding this file into the usecase doc once the spec questions below are
   answered.

Unaffected either way: the upstream contracts, the PMFBY OTP step, the allow-list on the
read, and every security rule in the packs. This is a change of Beckn envelope, not of what
we send to a portal or what we let back out.

---

# PMFBY

## 1 — `init`: ask for the challenge

No `descriptor`: nothing has been stated yet. Only the phone the challenge should go to.

```json
POST /init
{
  "context": {
    "version": "2.0.0", "action": "init", "networkId": "openagrinet",
    "bppId": "pmfby.openagrinet.io",
    "transactionId": "3f9a1c7e-2b40-4d91-8a55-6c0e2d84b731",
    "messageId": "8c21f0a4-9e17-4b3d-bb62-51a7c9e0d248",
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

`on_init` — commitment stays `DRAFT`. The OTP is never returned; the mechanism is named and the phone comes back masked.

```json
{
  "context": { "action": "on_init", "messageId": "8c21f0a4-…", "…": "…" },
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
        "@context": "…/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "challengeIssued": {
          "method": "SMS_OTP", "sentTo": "98XXXXXX10", "expiresAt": "2026-09-28T10:25:02Z"
        }
      }
    }]
  }}
}
```

## 2 — `support`: lodge the grievance

Same `transactionId`, new `messageId`. The contract does not travel — a `SupportAction` has
no `contract` property.

`descriptor` is the whole complaint. `code` is load-bearing: `3.10` is the portal's category
and sub-category joined by a dot, and the adapter splits it there into two upstream fields.
`channels[0]` holds only what identifies and authenticates the farmer.

```json
POST /support
{
  "context": {
    "version": "2.0.0", "action": "support", "networkId": "openagrinet",
    "bppId": "pmfby.openagrinet.io",
    "transactionId": "3f9a1c7e-2b40-4d91-8a55-6c0e2d84b731",
    "messageId": "9d44b8e2-31c6-4f07-a9d3-7be5104c8f26",
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
        "@context": "…/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "OnDemand",
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

`on_support` — `orderId` comes back unchanged. It is described as "the order against which
support is required", an ask-side meaning, and a reply does not change what the complaint is
against, so the application number is echoed rather than overwritten. The spec says the
provider returns "the ticket reference" but never names the field that holds it; the ticket
goes in `ticketNo` on the channel, because the channel is where
the scheme's own fields live and a ticket is not what the complaint is against. `channels`
holds one object, the case record; address it by `@type`, not by index.
`challenge` and `applicantPhone` are dropped by the response allow-list.

The portal's lodge reply carries four things: a success flag, `ticket-no`, `ticket-id` and
a message. Only `ticket-no` becomes a value you can read, as `ticketNo` — the number the
farmer quotes back. `ticket-id` is the portal's own row id and is dropped. The success flag
decides whether this is an ACK at all; it is not a case status.
The message is the portal's own and is never returned — it may hold a stack trace or an
internal hostname. `caseStatus`, `filedOn` and `source` are **not in the reply**; the adapter
asserts them, and the pack README says so. The `descriptor` is the caller's own words echoed
back, not portal data.

```json
{
  "context": { "action": "on_support", "messageId": "9d44b8e2-…", "…": "…" },
  "message": {
    "support": {
      "orderId": "KA2026KH00123456",
      "descriptor": {
        "code": "3.10",
        "name": "Enrollment / Portal Issues Login",
        "longDesc": "Claim approved in July but no amount credited."
      },
      "channels": [
        {
          "@context": "…/PMFBYGrievance/v0.1/context.jsonld",
          "@type": "openagrinet:PMFBYGrievance",
          "informationMode": "Direct",
          "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
          "ticketNo": "100626000099001",
          "caseStatus": { "code": "Registered" },
          "filedOn": "2026-09-28",
          "source": { "sourceId": "pmfby", "sourceName": "PMFBY Grievance Portal" }
        }
      ]
    }
  }
}
```

No helpline channel is emitted. Neither portal publishes one in its reply, and we have no
sourced number for either scheme, so there is nothing to put there. If one is confirmed
later it becomes a second member of `channels` — which is why a consumer must select by
`@type` rather than by position.

**Keep `contract.id` and `ticketNo`.** `ticket-id` is the portal's own row id and is not
mapped; no later call is known to need it.

## 3 — `status`: read the case

New `transactionId` — a separate session, days later. Same `contract.id` as `init`. No
`descriptor` on the ask: we are naming a ticket, not restating the complaint. No challenge either;
it is spent once, at filing.

```json
POST /status
{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "bppId": "pmfby.openagrinet.io",
    "transactionId": "b7e0d259-4a83-41f6-9c28-3de51fa0672b",
    "messageId": "2f8c6a13-05b9-4e7d-8a41-c6902be4713f",
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
        "@context": "…/PMFBYGrievance/v0.1/context.jsonld",
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

`on_status` — `contract.descriptor` carries the complaint as the portal holds it, and the
attributes carry the case record. `caseRemark` is what has been written about the case, not
the complaint, so it stays in attributes; it is absent while nothing has been recorded.

```json
{
  "context": { "action": "on_status", "messageId": "2f8c6a13-…", "…": "…" },
  "message": { "contract": {
    "id": "b1d4e2f0-5a63-4c81-9e77-2af0c9d31b45",
    "descriptor": {
      "code": "3.10",
      "name": "Enrollment / Portal Issues Login",
      "longDesc": "Claim approved in July but no amount has been credited to my account."
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
        "@context": "…/PMFBYGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMFBYGrievance",
        "informationMode": "Direct",
        "scheme": { "code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana" },
        "ticketNo": "100626000099001",
        "applicationNo": "KA2026KH00123456",
        "cropYear": "2026",
        "season": "Kharif",
        "caseStatus": { "code": "UnderReview", "name": "Open" },
        "filedOn": "2026-09-28",
        "caseRemark": "Claim file reopened, awaiting surveyor report.",
        "source": { "sourceId": "pmfby", "sourceName": "PMFBY Grievance Portal" }
      }
    }]
  }}
}
```

The portal's `FarmerName`, `RequestorMobileNo`, `Email`, address fields, `InsurancePolicyNo`
and `InsuranceCompany` are dropped by the response allow-list, exactly as today.

---

# PM-KISAN

Same rule, with two differences forced by the scheme rather than chosen.

No OTP leg — the portal asks for none, so there is no `init`.

**`orderId` is the registration number.** There is no application number and no case
number — the upstream takes one reference, `IdentityNo`, and nothing else in the API names a
case. The identity *is* the thing the complaint is against, which is what `orderId` means, so
it goes there: the same slot PMFBY fills with `applicationNo`. Like PMFBY's, it is echoed
unchanged on the reply — a reply does not change what the complaint is against. That echo is
deliberate and scoped —
handing the caller their own registration number over the same signed exchange it arrived on
discloses nothing — which is why `registrationNo` is neither `writeOnly` nor `no-echo`. `no-log`
and `no-trace` still apply without exception.

The AES-GCM envelope is applied by the adapter after the request mapping and removed before
the response mapping, so nothing on this page shows it.

## 1 — `support`: lodge the grievance

`descriptor.code` is one of the pack's ten codes; `G003` is *Installment not received*.
`orderId` is the registration number; `channels[0]` carries nothing but the scheme.

```json
POST /support
{
  "context": {
    "version": "2.0.0", "action": "support", "networkId": "openagrinet",
    "bppId": "pmkisan.openagrinet.io",
    "transactionId": "7f3a9c12-64b8-4e05-b1d7-38e6a0c4f992",
    "messageId": "4a1e8d60-92b7-4c35-8f0a-15d73e6b2094",
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
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" }
      }]
    }
  }
}
```

`on_support` — `orderId` comes back as it went up, as on PMFBY. What is missing here is the
ticket: PMFBY returns one on the channel and PM-KISAN issues none. The
portal's lodge reply is `{ Responce, message }` and nothing more: `Responce` becomes the
ACK, `message` is logged redacted and never returned. So **nothing below comes from the
portal** — `descriptor` is the caller's own words echoed back, `caseStatus` and `filedOn`
are the adapter's assertions, `source` is configuration.

```json
{
  "context": { "action": "on_support", "messageId": "4a1e8d60-…", "…": "…" },
  "message": {
    "support": {
      "orderId": "UP12345678A",
      "descriptor": {
        "code": "G003",
        "name": "Installment not received",
        "longDesc": "Third instalment for 2026 has not been credited."
      },
      "channels": [{
        "@context": "…/PMKISANGrievance/v0.1/context.jsonld",
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

No contact channel is emitted: PM-KISAN publishes no helpline we have on file. If one is
ever added it becomes a second object in `channels`, which is why a consumer must select
the case record by `@type` rather than by position.

**Keep the registration number and `filedOn`** — together they are what the read needs. On
`status` the registration number rides in `commitmentAttributes.registrationNo`, not `orderId`:
a `Contract` has no `orderId`. `filedOn` collides for two grievances filed on the same
identity the same day; that is unchanged from the current design.

## 2 — `status`: read the replies

No `descriptor` on the ask. `contract.id` is a UUID the caller mints here — see Open.

```json
POST /status
{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "bppId": "pmkisan.openagrinet.io",
    "transactionId": "c04b7e81-6d92-4a10-b8e3-59f2ac103d47",
    "messageId": "e91f52a8-7b04-4d6c-9138-20ae4c85f7b3",
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
        "@context": "…/PMKISANGrievance/v0.1/context.jsonld",
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

`on_status` — `registrationNo` is not echoed. The portal's farmer name, father's name, gender,
mobile number and address are dropped by the response allow-list, and `Reg_No` with them:
five of the record's fourteen fields survive. The record carries no category, so
`descriptor` comes back with `longDesc` only — the description the portal stored — and no
`code` or `name`.

```json
{
  "context": { "action": "on_status", "messageId": "e91f52a8-…", "…": "…" },
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
        "@context": "…/PMKISANGrievance/v0.1/context.jsonld",
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

---

## Nothing on file

Unchanged. A read matching no case is `202` with Beckn's `AckNoCallback` body and
`BIZ_NO_RESULTS_FOUND`, `status: "ACK"`. `/support` declares `AckNoCallback` among its
response codes, so this holds on the lodge leg too.

## Errors

Same codes, same shape. `/support` declares `Ack, AckNoCallback, NackBadRequest,
NackUnauthorized, ServerError` — the same set as `/confirm` and `/status`.

What changes is `details.path`, because three fields no longer sit in the attributes
object. On the contract flow a missing phone reports:

```
$.message.contract.commitments[0].commitmentAttributes.applicantPhone
```

On `support` the same failure reports `$.message.support.channels[0].applicantPhone`, and
a missing category reports `$.message.support.descriptor.code` rather than
`…commitmentAttributes.grievanceCategory.code`. A caller reading `details.path` to
highlight the offending input has to follow the same rule the mapping does; the pack's
`x-beckn-path` is the source for both.

---

## Open — carried forward

**The PMFBY case-read mapping is unverified, and this variant inherits it.** Every value in
the PMFBY `on_status` examples below comes from a set of upstream field names that occur in
no source we can check — see `grievance-upstream-contracts.md`. That is not an argument for
or against `support`; it is the same gap on either flow, and it is larger than anything on
this page. It has to be closed with PMFBY before either flow ships.

**`/status` asks about a contract that was never confirmed.** Its precondition is explicit
and has no alternative branch: *"A `/confirm` request MUST have completed and the CN MUST
have received an `/on_confirm` callback with a confirmed Contract carrying a valid id before
this endpoint may be called."* Replacing `confirm` with `support` means that is never true,
and on PM-KISAN there is no `init` either, so the `contract.id` presented at `status` is a
UUID the provider has never seen in any prior call. The current design already deviates from
`/confirm`'s own preconditions; this is a second deviation of the same kind, and both should
go to the network's spec authority together.

**Three fields leave the pack and their markings go with them.** `applicationNo` on PMFBY
and `registrationNo` on PM-KISAN (both `x-oan-pii: identifier`, `no-log, no-trace`) move to
`orderId`; `grievanceCategory` and
`grievanceDescription` (`x-oan-pii: freetext`, `no-log, no-trace`) move to `descriptor`. The
pack should keep describing them at their new paths rather than dropping them silently — see
the note below on `x-beckn-container`.

**`x-beckn-container` has to say more than it does.** It is a single value naming one
container; this variant needs it to say, per field, which Beckn slot the field occupies and
on which action. Nothing in the repo reads it today — `network-specs/scripts/` holds only
`generate-schema-pages.rb`, and there is no CI workflow — so this is a documentation change,
not a migration.

**`channels` is carrying something that is not a channel.** The spec defines it as
"available support channels ... such as phone, email, or chat endpoints", and we are putting
a case record in it. It validates — `Attributes` requires only `@context` and `@type` — but
it is a repurposing, and the reason is that `Support` has no `supportAttributes` the way
`Commitment` has `commitmentAttributes`. There is no other slot. A consumer must select the
case record by `@type`, never by index, so that a real channel appearing later does not
shift it. This is the strongest argument against the variant and should go to the network's
spec authority with the other two.

**A lodge reply carries almost no portal data, on either scheme.** PMFBY's returns a status
string, `ticket-no`, `ticket-id` and a message; only the ticket and status are usable.
PM-KISAN's returns `{Responce, message}` and nothing else — no identifier, no date, no
status, no category. Everything else in either `on_support` is an echo of the request or an
adapter assertion. That is defensible, but it is true of the contract flow too. Both packs
now carry an `Upstream response coverage` table saying, field by field, what the portal
sent, what we keep and what we drop; read that before trusting any value in an example.

**`ticket-id` is dropped.** PMFBY's lodge reply carries `ticket-no` *and* `ticket-id`; the
pack maps only the first. The second is the portal's own row id, and publishing an internal
key no caller reads is a cost with no benefit — the same judgement the case read already
makes about `TicketStatusID`. Confirm with PMFBY whether the case read keys on it; if it
does, the field comes back, which is cheaper than retiring a published one.

**No helpline is published by either scheme.** PMFBY's 14447 comes from the legacy voice
prompts, not from a PMFBY page; PM-KISAN has none on file. No contact channel ships until
one is sourced from the scheme itself.
