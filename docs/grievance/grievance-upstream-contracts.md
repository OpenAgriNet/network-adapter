# Grievance — upstream contracts

Date: 2026-09-30 · Status: reference

What PMFBY and PM-KISAN actually accept and actually return. This is the master
reference: the schema packs and every example in the other grievance docs are
checked against this page, and where they disagree, this page wins.

Nothing here is designed. Each block is either read out of the legacy
BharatVistaar source, with the file and line, or marked as not verified.

## How to read the provenance marks

| mark | means |
|---|---|
| **verified** | read from BharatVistaar source; the citation is the file and line |
| **not verified** | named in our own design docs and in no source available to us |

There is a third thing worth saying plainly: for PMFBY we have **no direct
portal client anywhere in the legacy tree**. Every PMFBY call goes through the
v1 Beckn gateway, so what is verified below is the gateway's tag vocabulary,
not the portal's own field names. The gateway sits between us and PMFBY and we
cannot see through it from here.

---

# PMFBY

One client: `Orchestrator/agents/tools/pmfby_grievance.py`. It posts Beckn v1.1.0
bodies to `BAP_ENDPOINT`, not to PMFBY. Three calls.

Every request puts its fields in
`message.order.fulfillments[0].customer.person.tags[]`, each as
`{descriptor: {code}, value}`. The `request_type` tag selects the operation.

## 1. Request an OTP — `/init`, `request_type: get_otp`

**verified** — `_payload_get_otp_grievance_flow`, lines 159-191.

| sent | value |
|---|---|
| `request_type` | `get_otp` |
| `phone_number` | ten digits, no country code |

Also `provider.id: pmfby-agri`, `items[0].id: pmfby`, and
`customer.contact.phone`.

**Response: not verified.** The tool returns the body as prose and pins no
field. Our packs model a `challengeIssued` with `method`, `sentTo` and `expiresAt`;
none of those names appears in any source.

## 2. Verify the OTP — `/status`

**verified** — `_payload_status_verify_otp`, lines 245-266.

The OTP travels as `message.order_id`. Not as a tag, not in a body field —
`order_id`. `provider.id` is `pmfby-agri` here, not `pmfby-grievance`.

The OTP is validated client-side first: exactly six digits, `_validate_otp`
line 68. Failure is detected by substring-matching the reply against
`_OTP_FAILURE_SUBSTRINGS`, line 36 — `"invalid otp"`, `"otp expired"`, and six
more. There is no status code or error field to key on.

## 3. Lodge the grievance — `/init`, `request_type: submit_grievance`

**verified** — `PMfbyGrievanceInitRequest.get_payload`, lines 272-317.

| sent | note |
|---|---|
| `request_type` | `submit_grievance` |
| `phone_number` | normalised to ten digits |
| `complaint_date` | `YYYY-MM-DD`, generated client-side — the portal is not asked |
| `receipt_source_id` | constant `134306`, line 34 |
| `ticket_category_id` | constant `3`, line 32 |
| `ticket_sub_category_id` | constant `10`, line 33 |
| `request_year` | crop year |
| `request_season` | Kharif 1, Rabi 2, Zaid 3 — `_normalize_request_season_for_pmfby_api`, line 78 |
| `application_no` | insurance application number |
| `grievance_description` | free text |

Note the two category constants. The legacy tool files **every** grievance as
3.10; it does not ask the farmer to choose. Our packs let the caller pick a
category, which is new behaviour, not a port of existing behaviour.

**Response — verified**, `format_grievance_result`, lines 357-390. One tag,
`grievance-response`, whose `list` carries at most four entries:

| returned | note |
|---|---|
| `status` | |
| `ticket-no` or `ticket_no` | both spellings accepted |
| `ticket-id` or `ticket_id` | both spellings accepted |
| `message` | the gateway's own prose |

That is the whole reply. No date, no application number, no category, no
farmer details. The gateway returns the ticket and nothing else.

## 4. Read the case — `/status`, `request_type: status_grievance`

**verified** — `_payload_grievance_status`, lines 195-241.

