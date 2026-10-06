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

Where to look, because it is not one tree. `BharatVistaar/` is a working
directory holding several independent git repositories, and `Beckn/` is one of
them. Its checked-out `BharatVistaar` branch is 138 commits behind `main`, and
**the grievance implementation for both schemes exists only on `main`**. An
earlier revision of this page concluded that no direct portal client existed
anywhere; that conclusion came from searching the checked-out working tree
alone and was wrong. Both clients are there:

- `main:src/services/pmfby/pmfby-greviance.service.ts` — PMFBY, calls FGMS directly
- `main:src/services/pmkisan-grievance/pmkisan-grievance.service.ts` — PM-KISAN
- `main:src/app.service.ts` — the Beckn mappings over both

Citations below name the branch when the file is not in the working tree.

---

# PMFBY

Two layers, and they speak different vocabularies.

`Orchestrator/agents/tools/pmfby_grievance.py` posts Beckn v1.1.0 bodies to
`BAP_ENDPOINT` in lowercase_snake tags. The adapter translates those into the
portal's own camelCase body and calls FGMS. **The v2 adapter replaces the
gateway and calls FGMS directly**, so the portal contract in §0 below is the
one that governs; the gateway's tag vocabulary is recorded only because the
request field meanings were established there.

## 0. The portal itself — FGMS

**verified** — `main:src/services/pmfby/pmfby-greviance.service.ts`.

One host, `PMFBY_BASE_URL`, carrying **two unrelated auth realms**:

| realm | path prefix | login | used for |
|---|---|---|---|
| PMFBY core | `/api/v*` | `POST /api/v2/external/service/login` | OTP, policy, claims |
| FGMS | `/krphapi/FGMS` | `POST /krphapi/FGMS/NICUsersLogin` | **all three grievance calls** |

FGMS login sends `{appAccessUID, appAccessPWD}` and reads the token from
`responseDynamic.token.Token`. It is sent back as a bare `Authorization:`
header — **no `Bearer` prefix** (line 91).

**There is no grievance-specific OTP endpoint.** The OTP is the PMFBY core
one, `POST /api/v1/services/nic/getOtp` and `/api/v1/services/nic/verifyMobile`
(`main:src/services/pmfby/pmfby.service.ts:232,274`), on the other auth realm.
An earlier draft of the registry listed a `/SendOTP` under FGMS; no such
endpoint exists.

**Every FGMS reply shares one envelope:**

| field | |
|---|---|
| `responseCode` | `"1"` is success; anything else is failure |
| `responseMessage` | the portal's own prose — never returned to the network |
| `recordCount` | present on the status call |
| `responseDynamic` | the payload |

### Lodge — `POST /krphapi/FGMS/AddKRPHNCIPGrievenceSupportTicket`

**verified** — lines 161-180. Not `InsertGrievenceTicket`, which an earlier
draft named and which does not exist.

Request body: `requestorMobileNo`, `complaintDate`, `receiptSourceID`,
`ticketCategoryID`, `ticketSubCategoryID`, `requestYear`, `requestSeason`,
`applicationNo`, `grievenceDescription` — the portal's spelling of
"grievence" preserved.

Response: `responseDynamic.GrievenceSupportTicketNo` and
`responseDynamic.GrievenceSupportTicketID` (lines 194-198).

### Read — `POST /krphapi/FGMS/GetGrievenceTicketsStatus`

**verified** — lines 86-98 for the call, `main:src/app.service.ts:3343-3374`
for the field names.

Request body: `{requestorMobileNo, GrievenceSupportTicketNo}`.

`responseDynamic` carries twelve named fields:

| returned | v2 target |
|---|---|
| `ApplicationNo` | `enrolmentId` |
| `TicketStatus` | `case.status.name`, and `code` derived from it |
| `ComplaintDate` | `case.filedOn` |
| `GrievenceDescription` | `grievance.description` |
| `TicketCategoryName` | `grievance.category.name` |
| `TicketSubCategoryName` | `grievance.subCategory.name` |
| `CropName` | `case.cropName` |
| `GrievenceSupportTicketID` | **dropped** — internal key |
| `FarmerName` | **dropped** — personal |
| `StateMasterName` | **dropped** — personal |
| `DistrictMasterName` | **dropped** — personal |
| `InsuranceCompany` | **dropped** — not the farmer's complaint |

Four things follow, and each one moved the pack:

1. **No category id comes back.** The portal returns `TicketCategoryName` and
   `TicketSubCategoryName` with no `TicketCategoryID` beside them. The base
   pack therefore requires one of `code` or `name` on a category, not `code`.
2. **No remark, and no date for one.** Nothing in the record is a reply.
   `latestRemark` was our own invention and appears in no source. The PMFBY
   pack now refuses `case.remark` as well as `case.remarkedOn`.
3. **No `requestYear` or `requestSeason` comes back.** `cropYear` and `season`
   are send-only.
4. **The ticket number is not returned either** — only the internal
   `GrievenceSupportTicketID`. On a read the adapter echoes the number the
   caller selected the record by.

Still open: `recordCount` sits beside a `responseDynamic` that v1 reads as a
**single object** (`dynamic.FarmerName`, not `dynamic[0].FarmerName`), while
also dumping the whole thing to a string as a hedge. Whether a multi-ticket
read returns an array is unconfirmed. See "What it leaves open".

---

## The v1 gateway's own vocabulary

Three calls, recorded for the request field meanings.

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

**Response — now verified, by the portal client rather than by this gateway.**
`format_status_result`, lines 396-417, walks every tag and prints
`descriptor.name or descriptor.code` against `value`. It never names a field,
so the reply's shape is invisible from *this* file by construction. §0 above
has the real names, read from the adapter on `main`.

