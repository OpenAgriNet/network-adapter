# Grievance — Implementation

Date: 2026-09-28 · Revised: 2026-09-30 · Status: draft · Internal

The adapter's side of the grievance capability: why the Beckn actions are what they are,
the capability packs, the JSONata mappings and guards for both providers, the PM-KISAN
envelope blocker, and what is still open.

Not caller-facing — the call sequence and the Beckn payloads are covered earlier.

> What the two portals actually accept and return — one line of provenance per field —
> is `grievance-upstream-contracts.md`. Where this page and that one disagree, that one
> wins.

## Why these Beckn actions

**The read is `status`, not `select`.** `/status` is defined as "the CN requests the
current state of an active contract from the PN by providing the contract identifier
received in the `/on_confirm` callback" — this exchange, described verbatim. `select`
would be wrong twice over: it is the negotiation phase, and it carries an explicit
prohibition — "No personal identifiable information (PII) is shared at this stage… The
Contract MUST NOT include billing or fulfillment personal details" — which the read
violates, because PMFBY matches a ticket to the phone it was filed from and PM-KISAN's
identity *is* the query. `init` is the action the spec designates for personal details,
which is why the phone travels there.

**The lodge is `/support`; the read is a `Contract`.** `Support` is
`{orderId, descriptor, channels}` with `additionalProperties: false`, so nothing new can be
added at its top level — but nothing needs to be. `channels` takes the pack's attribute
object whole, `orderId` takes the enrolment the complaint is against, and `descriptor`
takes the category and the farmer's words. A `SupportAction` payload carries no `contract`,
so the provider half of the binding key cannot be read from where the other actions keep
it; the channel carries `providerId` instead, and no adapter change is needed.
`StatusAction`, by contrast, composes a full `Contract` — `required: [id]` is `allOf`-ed
onto `Contract`'s own `required: [commitments]`, so a status payload carries both — and
`commitmentAttributes` is reachable there exactly as it is under `select`.

**PMFBY needs three actions, PM-KISAN two.** PMFBY's portal issues an OTP, so `init` has
a job to do: ask for one, and give the farmer time to read it before `support`. PM-KISAN's
`/LodgeGrievance` asks for no OTP, so there is nothing for an `init` leg to do and the
flow starts at `support`.

**`contract.id` is not the case identifier.** It is a UUID minted by the caller and echoed on
every later action; `Contract.id` is `format: uuid` and a PMFBY ticket number is not one.
PMFBY's case identifier is `commitmentAttributes.ticketNo`. PM-KISAN has none — see
"Case identity" in Open.

**The adapter is stateless.** It stores no contract and does not relate a `support` to the
`init` before it. On PMFBY the real linkage is portal-side: the OTP went to a phone number,
and `support` presents that number with the OTP. On PM-KISAN there is no linkage at all,
which is why `status` has to carry `filedOn`.

**All calls are synchronous** — `init` returns `on_init` in the HTTP 200, not an `Ack`.
No callbacks.

## How the adapter works

```
Beckn request  →  [ mapping.request  JSONata ]  →  provider request
                                                        ↓
Beckn response ←  [ mapping.response JSONata ]  ←  provider response
```

One mapping file per action, both halves in it. Routing is by binding key, read off the
payload: `pmfby|openagrinet:PMFBYGrievance` — provider id from
`message.contract.commitments[].offer.provider.id`, capability from
`…commitments[].commitmentAttributes.@type`.

That second path is **not** the adapter default. `common.BecknV2` points at
`…resources[].resourceAttributes.@type`, so every grievance step must declare
`capabilityCodeAt` explicitly — and `providerIdAt` alongside it, because
`BindingPaths` refuses one without the other. The default stays right for the
domain capabilities, which remain on `resourceAttributes`.

The `support` step re-paths both halves into the channel:

```yaml
providerIdAt:     message.support.channels[].providerId
capabilityCodeAt: message.support.channels[].@type
```

`Support` itself is sealed at three fields and none of them names a participant,
but `channels` is an array of `Attributes`, which is the spec's extensibility
container and is `additionalProperties: true`. The pack puts `providerId` there.
`scheme.code` is not a substitute: it names a scheme rather than a participant,
and it reads `PMFBY` against a registry holding `pmfby`.

Keeping the provider on a payload path rather than naming it statically in
config buys two things. `BindingPaths` already accepts both halves pathed, so
nothing in shared adapter code changes; and the provider path keeps an array in
it, which is what `countAt` counts — so the refusal of more than one entry still
applies and the lodge leg still carries exactly one channel. A static provider
would remove that array and the guard with it.

One detail bites: the path grammar is segments and `[]` with no indices, so
`channels[0]` would be read as a literal field name and match nothing silently.
`Paths.Validate()` only rejects blank segments and would not catch it.

The sections below show the JSONata for each request half and describe the response half
in prose, with the full field-by-field contract in a mapping table per provider. The
response JSONata is mechanical once the provenance of each field is settled, and settling
it is the part worth reviewing.

## The capability packs

Each provider has its own published pack. Payloads carry that pack's `@type` and
`@context`, and the binding key is built from them and the provider id — the latter read
from the contract on `init` and `status`, and from the channel's `providerId` on
`support`. Either way the key comes out the same, so all three legs resolve to the same
registry record and the same credential profile.

| | PMFBY | PM-KISAN |
|---|---|---|
| Pack | `api-schemas/PMFBYGrievance/v0.1` | `api-schemas/PMKISANGrievance/v0.1` |
| `@type` | `openagrinet:PMFBYGrievance` | `openagrinet:PMKISANGrievance` |
| `providerId` (on `support` only) | `pmfby` | `pmkisan` |
| Binding key | `pmfby\|openagrinet:PMFBYGrievance` | `pmkisan\|openagrinet:PMKISANGrievance` |

### Why a pack at all

An earlier draft rode on the generic `AgricultureResource` pack and published nothing, on
the grounds that `AgricultureResourceFields` never sets `additionalProperties: false`, so
every grievance field travels as an undeclared property and passes validation untouched.
That was true, and it is why the mapping guards below are written the way they are. It gave
up three things that turned out to matter: roughly fifteen fields validated by nothing,
none of them carrying an IRI, and a network spec that showed a generic agriculture resource
saying nothing about grievance. The packs close all three.

Naming their own types has a second effect: grievance no longer spends either provider's
one use of the generic `AgricultureResource` type, so a later PMFBY or PM-KISAN capability
is free to take it.

### What a pack enforces, and what it cannot

Neither pack composes `AgricultureResourceFields` any more: that field set is framed
around a Resource that holds information, which a grievance is not. `informationMode` is
now a pack-local field and stays required alongside `@type` and `scheme`;
`subjectCategories` is gone, being a discovery category for the catalog resource rather
than anything a per-case payload carries. Shape rules that hold in both directions live
in the pack.

Per-action requirements do not, and cannot. One `@type` covers every action, so
"`applicationNo` is required on `support` but not on `init`" is not expressible in a pack.
Each pack carries one `if/then` for direction — a `Direct` payload must name the case, its
status, when it was filed and where it came from — and even that is inert at runtime: the
extended-schema validator parses `if/then` and never evaluates it, which
`mandi-price.select.yaml` documents at length.

**Per-action field enforcement therefore lives in the mapping guards.** That is where it
was always going to live.

### The published pack is what gets fetched