| sent | note |
|---|---|
| `request_type` | `status_grievance` |
| `requestorMobileNo` | the filing phone |
| `GrievenceSupportTicketNo` | the ticket. The misspelling is the API's and is preserved deliberately — see the comment at line 231 |

`message.order_id` carries the ticket as well, and `provider.id` is
`pmfby-grievance`.

**Response: not verified, and this is the one real gap.**
`format_status_result`, lines 396-417, walks every tag and every list item and
prints `descriptor.name or descriptor.code` against `value`. It never names a
field, so the reply's shape is invisible from here.

Our design docs name fourteen: `GrievenceSupportTicketNo`, `ApplicationNo`,
`GrievenceDescription`, `TicketCategoryID`, `TicketSubCategoryID`,
`TicketCategoryName`, `TicketSubCategoryName`, `RequestYear`, `RequestSeason`,
`TicketStatus`, `TicketStatusID`, `ComplaintDate`, `latestRemark`,
`responseDynamic`, plus eleven personal fields. **Only
`GrievenceSupportTicketNo` occurs anywhere in BharatVistaar**, and it occurs as
a request tag, not a response field. The other thirteen occur in no file in the
legacy tree, in no beckn specification, and in nothing else on disk.

Either they came from a PMFBY API document we were given and did not keep, or a
previous draft invented them. Until that is settled, treat the entire PMFBY
case-read mapping as a proposal. It is the largest open item against the pack.

---

# PM-KISAN

Two clients, and they do not agree, so the choice between them is a real
decision rather than a detail.

- **Direct portal client** — `Voice/agents/tools/grievance.py`. Talks to
  `GRIEVANCE_BASE_URL` over an AES-GCM envelope. No OTP.
- **v1 Beckn gateway client** — `Orchestrator/agents/tools/pmkisan_grievance.py`.
  Talks to `BAP_ENDPOINT` in Beckn tags. Returns one field the direct client
  never sees.

The blocks below are the direct client unless they say otherwise, because that
is the portal's own contract.

## The envelope

**verified** — lines 46-101.

Every request body is encrypted whole and sent as `{"EncryptedRequest": "<b64>"}`.
Every response comes back as `{"d": {"__type": "...", "output": "<b64>"}}` and
`output` decrypts to the JSON below. AES-GCM, key and IV from
`GRIEVANCE_KEY_1` / `GRIEVANCE_KEY_2` as hex.

`TokenNo` is a static service token in every request body, from
`GRIEVANCE_TOKEN`. Its legacy default is `PMK_123456`, line 31 — a fail-open
default that must not be carried into v2. Fail closed.

## 1. Exchange Aadhaar for a token — `/GrievanceAadhaarToken`

**verified** — `_aadhaar_token`, lines 198-209.

Only when the identity is twelve digits, `_is_aadhaar`, line 194. A registration
number goes straight through untouched.

Sends `{Type: "IdentityNo_Details", TokenNo, IdentityNo}`.
Returns `{Responce, AadhaarToken, message}` — `AadhaarTokenResponse`, line 125.

The returned token then stands in for the identity everywhere below.

## 2. Lodge the grievance — `/LodgeGrievance`

**verified** — `CreateGrievanceRequest`, lines 236-259; call at line 324.

| sent | note |
|---|---|
| `Type` | `Reg_No_Details`, or `IdentityNo_Details` for an Aadhaar token |
| `TokenNo` | the static service token |
| `IdentityNo` | registration number, or the Aadhaar token |
| `GrievanceType` | `G001`–`G010` from `assets/grievance_types.json` |
| `GrievanceDescription` | free text, at least ten characters — line 308 |

**Response — verified**, `GenericMessageResponse`, line 177:

| returned | note |
|---|---|
| `Responce` | `"True"` / `"False"`, spelled as shown |
| `message` | the portal's own prose |

**Nothing else.** No identifier, no date, no status, no category. The reply
confirms receipt and that is all — which is why PM-KISAN has no case identifier
and why the read has to match on a date.

## 3. Read the cases — `/GrievanceStatusCheck`

**verified** — `StatusRequest`, lines 261-268; call at line 370.

Sends `{Type: "Reg_No_Status" | "IdentityNo_Status", TokenNo, IdentityNo}`.