**A correction worth keeping.** An earlier revision of this page listed
fourteen field names as "not verified" and recorded the search for them as
exhausted. That search covered the checked-out working tree only, and the
implementation is on `Beckn`'s `main` branch. Of the fourteen:

| our guess | reality |
|---|---|
| `ApplicationNo`, `GrievenceDescription`, `TicketStatus`, `ComplaintDate`, `TicketCategoryName`, `TicketSubCategoryName` | **real** |
| `TicketCategoryID`, `TicketSubCategoryID`, `TicketStatusID`, `RequestYear`, `RequestSeason`, `latestRemark` | **invented** — in no source, and the pack has been corrected |
| `GrievenceSupportTicketNo` | real, but a *request* field; the read does not return it |
| `CropName`, `recordCount` | real, and we had missed both |

The lesson is procedural, not technical: `BharatVistaar/` holds several
repositories, and "not in the working tree" is not "not in the codebase".

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

**Response — two clients, and the second one reads a field the first
discards.**

`Voice/agents/tools/grievance.py`'s `GenericMessageResponse`, line 177, keeps
two:

| returned | note |
|---|---|
| `Responce` | `"True"` / `"False"`, spelled as shown |
| `message` | the portal's own prose |

The v1 Beckn adapter, `Beckn` on `main`,
`src/services/pmkisan-grievance/pmkisan-grievance.service.ts`, reads more from
the same reply:

| returned | note |
|---|---|
| `GrievanceID` ?? `grievanceId` ?? `GrievanceNo` | **the case identifier** — the portal uses all three spellings |
| `Message` ?? `message` ?? `Remark` | the prose, under whichever key arrives |
| `Status` ?? `Responce` ?? `Rsponce` | the success sentinel; `"False"` means refused |

**PM-KISAN does issue a case identifier.** The direct client's Pydantic model
simply does not declare it, so it is dropped before anyone sees it. This
settles the long-open question in §4 below, and it is why
`PMKISANGrievance/v0.1` allows `case.ticketNo` rather than forbidding it.

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

**`grievance-id` — resolved. It is real, and it comes from the portal.** The
gateway does not mint it: the lodge reply carries it as `GrievanceID`, or
`GrievanceNo` where that is absent (§2). The direct client's model omits the
field, which is the only reason it looked invented.

What that changes: a lodged PM-KISAN grievance has a handle, so `case.ticketNo`
is populated on a lodge reply and the pack permits it. What it does *not*
change: `/GrievanceStatusCheck` still returns every grievance on the identity
and is not documented to repeat the handle per record, so a case **read** may
still have to match on date. The handle helps the caller quote its grievance
back; it does not give the portal a per-grievance read.

**`identity-no` is echoed back.** The gateway returns the registration number
or Aadhaar token it was given. That must not be carried into v2. The field is
`enrolmentId`, marked `no-log` and `no-trace`, and on a case read it is consumed
rather than surfaced: matched against the number the caller sent, then discarded.
The one place it comes back is `Support.orderId` on `on_support`, where it is
returned to the caller who sent it over the same signed exchange.

## Grievance categories

**verified** — `Orchestrator/assets/grievance_types.json` and
`Voice/assets/grievance_types.json` are byte-identical. Ten codes:

`G001` account number not correct · `G002` online application pending for
approval · `G003` installment not received · `G004` transaction failed ·
`G005` problem in Aadhaar correction · `G006` gender not correct · `G007`
payment related · `G008` problem in OTP-based eKYC · `G009` problem in
biometric-based eKYC · `G010` problem in facial-based eKYC

PM-KISAN uses these; PMFBY does not. PMFBY's category is a dotted pair, and the
legacy tool hard-codes it to 3.10.

---

# What this page settles

- **PMFBY's endpoints and auth.** One host, two unrelated realms; the grievance
  calls are `NICUsersLogin`, `AddKRPHNCIPGrievenceSupportTicket` and
  `GetGrievenceTicketsStatus`, and the token goes in a bare `Authorization`
  header. §0. Verified.
- **PMFBY's case record is twelve fields**, named. §0. Verified — this was the
  open item, and it is closed.
- **PMFBY's lodge reply is four fields** and three of them are usable. Verified.
- **PMFBY has no grievance OTP endpoint.** The OTP is the policy flow's,
  `/api/v1/services/nic/getOtp` and `/verifyMobile`. §0. Verified.
- **PM-KISAN issues a case identifier** — `GrievanceID`, or `GrievanceNo`. §2.
  Verified.
- **PM-KISAN's case record is fourteen fields**, not the five the direct client
  models. Verified, from the gateway's labels.

# What it leaves open

1. **Is PMFBY's `responseDynamic` an object or an array?** The reply carries a
   `recordCount` beside it, and v1 reads it as a single object while also
   dumping it to a string as a hedge. One captured response settles it — the
   telemetry store keeps them, `beckn_ext_events.ext_api_response`, filtered on
   `service_name = 'pmfby-greviance'`.
2. **The PMFBY challenge reply.** `challengeIssued.sentTo` and `.expiresAt`
   are our invention; the real reply is unpinned. `method` is a mapping
   constant, so it is ours too, but it restates a fact the v1 flow confirms.
3. **Which PM-KISAN client v2 follows** — the direct portal contract, which has
   no OTP, or the gateway's, which the v1 flow used.
4. **Whether PMFBY still wants a fixed 3.10 category** or the caller's choice,
   which is what our packs assume.