The validator resolves the payload's `@context` by string-swapping `context.jsonld` for
`attributes.yaml` (`schemav2validator/extended_schema.go:545`) and fetching it, so what
matters is the live file, not the working copy. **Both packs must be published to
`openagrinet.github.io` before any of this runs**, and `openagrinet.github.io` must appear
in `extendedSchema_allowedDomains` — it was added to `provider-adapter.yaml` for exactly
this. A pack the validator cannot fetch fails every payload.

### `@type` must be a string, not an array

Every pack permits the array form — canonical type plus provider-defined ones — but the
adapter reads the binding key through `ValuesAt`, which keeps only `leaf.(string)`
(`internal/common/paths.go:65`), so an array `@type` produces no binding key and the
request 404s. Filed as a separate bug; grievance payloads use the string form.

### What each pack declares

**PMFBY** — `applicantPhone` is an Indian mobile series, `season` is one of three names,
`cropYear` is four digits, and `grievanceCategory.code` is the dotted pair the adapter
splits on. `challenge` is the shared `Challenge` narrowed to `method: SMS_OTP` and a
six-digit `value`, and is `writeOnly` as a whole object. Its `Direct` branch requires
`ticketNo`, `caseStatus`, `filedOn` and `source`.

**PM-KISAN** — the identity (`registrationNo`), the category, the
description, and the case fields the portal returns. Its `Direct` branch requires
`caseStatus`, `filedOn` and `source`; there is no ticket number to require. Its categories
are a real, closed, ten-value vocabulary, so they are declared as an enum rather than left
to a guard.

The JSON key is `grievanceCategory` in both packs, but it resolves to
`openagrinet:pmfbyGrievanceCategory` in one and `openagrinet:pmkisanGrievanceCategory` in
the other. The two schemes publish incompatible value spaces — a dotted `3.10` against that
closed list — so one IRI could not hold both.

### The challenge is shared, and narrowed

`challenge` and `challengeIssued` are not PMFBY's. They are defined once, network-wide, in
`schema/AgricultureResource/v0.1/attributes.yaml` alongside `SourceReference` and
`IdentifiedDescriptor`, and the pack references and narrows them:

```yaml
challenge:
  allOf:
    - $ref: ".../AgricultureResource/v0.1/attributes.yaml#/components/schemas/Challenge"
    - not: { required: [txnId] }
      properties:
        method: { enum: [SMS_OTP] }
        value:  { pattern: "^[0-9]{6}$" }
```

Three things follow, and they are the reason for the shape.

**Adding a mechanism to the network is one edit.** `ChallengeMethod` is a single enum in a
single shared file — `SMS_OTP`, `AADHAAR_OTP`, `DEVICE_TOKEN` today. Every pack on the
network sees a new entry the moment it is added. No pack has to change to *permit* one.

**Nothing is permitted by accident.** Being in the shared enum does not make a Provider
accept it. A pack accepts only what it narrows to, so PMFBY's published schema advertises
`SMS_OTP` and nothing else — it does not claim to take a device token it has no
prerequisite for. The cost of the second mechanism is paid by the pack that wants it: it
widens its own `enum` and pins the new format at the same time. Narrowing cuts both ways:
the shared `Challenge` offers a `txnId` for mechanisms whose upstream issues a correlator,
and PMFBY refuses it, because it binds the challenge to the phone number alone. A field the
adapter would ignore is better rejected than accepted in silence.