There is **no per-grievance endpoint**. This returns every grievance on the
identity, and the caller picks.

**Response — two views, and the difference matters.**

The direct client's model, `GrievanceStatusDetail` line 134, keeps five fields
per record inside `{Responce, message, details[]}`:

| returned | kept by the direct client |
|---|---|
| `Reg_No` | yes |
| `GrievanceDate` | yes |
| `GrievanceDescription` | yes |
| `OfficerReply` | yes |
| `OfficeReplyDate` | yes |

The v1 gateway's formatter, `pmkisan_grievance.py` lines 222-238, labels
**fourteen**, so the portal sends at least these:

| returned | |
|---|---|
| `Farmer_Name` | personal |
| `Father_Name` | personal |
| `Gender` | personal |
| `Reg_No` | the registration number |
| `StateName` | personal |
| `DistrictName` | personal |
| `BlockName` | personal |
| `RevenueVillageName` | personal |
| `MobileNo` | personal |
| `GrievanceDate` | |
| `GrievanceStatus` | **the direct client's model drops this** |
| `GrievanceDescription` | |
| `OfficerReply` | |
| `OfficeReplyDate` | |

`GrievanceStatus` is the one that matters: the portal returns it, and the
direct client silently discards it. A v2 adapter modelled on the direct client
alone would lose the case status and have to infer it from whether a reply
exists. Read the record, not the client's model of it.

Nine of the fourteen are personal data and none of them may reach a payload, a
log or a trace.

## 4. What the v1 gateway adds

**verified** — `pmkisan_grievance.py` lines 263-282. Its own response tags:
`status`, `grievance-id`, `lookup-type`, `identity-no`, `grievance-type`,
`grievance-status`, `grievance-date`, `message`, `details`.

Two of these are worth naming.

**`grievance-id` — unresolved, and it changes the design if it is real.** The
direct client's `/LodgeGrievance` returns `{Responce, message}` with no
identifier, so either the gateway mints this itself or it reads something the
direct client never asked for. If PM-KISAN does issue a case id, then "PM-KISAN
has no case identifier" is wrong and the whole date-matching read is
unnecessary. This must be settled with PM-KISAN before the pack leaves v0.1.

**`identity-no` is echoed back.** The gateway returns the registration number
or Aadhaar token it was given. That must not be carried into v2: `registrationNo`
is `writeOnly` and is not echoed.

## Grievance categories

**verified** — `Orchestrator/assets/grievance_types.json` and
`Voice/assets/grievance_types.json` are byte-identical. Ten codes:

`G001` account number not correct · `G002` Aadhaar not seeded · `G003`
instalment not received · `G004` name mismatch · `G005` bank account
change · `G006` land record not updated · `G007` eKYC not done · `G008`
status not updated · `G009` registration not approved · `G010` problem in
facial-based eKYC

PM-KISAN uses these; PMFBY does not. PMFBY's category is a dotted pair, and the
legacy tool hard-codes it to 3.10.

---

# What this page settles

- **PMFBY's lodge reply is four fields** and three of them are usable. Verified.
- **PM-KISAN's lodge reply is two fields** and neither is data. Verified.
- **PM-KISAN's case record is fourteen fields**, not the five the direct client
  models. Verified, from the gateway's labels.
- **PMFBY's case record is unknown.** Not verified, and no source on disk names
  it.

# What it leaves open

1. **PMFBY case-read field names.** Thirteen of the fourteen in our docs are
   uncorroborated. Get the PMFBY API document, or drop the mapping to a
   proposal.
2. **`grievance-id`.** If PM-KISAN issues one, the PM-KISAN retrieval design
   changes completely.
3. **The PMFBY challenge reply.** `challengeIssued.sentTo` and `.expiresAt`
   are our invention; the real reply is unpinned. `method` is a mapping
   constant, so it is ours too, but it restates a fact the v1 flow confirms.
4. **Which PM-KISAN client v2 follows** — the direct portal contract, which has
   no OTP, or the gateway's, which the v1 flow used.
5. **Whether PMFBY still wants a fixed 3.10 category** or the caller's choice,
   which is what our packs assume.