**Enforcement survives the move.** This matters because the obvious alternative does not
work here. A single `Challenge` with `if method = X then value matches Y` would validate
nothing: the extended-schema validator parses `if/then` and never evaluates it, which is
the same limitation [`What a pack enforces`](#what-a-pack-enforces-and-what-it-cannot)
records. `allOf` it *does* evaluate — the packs already compose through it — so a format
pinned in a narrowing is genuinely checked. The six-digit rule is as enforced after this
change as it was when the field was a bare `otp` with a pattern.

The seam, then, is: the shared file owns the *shape* and the vocabulary of mechanisms; the
pack owns *which* mechanisms and *what format*; the mapping guard owns *which action must
carry one*. Each rule sits at the narrowest scope that can still enforce it.

One consequence for the adapter: the prerequisite hook switches on `challenge.method`
rather than assuming. See [`support`](#2-support--the-challenge-plus-the-complaint-ticket-issued).

## What identity is proven

Neither portal proves the farmer. What differs is how much each pretends to.

**PMFBY proves the phone at filing time, and nothing at reading time.** The OTP binds
`support` to someone holding the handset. `status` asks for no OTP, by design: re-texting
one every time a farmer wants an update would be hostile, and the portal does not ask for
it. The consequence should be stated rather than left implied — **anyone holding a ticket
number and its filing phone number can read the case, including the officer's reply.** The
phone is the only thing binding the read and it is asserted, not proven. The OTP protects
*filing*, not *reading*. Two things bound the exposure: both values are needed, and a
ticket number is not derivable from a phone number, so cases are not enumerable. It leaks
only to someone who already holds both.

**PM-KISAN proves nothing, in either direction.** `/LodgeGrievance` takes a registration
number and writes a grievance; it does not check that the caller is that farmer. Anyone
holding an 11-character registration number can file a complaint in someone's name, and
anyone holding one can read the officer replies on it through `/GrievanceStatusCheck`.
That is the portal's design and the adapter cannot repair it.

The v1 Vistaar flow did put an OTP in front of PM-KISAN, but that OTP came from the
separate `pmkisan` scheme-status provider, not from the grievance portal — a Vistaar
policy layer, not a portal requirement. Two honest positions:

1. **Match the portal.** Two actions, as specified here. The adapter is as trustworthy as the
   portal and no more, and the caller is told so.
2. **Keep the v1 OTP layer.** Add an `init` leg that calls the scheme-status provider for
   an OTP, and a verify prerequisite on `support` — structurally identical to PMFBY.
   Costs a second upstream provider in the grievance path.

This design specifies (1), because it is what the grievance API actually offers, and
inventing proof the portal ignores would misrepresent the guarantee. (2) is a network
policy decision rather than a technical one; it is in Open.

So this is a network-level policy question, not a defect in either provider's mapping.
What *is* authenticated on PM-KISAN is the integrator rather than the farmer — the service
token and the AES key both identify the caller, and the next section is about where they
live.

## The blocker: PM-KISAN speaks in envelopes

Everything in the PM-KISAN sections is designed and none of it can ship until this is
built. PMFBY is unaffected and can ship first.

PM-KISAN does not accept or return JSON. Every request body is

```json
{ "EncryptedRequest": "<base64 AES-GCM ciphertext>" }
```

and every response is

```json
{ "d": { "__type": "…", "output": "<base64 AES-GCM ciphertext>" } }
```

The plaintext inside is the JSON the mappings care about. Both the key and the IV are
static, hex, and read from the environment — in the legacy client `GRIEVANCE_KEY_1` is the
key and `GRIEVANCE_KEY_2` is the **IV**, despite the name. That second variable is the
whole of the problem described below.

Nothing in the adapter can do this today:

- JSONata cannot encrypt. There is no crypto in the language and none in the engine.
- The six auth schemes (`none | basic | header | query | oauth2 | tokenQuery`) all attach
  a credential to a request. None transforms a body.
- A Go prerequisite runs *before* the mapping and hands its result to JSONata as `_local`.
  The outbound body does not exist yet when it runs, and the response has already been
  parsed by the time it could look at one. Neither end is reachable from there.
- `encrypter/encrypter.go` is ECDH-derived AES for Beckn transport security between
  network participants. Different key agreement, different envelope, not reusable here.

### What to build

A **body codec**: a per-provider hook that wraps the request body after the request
mapping runs and unwraps the response body before the response mapping runs. It sits
outside JSONata deliberately — this is transport, and the mappings should keep seeing
plaintext JSON.

```yaml
# provider-adapter.yaml, alongside authScheme
pmkisan:
  authScheme: none                        # nothing for the auth layer to attach; see below
  bodyCodec: aesGcmEnvelope
  codecKeyEnv:  PMKISAN_GRIEVANCE_KEY     # names the variable; holds no secret
  codecIvEnv:   PMKISAN_GRIEVANCE_IV      # fixed IV, for portal compatibility only
  serviceTokenEnv: PMKISAN_SERVICE_TOKEN  # the body's TokenNo, read by the prerequisite
  requestEnvelopeField: EncryptedRequest
  responseEnvelopePath: d.output
```

Same discipline as every credential in that file: the `*Env` keys name an environment
variable, and no key material is written into the config, a mapping file, or a registry
row. Mapping files are published; they must stay readable by anyone.

**`authScheme: none` does not mean PM-KISAN is open.** It means the auth layer has nothing
to attach. The portal still has two caller credentials, both in places an auth scheme
cannot reach: `TokenNo`, a static service token that is a *body* field on all three calls,
and the AES-GCM key itself — GCM is authenticated encryption, so the tag verifies only for
a party holding the key, which makes the encryption the caller authentication whatever its
confidentiality is worth under a fixed IV. `codecKeyEnv` is therefore a credential.

Both must **fail closed**, and neither can inherit that from the auth layer, which is what
normally enforces it. An unset `codecKeyEnv`, `codecIvEnv` or `serviceTokenEnv` refuses the
request; the legacy client fails *open* on the last of these and that must not be carried
over. See Open.

The IV is the portal's, not ours: a per-request random IV only works if the portal reads
one back off the wire, and it does not. Implement the codec bug-compatibly to talk to the
portal at all, take the IV as a per-call argument internally even though `codecIvEnv`
supplies the same value every time, and raise it with PM-KISAN — then switching to a
generated IV is a config change rather than a rewrite.

## PMFBY: mappings and guards

### 1. `init` — start the complaint, get a challenge

#### Mapping → provider

```jsonata
(
  $ca := beckn.message.contract.commitments[0].commitmentAttributes;
  { "phone_number": $ca.applicantPhone }
)
```

```json
{ "phone_number": "9876543210" }
```

#### Provider response

```json
{ "status": "success", "message": "OTP sent to registered mobile", "valid_for": 600 }
```

#### Mapping → Beckn (`on_init`)

Commitment stays `DRAFT`; `challengeIssued` is added, as a mapping constant for `method`
plus the two derived values:

```jsonata
(
  $ca := beckn.message.contract.commitments[0].commitmentAttributes;
  {
    "method":    "SMS_OTP",
    "sentTo":    $substring($ca.applicantPhone, 0, 2) & "XXXXXX"
                   & $substring($ca.applicantPhone, 8, 2),
    "expiresAt": $fromMillis($toMillis($now()) + response.valid_for * 1000)
  }
)
```

`response` is the portal's reply and `beckn` the original request, as in every other
mapping; `$ca` is re-declared here because each block is its own expression and nothing
carries over from the request mapping above.

`method` is a constant because PMFBY offers one mechanism. It is still sent, because it is
what tells the caller *what to collect* — a client that reads it keeps working if the
portal ever moves to another mechanism, and the pack would then widen its `enum` rather
than change shape. The OTP itself is never returned, and `applicantPhone` does not come
back — the masked `challengeIssued.sentTo` is there so the farmer can confirm which number
was used, and that is all a response needs to carry.

### 2. `support` — the challenge plus the complaint, ticket issued

#### Guards — refused before the provider is called

There is no contract on this leg — a `SupportAction` has none — so every guard reads from
`message.support`: the application number from `orderId`, the category and the complaint
text from `descriptor`, and the rest from the one channel.

```yaml
required:
  - check: |
      ($s := beckn.message.support; $ch := $s.channels[0];
       $exists($s.orderId) and $exists($ch.cropYear) and $exists($ch.season))
    message: "lodging a PMFBY grievance needs the application number, crop year and season"
  - check: |
      ($s := beckn.message.support;
       $count($match($s.descriptor.code, /^[0-9]+\.[0-9]+$/)) > 0)
    message: "descriptor.code must be <category>.<subCategory>, e.g. 3.10"
  - check: |
      ($s := beckn.message.support; $c := $s.channels[0].challenge;
       $c.method = "SMS_OTP" and $exists($c.value) and $exists($s.descriptor.longDesc))
    message: "support carries an SMS OTP challenge and the complaint text"
```

`$count($match(…)) > 0` rather than `$exists($match(…))`: this engine returns an empty
array on no-match, and `$exists([])` is true — the mandi mapping carries the same note.

These guards enforce what the pack deliberately cannot: which fields a *particular* action
must carry. The pack checks shape — the season enum, the phone pattern — on whatever
payload arrives, and has no `OnDemand` required branch, because what `support` needs is not
what `init` or `status` need. The guards live in `grievance.support.yaml`, so they run only
on `support`, and `init` is not forced to carry fields it cannot have yet.

For three of the fields these guards are the only check there is. `orderId` and
`descriptor` sit outside the channel, so the pack never sees the application number, the
category code or the complaint text on this leg — `x-beckn-path` records where each one
lands, but the validator only reads `channels[]`. The category-code pattern here is
therefore not a repeat of the pack's: on `support` it is the enforcement.

No guard checks `providerId`, and none should: the binding key is built before the mapper
runs, so a payload missing it never reaches a guard — it is refused as unroutable.

#### Two upstream calls, one Beckn action

`support` hits the portal twice — verify the challenge, then lodge — and a mapping file
drives exactly one method and path. So the verify call is a **Go prerequisite**: it runs
before the mapping, switches on `challenge.method`, calls the endpoint that method maps to
with `applicantPhone` and `challenge.value`, and fails the request with `401` if the proof
is wrong. Only on success does the mapping below run.

Switching on `method` is the whole reason the field is sent. Today the switch has one arm,
`SMS_OTP` → the portal's `verifyMobile` endpoint. A second mechanism adds an arm and a
value in the pack's `enum`; it does not change the carrier, the guards' structure, or the
lodge mapping, none of which ever see the proof.

`challenge.value` is consumed there and goes no further: it is not in the lodge body, not
logged, and not echoed in `on_support`.

Prerequisites are registered per **binding key, not per action**
(`internal/common/serve.go:63`), so this hook also fires on `init` and `status`. It reads
`context.action` and returns immediately unless the action is `support`.

#### Mapping → provider

```jsonata
(
  $s   := beckn.message.support;
  $ch  := $s.channels[0];
  $cat := $split($s.descriptor.code, ".");
  $seasons := { "Kharif": "1", "Rabi": "2", "Zaid": "3" };
  {
    "phone_number":          $ch.applicantPhone,
    "application_no":        $s.orderId,
    "request_year":          $ch.cropYear,
    "request_season":        $lookup($seasons, $ch.season),
    "ticket_category_id":    $cat[0],
    "ticket_sub_category_id": $cat[1],
    "grievance_description": $trim($s.descriptor.longDesc),
    "complaint_date":        $fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530"),
    "receipt_source_id":     "134306"
  }
)
```

```json
{
  "phone_number": "9876543210",
  "application_no": "KA2026KH00123456",
  "request_year": "2026",
  "request_season": "1",
  "ticket_category_id": "3",
  "ticket_sub_category_id": "10",
  "grievance_description": "Claim approved in July but no amount credited.",
  "complaint_date": "2026-09-28",
  "receipt_source_id": "134306"
}
```

#### Provider response

```json
{
  "status": "success",
  "ticket_no": "100626000099001",
  "ticket_id": "1",
  "message": "Grievance registered successfully"
}
```

#### Mapping → Beckn (`on_support`)

There is no commitment on this leg and no offer or resource either — the reply is a
`Support` object. `orderId` carries the application number back unchanged, `descriptor` is
the caller's own words echoed back, and `channels[0]` is the case record, its
`informationMode` flipped to `Direct` because it now carries a real case rather than a
request for one. The ticket the portal just issued is in that case record, not in `orderId`:
`orderId` means the thing support is required against, and that is the application both
before and after the call. From here on `ticketNo` identifies the case, and a later `status`
re-enters through a contract whose resource is the thin catalog pointer
`res:pmfby:grievance`.

The lodge response returns only `status`, `ticket_no`, `ticket_id` and `message`. Every
field in `on_support` comes from one of four places:

- **Read from the provider** — `ticketNo`, and nothing else.
- **Stated by the mapping** — `caseStatus.code` is `Registered`, derived from
  `status: "success"` (the portal has no status field on this leg; a freshly lodged
  grievance is registered by definition). No `name` accompanies it: the portal said
  nothing, and an absent `name` is how a caller tells an inferred status from a quoted
  one. `filedOn` restates the `complaint_date` the request just generated, not a value
  the portal echoed — an IST calendar date. `source` is a constant naming the upstream.
- **Changed** — `informationMode`, `OnDemand` → `Direct`.
- **Echoed from the request** — `scheme`, and nothing else.

`ticket_no` maps to `ticketNo` on the channel and nowhere else. The spec says the provider
returns "the ticket reference" without naming the field that holds it, and `orderId` is the
obvious candidate only if you read it as a general-purpose reference slot — but it is
defined as "the order against which support is required", and the ticket is not that. The
channel is where the scheme's own fields live, `ticketNo` is already one of them, and it
carries the same value on `on_status`, so one field means one thing on both legs.
`ticket_id` is dropped. It is the portal's own row id rather than the number the farmer
quotes, and no later call is known to need it — an internal key published to a network with
no consumer. That is the same judgement the case read makes about `TicketStatusID`, and the
pack now makes it consistently. The rule this pack follows is: map what the portal sends, or
say in the README why not. The README says why not. `applicantPhone` and `challenge` are not echoed:
neither belongs in a response.

`applicationNo` **is** echoed here, in `orderId`, unchanged from the ask. It tells the caller
nothing they did not send, which is exactly why it is safe; what it buys is that `orderId`
holds one meaning in both directions and on both providers. The case read echoes it too, on
the commitment, because the portal sends `ApplicationNo` back. It is `class: identifier`,
not personal data, so echoing it is not the concern; the earlier reading that it was is
withdrawn.

### 3. `status` — check the ticket

#### Guards

The portal matches a ticket to the phone it was filed from, so both are mandatory. Absent
a guard, a missing field would reach the upstream as a blank and return somebody else's
answer or none at all. There is no challenge on this leg — it is spent once, at `support` —
so those two fields are the whole of the check; see "What identity is proven".

```yaml
required:
  - check: |
      ($ca := beckn.message.contract.commitments[0].commitmentAttributes;
       $exists($ca.ticketNo) and $exists($ca.applicantPhone))
    message: "checking a PMFBY grievance needs the ticket number and the filing phone number"
```

#### Mapping → provider

```jsonata
(
  $ca := beckn.message.contract.commitments[0].commitmentAttributes;
  {
    "requestorMobileNo":        $ca.applicantPhone,
    "GrievenceSupportTicketNo": $ca.ticketNo
  }
)
```

The upstream misspells "Grievance". The typo is preserved in the mapping and corrected
at the network boundary.

#### Provider response

**Not verified — treat the field names below as a proposal.** They are attributed to
`/krphapi/FGMS/GetGrievenceTicketsStatus` on the demo portal, but of the names in this
block only `GrievenceSupportTicketNo` occurs anywhere we can check, and it occurs in
BharatVistaar as a *request* tag rather than a response field. The legacy client
(`Orchestrator/agents/tools/pmfby_grievance.py`, `format_status_result`, lines 396-417)
renders this reply generically — `descriptor.name or descriptor.code` against `value` —
so it never names a field and the reply's shape is not observable from it. Confirm these
names against PMFBY's own API document before anything is built on them. See
`grievance-upstream-contracts.md`, the master reference for what each portal actually
accepts and returns; where it and this page disagree, it wins.

Abridged to the fields that matter:

```json
{
  "responseObject": null,
  "responseDynamic": {
    "GrievenceSupportTicketNo": "100626000099001",
    "TicketStatus": "Open",
    "TicketStatusID": 109301,
    "ComplaintDate": "2026-05-11",
    "ApplicationNo": "040108251010160770605",
    "GrievenceDescription": "Claim is not recieved by me.",
    "TicketCategoryID": 3,
    "TicketCategoryName": "Enrollment",
    "TicketSubCategoryID": 10,
    "TicketSubCategoryName": "Portal Issues Login",
    "RequestYear": 2025,
    "RequestSeason": 1,
    "latestRemark": "",

    "FarmerName": "…", "RequestorMobileNo": "…", "Email": null,
    "StateMasterName": "…", "DistrictMasterName": "…", "SubDistrictName": "…",
    "GramPanchayat": "…", "NyayPanchayat": null, "VillageName": null,
    "InsurancePolicyNo": "…", "InsuranceCompany": "…"
  },
  "responseCode": 1,
  "responseMessage": "Fetched successfully",
  "recordCount": 0
}
```

Three things about the envelope. The payload is under `responseDynamic`, not
`responseObject`, which is `null` here. Success is `responseCode: 1`, not an HTTP status
and not a `status` string. And `recordCount` is `0` despite a record being present, so
the mapping must not gate on it — read `responseDynamic` and test that directly.

#### Mapping → Beckn (`on_status`)

`ComplaintDate` is already ISO, so `filedOn` needs no date conversion — unlike `support`,
where the adapter generates the date. `caseStatus` splits in two: `name` carries
`TicketStatus` verbatim, and `code` is the network's `CaseStatusCode` the adapter maps
that phrase to. The portal also returns `TicketStatusID`, an opaque internal key that is
not mapped at all.

The mapping is a lookup, not a transformation. A phrase the lookup does not hold falls to
`UnderReview` — never to a terminal code, which must come from the portal — and the phrase
still reaches the caller in `name`. So an unfamiliar status is rendered correctly and is
never mistaken for a finished case.

The response is richer than the request. It carries its own `GrievenceSupportTicketNo`,
so `ticketNo` need not be echoed, and it returns `ApplicationNo`, `GrievenceDescription`,
the category pair with names, `RequestYear` and `RequestSeason` — all of which map
straight through. `RequestYear` arrives as a number and is stringified. The two category
names are joined with a slash into `grievanceCategory.name`, mirroring the dot that joins
the two codes.

`latestRemark` becomes `caseRemark`. The portal sends an empty string rather than
omitting it when nothing has been recorded, so the adapter omits the field instead — an
absent `caseRemark` reads as "nothing recorded yet". Anything longer than 2000 characters
is rejected by the pack; this is unvalidated upstream free text going into a response.
There is no remark date anywhere in the response, which is why this pack has no
`remarkedOn`.

`applicantPhone` is deliberately not echoed, even though `RequestorMobileNo` comes back.

**The response mapping is an allow-list.** The record also carries the farmer's name,
mobile number, email, and the full state / district / sub-district / panchayat / village
hierarchy, plus the insurance policy number and insurer. None of it is mapped, logged or
traced. A passthrough here would repeat the v1 `identity-no` echo at far greater scale.

The commitment stays `ACTIVE` while the case is open and should become `CLOSED` on a
terminal status — but which `TicketStatus` values are terminal is not yet known, so
today everything maps to `ACTIVE`. See Open.

### Field mapping

| Beckn (`commitmentAttributes`) | Provider |
|---|---|
| `applicantPhone` | `phone_number` / `requestorMobileNo` |
| `applicationNo` | `application_no` — returns as `ApplicationNo` |
| `cropYear` | `request_year` — returns as `RequestYear`, a number; stringified on the way back |
| `season` | `request_season` — the name is mapped to the portal's code: Kharif 1, Rabi 2, Zaid 3; returns as `RequestSeason` |
| `grievanceCategory` | `ticket_category_id` + `ticket_sub_category_id` (code split on `.`); returns as `TicketCategoryID`/`TicketSubCategoryID`, with `TicketCategoryName`/`TicketSubCategoryName` joined by ` / ` into `name` |
| `grievanceDescription` | `grievance_description` — returns as `GrievenceDescription` |
| `ticketNo` | `ticket_no` on `support`, returned on the channel and never in `orderId`; `GrievenceSupportTicketNo` both as the `status` query and in its response |
| `caseStatus` | `TicketStatus` verbatim into `name`; `code` is the `CaseStatusCode` it maps to, falling back to `UnderReview` |
| `caseRemark` | `latestRemark` — omitted when empty, capped at 2000 characters. PMFBY publishes no remark date, so there is no `remarkedOn` |
| `filedOn` ← | `ComplaintDate` on `status`, already ISO; on `support` it restates the generated `complaint_date`. An IST calendar date |
| `challenge` | `value` goes to the verify endpoint only, selected by `method`; never on the lodge call, never in a response |
| `challengeIssued` | `on_init` only — `method` is the constant `SMS_OTP`, `sentTo` is the masked request phone, `expiresAt` is the reply's `valid_for` seconds from now. All three are required; `txnId` is refused |
| `informationMode` | not sent; `OnDemand` on requests, `Direct` on responses that carry a real case |
| `scheme` | not sent; echoed unchanged, it identifies the scheme |
| `source` | not sent; a mapping constant naming the upstream portal |
| — | `complaint_date` = `$fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530")`, generated in IST — the farmer does not backdate |
| — | `receipt_source_id` = `134306` (provider constant) |
| consumed | `status` — drives `caseStatus` on `support` and the error path on both legs; not surfaced as a field |
| dropped | `ticket_id` on `support` — the portal's own row id, an internal key with no consumer; `TicketStatusID` — an opaque internal key the derived `caseStatus.code` replaces; `message` — the portal's own text, logged redacted and never returned |
| dropped (PII) | `FarmerName`, `RequestorMobileNo`, `Email`, `StateMasterName`, `DistrictMasterName`, `SubDistrictName`, `GramPanchayat`, `NyayPanchayat`, `VillageName`, `InsurancePolicyNo`, `InsuranceCompany` — never mapped, logged or traced |

## PM-KISAN: mappings and guards

### 1. `support` — lodge the grievance

#### What the request carries

`support.orderId` carries the farmer's PM-KISAN registration number, and the channel
carries nothing but the scheme. The registration number is both the identity the portal
authenticates on and the enrolment the complaint is against — the upstream takes one
reference, `IdentityNo`, and nothing else in its API names a case — so it sits in the same
slot PMFBY fills with the application number.

Aadhaar is not offered: the grievance flow has no use for one, and an identifier the
network does not need is an identifier it should not collect.

#### Guards

```yaml
required:
  - check: |
      ($s := beckn.message.support;
       $count($match($s.orderId, /^[A-Za-z0-9]+$/)) > 0)
    message: "orderId must be a non-empty alphanumeric registration number"
  - check: |
      ($s := beckn.message.support;
       $s.descriptor.code in
         ["G001","G002","G003","G004","G005","G006","G007","G008","G009","G010"])
    message: "descriptor.code must be one of G001-G010"
  - check: |
      ($s := beckn.message.support;
       $length($trim($s.descriptor.longDesc)) >= 10)
    message: "the grievance description must say something - at least 10 characters"
```

The category guard is an exact list because the vocabulary is closed and known. PMFBY's
equivalent is a shape check because its vocabulary is not.

**The identity guard is deliberately loose.** The legacy client never validates a
registration number at all — anything that is not twelve digits falls through to the
`Reg_No_*` path untouched. "Eleven alphanumeric characters" appears only in a tool
docstring, and a guard built on a docstring rejects valid grievances at the adapter,
before the farmer's complaint ever reaches the portal. So the check is only non-empty
and alphanumeric. Tighten it to the exact format once the
portal's integration document states one; see Open.

#### No second upstream call

A PM-KISAN `support` is one call to the portal. There is no OTP to verify and no identity
to exchange — the registration number goes up as it arrived. A **Go prerequisite** still
runs, but only to supply `serviceToken`, because every call needs a `TokenNo` and a
published mapping file is no place for a shared secret.

The registration number is never logged and never traced, and it appears in exactly one
response field: `orderId` on `on_support`, handed back to the caller who sent it over the
same signed exchange. Nowhere else — a case read does not return it.

#### Mapping → provider

`Type` is fixed by the action: `Reg_No_Details` to lodge, `Reg_No_Status` to read.

```jsonata
(
  $s := beckn.message.support;
  {
    "Type":                 "Reg_No_Details",
    "TokenNo":              _local.serviceToken,
    "IdentityNo":           $s.orderId,
    "GrievanceType":        $s.descriptor.code,
    "GrievanceDescription": $trim($s.descriptor.longDesc)
  }
)
```

`TokenNo` is the portal's static service token, not a farmer credential. It comes from
the prerequisite as `_local.serviceToken` rather than being written into the mapping,
because mapping files are published and it is a shared secret however weak.

The body codec encrypts this object and posts `{"EncryptedRequest": "…"}`.

```json
{
  "Type": "Reg_No_Details",
  "TokenNo": "…",
  "IdentityNo": "UP12345678A",
  "GrievanceType": "G003",
  "GrievanceDescription": "Third instalment for 2026 has not been credited."
}
```

#### Provider response

After the codec unwraps `d.output`:

```json
{ "Responce": "True", "message": "Grievance registered successfully" }
```

`Responce` is misspelled upstream, and its value is the **string** `"True"` or `"False"`,
not a boolean. The mapping matches the upstream on both counts — the spelling and the
string comparison — and neither leaks past the network boundary.

**It is also optional here.** The legacy client models the lodge response as
`Responce: Optional[str]` and treats an absent field as success, with the comment that the
response "typically carries `message` and sometimes `Responce`". So the mapping must test
for failure, not for success: `Responce = "False"` is the error path, and anything else —
`"True"` or nothing at all — is a registered grievance. A mapping written the other way
round (`Responce = "True"` ? registered : error) would `502` every successful lodge on
which the portal omitted the field. `/GrievanceStatusCheck` is different: `Responce` is
always present there and is tested directly.

#### Mapping → Beckn (`on_support`)

**The portal returns no ticket number.** There is nothing to quote back — no case id, no
reference, not even a timestamp. So `orderId` comes back as it went up, the registration
number unchanged, where PMFBY's would carry a ticket. Provenance of each field in
`on_support`:

- **Read from the provider** — nothing but the success/failure decision.
- **Stated by the mapping** — `caseStatus.code` is `Registered`, asserted whenever
  `Responce` is not `"False"`, with no `name`: the portal said nothing, and the absent
  `name` is how a caller tells an inferred status from a quoted one. `filedOn` is
  `$fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530")`, generated in IST,
  because the portal returns no date. `source` is a constant naming the upstream.
- **Changed** — `informationMode`, `OnDemand` → `Direct`.
- **Echoed from the request** — `scheme` on the channel, and `orderId` and `descriptor` on
  the support object itself. The description is echoed so this response has the same shape
  as the one `status` returns.

Echoing `orderId` is deliberate and is the only place the registration number comes back.
It discloses nothing: the caller sent it, and it returns over the same signed exchange. On
a case read it is not surfaced at all — a `Contract` has no `orderId`, and the attribute is
not mapped either.

There is no resource or offer on this leg. `res:pmkisan:grievance`, the fixed catalog
pointer, appears on the `status` contract and does not change on any response. Per-case
identity lives elsewhere: `contract.id` is the caller's handle on this grievance, and
`filedOn` is what a later `status` uses to find it at the portal. This is the one place the PM-KISAN design is weaker than PMFBY, which
has a portal-issued `ticketNo` for the same job. See `status` below, and `grievance-id` in
Open.

**`filedOn` is shifted to IST before truncating**, which is why the mapping above does not
simply truncate `$now()`. Because `status` matches on it, a day's drift would not merely
mislabel the grievance — it would fail to find it. See Open.

The caller should tell the farmer to keep their registration number, because that — not
anything in this response — is what retrieves the grievance later.

### 2. `status` — read the replies

#### What the request carries

**`filedOn` is the discriminator, and it is why the request carries three fields rather
than two.** The portal has no per-grievance endpoint: `/GrievanceStatusCheck` takes an
identity and returns *every* grievance on it. But `/on_status` answers for **one**
contract, so the adapter has to select the record this contract is about, and the adapter
is stateless — it stored nothing at `support`. The caller did: `on_support` returned
`filedOn`, and the caller sends it back here. The portal's `GrievanceDate` is then matched
against it.

That is the honest limit of this design. Two grievances filed on the same identity on the
same day are indistinguishable, because the portal returns nothing else stable to key on.
A portal-issued grievance id would replace `filedOn` here and close the gap; see Open.

#### Guards

The identity guard, restated in `grievance.status.yaml` against the contract this action
does carry — the registration number must be non-empty alphanumeric, read from
`commitmentAttributes.registrationNo` rather than from `orderId`, because a `Contract` has no
`orderId` — plus one this action needs on its own: `filedOn` must be present and ISO, since
without it there is nothing to match the returned records against. Guards are per mapping
file, so this is a copy, not a reference; the category and description guards have no place
here and are not copied.

#### Mapping → provider

```jsonata
(
  $ca := beckn.message.contract.commitments[0].commitmentAttributes;
  {
    "Type":       "Reg_No_Status",
    "TokenNo":    _local.serviceToken,
    "IdentityNo": $ca.registrationNo
  }
)
```

Same two-by-two, `_Status` suffix this time.

#### Provider response

```json
{
  "Responce": "True",
  "message": null,
  "details": [
    {
      "Reg_No": "UP12345678A",
      "GrievanceDate": "28-09-2026",
      "GrievanceDescription": "Third instalment for 2026 has not been credited.",
      "OfficerReply": "Bank account seeded with Aadhaar; payment in next cycle.",
      "OfficeReplyDate": "06-10-2026"
    },
    {
      "Reg_No": "UP12345678A",
      "GrievanceDate": "14-03-2026",
      "GrievanceDescription": "Name spelling incorrect.",
      "OfficerReply": null,
      "OfficeReplyDate": null
    }
  ]
}
```

#### Mapping → Beckn (`on_status`)

**`details` is a list; `on_status` returns the one record that matches.** The mapping
converts each `GrievanceDate` from `dd-MM-yyyy` to ISO and keeps the record whose date
equals the request's `filedOn`. The rest are discarded — they belong to other contracts.
The legacy client rendered `details[0]` and discarded the rest too, but that was a display
shortcut that happened to land on the newest record; this is a match, not a guess.

```jsonata
$ca.filedOn = $fromMillis($toMillis($.GrievanceDate, "[D01]-[M01]-[Y0001]"),
                          "[Y0001]-[M01]-[D01]")
```

`caseStatus` carries the portal's `GrievanceStatus` verbatim in `name`, with `code` the
`CaseStatusCode` the adapter maps that phrase to — PMFBY's treatment exactly, which is the
point of defining the vocabulary once. The direct client's model drops that field, but the
portal does return it (the v1 adapter formats it and exposes a `grievance-status` tag).
Only when it is absent does the mapping infer, and then it emits `code` alone:

- `OfficerReply` present → `Replied`
- `OfficerReply` null or empty → `UnderReview`

Emitting no `name` on those two branches is deliberate. It is the only signal a caller has
that the state is the adapter's reading rather than the portal's word, and it costs nothing
to provide. An unmapped `GrievanceStatus` phrase also falls to `UnderReview`, but keeps its
`name` — so "inferred" and "unrecognised" stay distinguishable.

`GrievanceStatus` does not appear in the sample response above, and that is not an
oversight: no captured response carrying it exists anywhere in the legacy tree, so rather
than invent a value the sample shows the inference branch only. The pack's examples do the
same, for the same reason.

The resource id is `res:pmkisan:grievance` — the fixed catalog pointer, unchanged on every
response that carries one. A resource that keeps its name across responses is the point: it
names the catalog entry rather than the case, so nothing about it varies with the record
matched. It also carries no farmer identifier. An earlier draft keyed it on the matched
record's `Reg_No` and `GrievanceDate`, which broke both properties at once — the id changed
between the two responses, and it published the registration number in a field that `no-log`
and `no-trace` cannot reach and that logs and traces retain by default.

Two empty cases, and both are answers rather than faults: `Responce: "False"` means no
grievances on this identity, and a non-empty `details` with no date match means none of
them is this contract's. Both return `202` with `BIZ_NO_RESULTS_FOUND` rather than an
`on_status`, so the two are indistinguishable to the caller — deliberately, since the
difference is about what PM-KISAN holds on the identity, not about this contract.
`resources` is unaffected: it is the fixed catalog pointer and says nothing about whether a
case exists.

The second record in the provider response — the March grievance — is dropped: its date
does not match, so it belongs to a different contract.

`Reg_No` comes back in every record and is **discarded**. It appears in no field and in no
id — it is only what the caller just sent. The one place the registration number comes back
is `orderId` on `on_support`, and a `Contract` has no `orderId`, so on a case read it is
matched and then dropped.

`caseRemark` and `remarkedOn` are omitted when null rather than emitted as `null` — an
absent field reads as "nothing recorded yet," which is what it means. `caseRemark` is
capped at 2000 characters by the pack, being unvalidated upstream free text.

### Field mapping

| Beckn (`commitmentAttributes`) | Provider |
|---|---|
| `registrationNo` | `IdentityNo` — the registration number, sent as it arrived. It rides in `support.orderId` on the lodge leg and in `commitmentAttributes` on the read |
| action | selects the `Type` suffix: `_Details` on `support`, `_Status` on `status` |
| `grievanceCategory.code` | `GrievanceType` (`G001`–`G010`, verbatim) |
| `grievanceDescription` | `GrievanceDescription` out; `GrievanceDescription` in on `status` |
| `caseStatus` | `code` only: `Registered` on `support` unless `Responce` is `"False"`; on `status`, `GrievanceStatus` into `name` with the mapped `CaseStatusCode` in `code`, falling back to `Replied`/`UnderReview` from `OfficerReply` when the portal publishes no status |
| `filedOn` | sent on `status` and matched against `GrievanceDate`; generated on `support`, which returns no date |
| `caseRemark`, `remarkedOn` | `OfficerReply`, `OfficeReplyDate` — omitted when null. The network does not adopt the portal's field name; the portal itself is inconsistent about the author (`OfficeReplyDate`, not `OfficerReplyDate`) |
| `informationMode` | not sent; `OnDemand` on requests, `Direct` on responses |
| `scheme` | not sent; echoed unchanged |
| `source` | not sent; a mapping constant naming the upstream portal |
| — | `TokenNo` = the portal's static service token, from the prerequisite |
| consumed | `Responce` — drives `caseStatus`, the empty-result path, and the error path |
| dropped | `Reg_No` — returned on every `status` record and discarded; it appears in no field and in no id |
| dropped (PII) | `Farmer_Name`, `Father_Name`, `Gender`, `MobileNo`, `StateName`, `DistrictName`, `BlockName`, `RevenueVillageName` — returned on every `status` record; never mapped, logged or traced |
| dropped | `message`, `__type` — portal prose and envelope chatter |
| echoed once | `registrationNo` — returned in `orderId` on `on_support` to the caller who sent it, and nowhere else; never logged, never traced, never surfaced on a case read |

## When it fails

The status codes and the NACK body are in the Errors section. What produces them:

**Guards run before any upstream call**, so a malformed payload never reaches the portal.

**An empty answer is not an error, but it is still said out loud.** An unknown ticket, or no
PM-KISAN record matching `filedOn`, returns `202` with an `AckNoCallback` body carrying
`BIZ_NO_RESULTS_FOUND` — `status: "ACK"`, because the request was accepted and processed.
This needs no schema change: no field of either pack is involved.

Returning a commitment with `commitmentAttributes` omitted would also be spec-legal, and is
what an earlier draft did. It was dropped because a missing field is not a message — a
consumer cannot distinguish it from an adapter bug that lost the field.

**The portals signal failure in the body, not the status line.** PMFBY sends
`status != "success"`; PM-KISAN sends `Responce: "False"`, and on `/LodgeGrievance` an
*absent* `Responce` is success rather than failure. Both become `502` /
`NET_DOWNSTREAM_UNAVAILABLE`.

**The portal's message does not go into the response.** `common/http.go` already refuses this
for a non-2xx — a failure body may hold a stack trace or an internal hostname, and a rejected
request is often quoted back with the credential in it, so it is logged redacted and never
returned. The body-level failures above never reach that code, so the mappings must hold the
same line themselves: report that the portal rejected the request, not what it said.

**Decryption failure is the failure mode the envelope adds.** A response that will not
decrypt, or that has no `d.output`, looks like a malformed response and is almost always key
or IV drift, so it is a `500` / `NET_INTERNAL_ERROR` — our configuration — rather than a
`502`, which would blame a portal that answered correctly. It must not log the ciphertext,
which may be a valid envelope the adapter cannot open, and the request is not retried.

No error path carries `challenge.value`, `registrationNo`, or the service token.

## What gets added

```
network-specs/api-schemas/PMFBYGrievance/v0.1/           (published; must be live first)
network-specs/api-schemas/PMKISANGrievance/v0.1/         (published; must be live first)

beckn-onix/config/mappings/pmfby/grievance.{init,support,status}.yaml
beckn-onix/config/mappings/pmkisan/grievance.{support,status}.yaml

beckn-onix/pkg/plugin/implementation/Grievance/          one plugin, both providers
                                                         + challenge-verify hook (pmfby)
                                                         + service-token hook   (pmkisan)
beckn-onix/pkg/plugin/implementation/internal/common/    body codec + config plumbing  ← new

registry: 2 × (1 SchemaRegistry + 1 Participant + 1 ProviderSchema)   (rows in Registry)
```

One plugin, two providers, two binding keys, two sets of mappings. The directory is named
for the capability it implements, as `MandiPrice/` and `AgricultureFacility/` are. The
prerequisite hook is where the providers diverge — PMFBY verifies a challenge, PM-KISAN
only supplies the service token — and both dispatch on the binding key they were registered
under.

Most of the field differences need no code at all. Registration number instead of
application number, `G001`–`G010` instead of a dotted pair, no season: all declared in the
packs, so the mappings differ only in their guards and key names.

**The body codec is the only genuinely new machinery** — the one item that touches shared
adapter code rather than adding a provider beside the existing ones. Everything else is
configuration and mapping files. PMFBY needs none of it and can ship first.

## Open

### Blocking

- **The PMFBY case-read contract is unverified** — the whole `on_status` mapping rests on
  it. Thirteen of the fourteen field names in the response above occur in no source we
  hold: not in BharatVistaar, not in any Beckn specification, not anywhere on disk. The
  fourteenth, `GrievenceSupportTicketNo`, occurs only as a *request* tag. The legacy client
  renders that reply generically and never names a field, so the shape is not recoverable
  from the code. Either these names came from a PMFBY API document we were given and did
  not keep, or a previous draft invented them — and the mapping, the pack's `result_fields`,
  the `on_status` examples in every doc, and the demo host `pmfbydemo.amnex.co.in` all
  inherit whichever it is. Get the document from PMFBY, or get one live response, before
  anything is built on this. `grievance-upstream-contracts.md` records the provenance of
  each field either way. PM-KISAN is unaffected.

- **Body codec** — nothing on PM-KISAN ships without it. Designed under "The blocker"
  above; needs approval before implementation, since it is the only change to shared
  adapter code in this design. It carries a second, smaller change with it:
  `ParseProviderAuth` rejects any setting outside the closed `authFields` vocabulary, so
  the codec keys need their own parser before the config will start. JSONata cannot
  encrypt, the six auth schemes all attach a credential rather than transform a body, and
  a prerequisite runs before the outbound body exists — so this is a transport-level
  change and there is no cheaper place to put it. PMFBY is unaffected and can ship first.

### Applies to both providers

- **The `x-oan-pii` markings are not enforced.** Each pack now marks its personal-data
  fields with a `class` and a `handling` list, but `x-` keys are inert to the validator.
  A CI check should assert that no property marked `no-echo` appears in any `Direct`
  example, and that `no-forward` fields reach no downstream call. Until that check exists
  the marking is documentation.

- **The negotiation chain is skipped entirely, and `/status` reads a contract that was
  never confirmed.** The spec chains its preconditions `select` → `init` → `confirm`.
  `/init` states that "A `/select` request MUST have completed and the CN MUST have received
  an `/on_select` callback with a priced quote before this endpoint may be called", and
  `/status` is defined against "the contract identifier received in the `/on_confirm`
  callback". This design never calls `/confirm`: the grievance is lodged through `/support`,
  and `contract.id` on the read is a UUID the caller mints, which the provider has never
  seen in a prior call. PMFBY's `/init` leg exists only to fetch an OTP and follows no
  `/select`.

  A grievance has no price, so there is no quote to receive and no payment to prove — which
  is why the chain is skipped rather than shortened. All of these requirements live in
  endpoint and schema *descriptions* rather than in the schemas themselves, and there is no
  "MAY skip" language anywhere to lean on, so nothing fails validation today. The
  alternative, a `select` leg negotiating a price for filing a complaint, is worse than the
  deviation. Recorded so a reviewer meets it here rather than discovering it in the spec;
  confirm with the network's spec authority that a zero-consideration contract, and a
  `/status` read against one that was never confirmed, are meant to work this way.

- **Generated dates are shifted to IST; the assumption behind it is unconfirmed.** `$now()`
  is UTC, so a truncated UTC date lands on the previous day for anything filed between
  00:00 and 05:30 IST. On PMFBY the generated `complaint_date` is *sent to the portal*, so
  the drift would be written upstream; on PM-KISAN the generated `filedOn` is what a later
  `status` matches against, so it would not merely mislabel the grievance — it would fail
  to find it. Both mappings above therefore shift before truncating, with
  `$fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530")`. What remains open is
  only the premise: confirm with both portals that they record dates in IST.

- **Terminal statuses — ask both portals for their full status vocabulary.** Neither
  documents which values close a case, so every commitment sits at `ACTIVE` and
  `status.descriptor.code` carries a constant. `caseStatus` holds the real state, but in
  each portal's own words, so a consumer without this pack cannot tell an open case from a
  finished one — which is the only job the coarse enum has. What is needed is the complete
  list of values each portal can return and which of them are terminal; `CLOSED` is then a
  mapping rather than a guess. Until then the field stays `ACTIVE`: inventing a terminal
  set would be worse than leaving it flagged.

- **Endpoint paths and credentials** need the portals' integration documents. PMFBY's
  request body is reconstructed from the v1 *gateway's* tag vocabulary — the legacy repo
  holds the caller-side tools but no client that ever called PMFBY directly, so even the
  request field names are one translation removed from the portal's own. Its response is
  worse off; see the blocking item above. The two paths `/SendOTP` and
  `/InsertGrievenceTicket` are marked `TODO: path unconfirmed` in the registry for the same
  reason. PM-KISAN's two paths
  and its `TokenNo` are taken from the legacy client; the base URL, the token's real
  value, and whether it is per-integrator are all unconfirmed.

- **`/support` lodges the grievance, and two of its preconditions are read generously.**
  `/support` states that "The CN MUST hold a confirmed Contract id or a reference to another
  supported entity before this endpoint may be called". There is no confirmed contract here,
  so this design leans on the second clause: `orderId` holds a reference to a supported
  entity — a PMFBY application or a PM-KISAN enrolment — which the farmer does hold before
  calling. That is a reading, not something the spec spells out.

  `Support.channels` is also described as "domain-specific channel details such as phone,
  email, or chat endpoints", and the lodge leg puts a case record there instead. `Attributes`
  is an open object and the schema permits it, but the prose does not anticipate it. Both
  readings go to the network's spec authority together with the deviations above.

  The literal reading — a helpline lookup once a case is filed — still fits `channels` and
  remains additive: each scheme's helpline number and grievance email, static per provider,
  answered from configuration with no upstream call. It is not specced here because neither
  scheme's published helpline details have been sourced; when they are, they become a
  second entry in `channels` beside the case record, which is why a consumer must select by
  `@type` rather than by position.

### PMFBY

- **`grievanceCategory`** — only `3`/`10` is used today, and the
  `<category>.<subCategory>` code shape is this design's invention. If the portal publishes
  a real list, enumerate it in the `support` guard rather than the pack: the list is
  provider-specific, and PM-KISAN's `G001`–`G010` shares the field but not the values.
- **`ticket-id`** — dropped, after a draft that mapped it. Nothing reads it back and it is
  an internal key, so publishing it costs without paying. Ask PMFBY whether the case read
  keys on it; if it does, the field comes back, which is far cheaper than retiring a
  published one.
- **`receipt_source_id` `134306`** — assumed fixed for the Vistaar channel.

### PM-KISAN

- **Credential fail-closed** — `codecKeyEnv`, `codecIvEnv` and `serviceTokenEnv` sit
  outside the auth layer, so they do not inherit its "absent variable fails the request"
  rule. The codec and the prerequisite must enforce it themselves. Confirm no deployment
  relies on the legacy `PMK_123456` default.
- **Static IV** — raise with PM-KISAN. The codec should take the IV per call so the fix is
  configuration, not a rewrite.
- **OTP** — whether the network requires proof of identity the portal itself does not.
  A policy decision; see "What identity is proven". If yes, an `init` leg and a verify
  prerequisite, sourced from the `pmkisan` scheme-status provider as in v1.
- **Registration-number format** — the guard checks only "alphanumeric, non-empty". The
  legacy client validates nothing, and the 11-character figure comes from a docstring.
  Ask PM-KISAN for the real format and tighten the guard to it; until then a loose guard
  is the safe error.
- **Case identity** — the resource is a fixed catalog pointer and carries no case identity.
  `contract.id` is the caller's handle, and `filedOn` is what actually retrieves the
  grievance from the portal — and it collides for two grievances filed on the same identity
  the same day. If the portal has an internal grievance id it does not currently return,
  asking for it in the response would close the collision and give the pack a real case
  identifier; see `grievance-id` below.
- **`grievance-id`** — the v1 adapter reads a `grievance-id` tag, but nothing in the portal
  client produces one and no sample response exists in the tree. If it turns out to be
  portal-issued, it replaces `filedOn` as the `status` discriminator and the pack needs a
  case-identifier field. Resolve against a live response before v1.0.
- **`caseStatus` vocabulary** — the portal does return a `GrievanceStatus` field (the v1
  adapter formats it and exposes it as a `grievance-status` tag); the direct client's
  `GrievanceStatusDetail` model simply drops it. Carry it verbatim in `caseStatus.name`
  and map it to a `CaseStatusCode` for `code`; fall back to inferring
  `Replied`/`UnderReview` from `OfficerReply` only when it is absent. What is still needed
  is the phrase list itself, so the lookup can be written — until then every phrase falls
  to `UnderReview`, which is safe but uninformative.
- **"Show me all my grievances"** — the portal read returns every grievance on an identity,
  and `status` deliberately discards all but the matching one, because `/on_status` answers
  for one contract. If the network wants the full list as a farmer-facing view, it is a
  separate read and needs its own action — a discovery, not a contract query. Not specced
  here.
