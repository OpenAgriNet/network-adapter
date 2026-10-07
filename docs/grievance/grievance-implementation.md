# Grievance — Implementation

Date: 2026-09-28 · Revised: 2026-10-06 · Status: draft · Internal

The adapter's side of the grievance capability: why the Beckn actions are what they are,
how a caller discovers the two desks in the first place, the capability packs, the
JSONata mappings and guards for both providers, the PM-KISAN envelope blocker, and what
is still open.

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
it; the channel carries `provider` instead, and no adapter change is needed.
`StatusAction`, by contrast, composes a full `Contract` — `required: [id]` is `allOf`-ed
onto `Contract`'s own `required: [commitments]`, so a status payload carries both — and
`commitmentAttributes` is reachable there exactly as it is under `select`.

**PMFBY needs three actions, PM-KISAN two.** PMFBY's portal issues an OTP, so `init` has
a job to do: ask for one, and give the farmer time to read it before `support`. PM-KISAN's
`/LodgeGrievance` asks for no OTP, so there is nothing for an `init` leg to do and the
flow starts at `support`. Which of the two a caller is looking at is published rather
than assumed — see [`Discovery`](#discovery-how-a-caller-finds-any-of-this).

**`discover` comes before all of them, and is answered from a catalog, not a portal.**
The two catalogs are published once and indexed; no grievance call reaches a portal
until the caller has chosen a desk. That is also the only leg in this design that
touches neither upstream, which is why it needs no mapping file and no JSONata.

**`contract.id` is not the case identifier.** It is a UUID minted by the caller and echoed on
every later action; `Contract.id` is `format: uuid` and a portal's ticket number is not one.
On both providers the case identifier is `commitmentAttributes.case.ticketNo`. The
difference is what it is good for: PMFBY's is the read key, PM-KISAN's is a handle the
farmer quotes, because that portal has no per-grievance endpoint. See "Case identity" in
Open.

**The adapter is stateless.** It stores no contract and does not relate a `support` to the
`init` before it. On PMFBY the real linkage is portal-side: the OTP went to a phone number,
and `support` presents that number with the OTP. On PM-KISAN there is no linkage at all,
which is why `status` has to carry `case.filedOn`.

**All calls are synchronous** — `init` returns `on_init` in the HTTP 200, not an `Ack`.
No callbacks.

## Discovery: how a caller finds any of this

Every leg above starts from identifiers the caller is assumed to already hold —
`off:pmfby:grievance`, `res:pmfby:grievance`, and the participant id `pmfby` that
routes the call. Nothing in this design publishes them, so an experience layer has
no way to learn them except by being told out of band. This section is the leg that
closes that: two catalogs the adapter publishes once, and the `discover` a caller
issues to find them.

It answers three questions and is not meant to answer more.

**Who to call.** `bppId` on the `on_discover` context names the adapter, whose address
the registry resolves. `provider.id` becomes `offer.provider.id` on `init` and `status`
and `channels[].provider.id` on `support`. It is also half the binding key, so the
identifier the caller discovers is the identifier the adapter routes on — the same
string, not two that have to be kept in step.

**What to send.** `resourceAttributes.@context` dereferences to the pack, by the
same `context.jsonld` → `attributes.yaml` swap the validator uses. The pack is the
complete statement of what every later attribute object may carry — which fields
exist, which are required, which value spaces are closed — so a caller that can
fetch and read it needs nothing else to construct a payload. This is why the packs
exist at all, and discovery is what makes them reachable without a human handing
over a URL.

**Which legs exist.** Covered by `challengeMethods` below.

### The two payloads live in the use-case document

`catalog/publish` for both providers, the `discover` request, and the JSONPath filter
forms are in `grievance-usecase.md` under "Step 0". They are caller-facing, like every
other payload on that page, and this one is not.

What is settled there and assumed here: one catalog per provider, one resource and one
offer in each, the resource and offer ids being the ones the transaction legs already
quote, and the declaration riding in `resources[].resourceAttributes`. Discovery filters
are JSONPath evaluated over the indexed catalog and only `resourceAttributes` is
reachable from an expression, which is what fixes where the two fields below have to sit:
there, or nowhere selectable.

### Deciding the flow: `challengeMethods`

An experience layer has to choose between `init` → `support` → `status` and
`support` → `status` before it sends anything, and nothing published today tells
it which. The difference is the OTP, so the declaration names the mechanisms the
portal issues:

```yaml
challengeMethods:
  type: array
  uniqueItems: true
  items: { type: string, enum: [SMS_OTP] }    # pinned per pack
```

Non-empty means `init` comes first, and the array names the challenge to expect, so
the caller branches on a method rather than assuming six digits — the same rule
`on_init` already states for `challengeIssued.method`. Empty means `support` is
the first call.

A list rather than a boolean, for the reason the `challenge` object already gives:
a portal that adds a second mechanism widens the enum, and a caller reading the
list keeps working. A boolean would say an OTP is needed and leave the caller to
discover which kind by sending one and being refused. The items enum is pinned per
pack to exactly what that portal issues — `[SMS_OTP]` on PMFBY — so the declaration
cannot advertise a mechanism the adapter has no prerequisite hook for.

The field sits in `GrievanceBase` rather than in the PMFBY pack, which is the
opposite of where `challenge` itself sits, and deliberately. `challenge` is a
field only one portal has, so it belongs to that portal's pack. `challengeMethods`
is the question *every* pack has to answer, including by answering "none" — a pack
that cannot state it is a pack a caller cannot plan against. PM-KISAN answers with
an empty list, which is also what marks its catalog entry as a declaration; see
"What the packs must add".

### What a request must carry is not published

The packs state what a *response* must carry — PMFBY's `Direct` gate requires
`case.ticketNo`, `case.status` and `case.filedOn`, PM-KISAN's requires the latter
two. What a *request* must carry is enforced by the mapping guards, which are
adapter-internal. So a caller reading both packs cannot tell that a PMFBY read is
keyed on a ticket number and a phone while a PM-KISAN read is keyed on a
registration number and a date.

An earlier revision published a `caseLookup` enum in the catalog to close that gap.
It was dropped: the value named a strategy rather than the fields, so a caller still
needed a hard-coded table to turn `ByTicketNoAndPhone` into `case.ticketNo` and
`applicantPhone` — the same branch it would write on `@type`, one field further away.
And it answered the question for `status` only, while `support` has the larger gap.

The requirement is documented instead, in the use-case document's "What each call
must carry". Publishing it properly means a per-action annotation on the packs —
`x-oan-required-by-action`, covering every action rather than one — which is listed
in Open and not yet decided.

### Why the catalog entry reuses the transaction `@type`

Because the network has already settled this. `schema/index.md` records that the
schemas formerly named `AgricultureCapability`, `AdvisoryCapability` and
`WeatherAdvisoryCapability` are **retired**, and that a capability declaration is
now the corresponding active pack in `OnDemand` mode. `WeatherAdvisory`'s own
README states it plainly: `OnDemand` is the capability declaration, `Direct` is the
place-specific advisory, and there is no separate `WeatherAdvisoryCapability`
schema.

Grievance follows it exactly. `informationMode: OnDemand` on a catalog entry is not
a stretched reading of "the ask" — a declaration is what a caller may ask for, and
the field already means direction rather than Beckn action. One `@type`, one
`@context`, one `attributes.yaml`: the caller resolves the same pack for discovery
and for every call that follows, which is the property worth protecting. A separate
`PMFBYGrievanceCapability` would double the packs and make the discovery pack and
the transaction pack two things that must be kept in agreement by hand.

One live example contradicts this — `discovery-service/examples/01-publish-weather-advisory.json`
still publishes `@type: openagrinet:WeatherAdvisoryCapability` against a context
URL for a schema that no longer exists. It predates the retirement. Worth fixing
there, and worth not copying here.

### What the packs must add

Four changes, all in the schemas, none in adapter code:

1. **`GrievanceBase`** gains `challengeMethods`, optional, described as
   declaration-only — it appears on a catalog entry and on no transaction payload.
2. **PMFBY** pins `challengeMethods` to `minItems: 1` with `items.enum: [SMS_OTP]`.
3. **PM-KISAN** pins it to `maxItems: 0`: the field is present and empty, which is
   the honest statement that the portal issues no challenge.
4. **Both** gain one branch in the "every payload must be about something" gate:

   ```yaml
   - anyOf:
       - required: [grievance]
       - required: [case]
       - required: [enrolmentId]
       - required: [challengeMethods]   # a catalog entry: declares, asks nothing
       # ... plus challengeIssued and applicantPhone on PMFBY
   ```

   Without it a catalog entry fails validation, because it carries none of the
   things a transaction payload is about. This is why PM-KISAN publishes an empty
   array rather than omitting the field: grievance uses `OnDemand` for both the
   declaration and the ask, unlike `WeatherAdvisory` where `OnDemand` is the
   declaration and nothing else, so the declaration needs a field of its own to be
   told apart from a request.

Each pack also gains one example, `examples/on-demand-capability.json`, holding the
`resourceAttributes` object alone. Pack examples are bare attribute objects, which is
why the `catalog/publish` envelope around it is not one: the envelope is a payload and
lives in the use-case document, the declaration inside it is validated by the pack like
every other example.

### Where it binds

`resources[].resourceAttributes.@type` is the adapter's own default binding path —
`common.BecknV2` points at it, and it is the one the domain capabilities use
unchanged. The grievance transaction legs are the exception, overriding
`capabilityCodeAt` to reach `commitmentAttributes` and `channels`. So the catalog
leg is the single grievance payload that needs no override: a declaration really is
a resource, which is what the default assumes, and only a lodged grievance is not.

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
providerIdAt:     message.support.channels[].provider.id
capabilityCodeAt: message.support.channels[].@type
```

`Support` itself is sealed at three fields and none of them names a participant,
but `channels` is an array of `Attributes`, which is the spec's extensibility
container and is `additionalProperties: true`. The pack puts `provider` there,
a Beckn `Provider` narrowed to a reference. The path then ends in `.provider.id`
exactly as the Beckn v2 default does — `BindingPaths` walks dotted segments past
a `[]`, so this is the default's own grammar pointed at a different container,
not a special case. `scheme.code` is not a substitute: it names a scheme rather
than a participant, and it reads `PMFBY` against a registry holding `pmfby`.

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
from the contract on `init` and `status`, and from the channel's `provider.id` on
`support`. Either way the key comes out the same, so all three legs resolve to the same
registry record and the same credential profile.

| | PMFBY | PM-KISAN |
|---|---|---|
| Pack | `api-schemas/PMFBYGrievance/v0.1` | `api-schemas/PMKISANGrievance/v0.1` |
| `@type` | `openagrinet:PMFBYGrievance` | `openagrinet:PMKISANGrievance` |
| `provider.id` (on `support` only) | `pmfby` | `pmkisan` |
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

Neither pack composes `AgricultureResourceFields`: that field set is framed around a
Resource that holds information, which a grievance is not. `subjectCategories` is gone
with it, being a discovery category for the catalog resource rather than anything a
per-case payload carries.

What they compose instead is **`GrievanceBase`**, in
`api-schemas/Grievance/v0.1/attributes.yaml`. It owns everything both packs share, in four
bands — `informationMode`, `provider`, `scheme` and `enrolmentId` at the top; a `grievance`
object holding `category`, `subCategory` and `description`; a `case` object holding
`ticketNo`, `status`, `filedOn`, `remark` and `remarkedOn` — together with `CaseStatusCode`,
`CalendarDate` and `ProviderReference`. Each pack `allOf`-references it, pins its own
`@type` and `scheme.code`, and narrows or refuses what its portal does not have. The
case-status vocabulary is therefore one list, not two kept in step by review.

The band a field sits in says who wrote it: the caller at the top, the farmer under
`grievance`, the portal under `case`. That is the axis, not mutability — `case.filedOn` is
a date the portal stamps, so it sits with the portal's other fields even though it never
changes.

PM-KISAN adds no field of its own; it only narrows, refuses and annotates. That is the
measure of whether the base is drawn right.

`Grievance/v0.1` is a file, not a pack: no `profile.json`, never indexed, and no payload
ever declares `@type: openagrinet:GrievanceBase`. The one field that stays out of it is
`@type`, which is the pack's identity. `grievance.category` is in the base but carries no
IRI there — each pack binds it to its own, over value spaces that do not overlap.

Shape rules that hold in both directions live in the base or the pack.

Per-action requirements do not, and cannot. One `@type` covers every action, so
"`enrolmentId` is required on `support` but not on `init`" is not expressible in a pack.

**Direction is enforced, and it is enforced with `anyOf`.** Each pack carries two top-level
`anyOf` gates: a `Direct` payload must carry a `case` band with a status and a filing date,
and every payload must carry at least one of the things it could be about. Both were tested
against the real validator.

`if/then` would be the natural way to write the first gate and it does not work. The
extended-schema validator parses `if`/`then` and never evaluates it, so a schema saying
*"if `informationMode` is `Direct` then `case` is required"* accepts a `Direct` payload with
no `case` at all. `allOf`, `anyOf` and `not` are evaluated, including nested under
`properties`, which is what the gates and the per-pack refusals are built from. Two earlier
guards in these packs were written as `if/then` and enforced nothing.

A gate has to live in the pack rather than the base. A pack cannot widen an inherited
`anyOf` — `allOf` means both must hold — and PMFBY needs a branch the base cannot know
about, for the challenge acknowledgement that carries nothing but `challengeIssued`.

**What remains per-action lives in the mapping guards**, as it always would have.

### The published pack is what gets fetched

The validator resolves the payload's `@context` by string-swapping `context.jsonld` for
`attributes.yaml` (`schemav2validator/extended_schema.go:545`) and fetching it, so what
matters is the live file, not the working copy. **Both packs and
`api-schemas/Grievance/v0.1/attributes.yaml` must be published to
`openagrinet.github.io` before any of this runs**, and `openagrinet.github.io` must appear
in `extendedSchema_allowedDomains` — it was added to `provider-adapter.yaml` for exactly
this. A pack the validator cannot fetch fails every payload, and so does a pack whose
base it cannot fetch: the `$ref` to `GrievanceBase` is relative, so it resolves against
the same host and is subject to the same allow-list.

### `@type` must be a string, not an array

Every pack permits the array form — canonical type plus provider-defined ones — but the
adapter reads the binding key through `ValuesAt`, which keeps only `leaf.(string)`
(`internal/common/paths.go:65`), so an array `@type` produces no binding key and the
request 404s. Filed as a separate bug; grievance payloads use the string form.

### What each pack declares

**PMFBY** — `applicantPhone` is an Indian mobile series, `season` is one of three names,
`cropYear` is four digits, and `grievance.category.code` and `grievance.subCategory.code`
are each digits, held as two fields rather than one joined string. `challenge` is declared
in the pack itself — `method: SMS_OTP`, a six-digit `value`, nothing else accepted. Its
`Direct` gate requires `case.ticketNo`, `case.status` and `case.filedOn`, and it refuses
`case.remarkedOn`, which PMFBY does not publish.

**PM-KISAN** — narrowings only. Its `Direct` gate requires `case.status` and
`case.filedOn`, and not `case.ticketNo`. The field is allowed and is populated on a lodge,
but the status call is not documented to repeat the handle per record, so a case read may
carry none and the gate must not demand one. `grievance.subCategory` is refused: PM-KISAN
classifies one level deep.
Its categories are a real, closed, ten-value vocabulary, so they are an enum rather than a
guard.

The JSON path is `grievance.category` in both packs, but it resolves to
`openagrinet:pmfbyGrievanceCategory` in one and `openagrinet:pmkisanGrievanceCategory` in
the other. The two schemes publish incompatible value spaces — bare digits against that
closed list — so one IRI could not hold both.

**Neither `writeOnly` nor `readOnly` appears in either pack.** The validator visits every
payload with `VisitAsRequest`, on the way out as well as in, so `writeOnly` would assert
nothing and `readOnly` would reject the very response it describes — a `readOnly`
`challengeIssued` makes the challenge acknowledgement unvalidatable. Direction is carried by
`x-oan-pii` handling instead.

### The challenge is PMFBY's own

`challenge` and `challengeIssued` are declared inline in
`api-schemas/PMFBYGrievance/v0.1/attributes.yaml`. Neither is defined in a domain schema,
and neither is shared with another pack:

```yaml
challenge:
  type: object
  required: [method, value]
  additionalProperties: false
  x-oan-pii:
    class: credential
    handling: [no-log, no-trace, no-echo, no-forward]
  properties:
    method: { type: string, enum: [SMS_OTP] }
    value:  { type: string, pattern: "^[0-9]{6}$" }
```

Three things follow.

**The pack advertises exactly what the Provider accepts.** `SMS_OTP` and nothing else —
it does not claim to take a device token it has no prerequisite for. A second mechanism is
paid for by the pack that wants it: widen the `enum` and pin the new format in the same
edit.

**Nothing travels that the adapter would ignore.** `additionalProperties: false` refuses
every key not named here, which is both stricter and shorter than refusing them one at a
time. An earlier draft composed a network-wide `Challenge` that carried a `txnId` for
mechanisms whose upstream issues a correlator, and spent a `not: { required: [txnId] }` to
refuse it. PMFBY binds the challenge to the phone number alone, so the field is now simply
never defined.

**The format rule is genuinely enforced.** A single `Challenge` with `if method = X then
value matches Y` would validate nothing: the extended-schema validator parses `if/then`
and never evaluates it, which is the same limitation [`What a pack
enforces`](#what-a-pack-enforces-and-what-it-cannot) records. A `pattern` on a directly
declared property has no such problem, and the six-digit rule is as enforced as it was
when the field was a bare `otp`.

It stays in the pack rather than moving into `GrievanceBase` for the plainest of
reasons: PM-KISAN issues no challenge at all, so there is no second consumer. The base
holds what both packs have; a field only one portal has belongs to that pack. The seam is
then: the pack owns the shape and the format, the mapping guard owns which action must
carry one.

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
  { "mobile": $ca.applicantPhone, "otpType": "SMS" }
)
```

```json
{ "mobile": "9876543210", "otpType": "SMS" }
```

This leg is on the **PMFBY core realm**, `POST /api/v1/services/nic/getOtp`, not on FGMS.
PMFBY publishes no grievance-specific OTP endpoint; the grievance flow reuses the policy
flow's pair. The two realms log in separately and their tokens are not interchangeable.

#### Provider response

```json
{ "status": true, "data": "OTP sent to registered mobile", "error": "" }
```

`status` is a boolean, not the string `"success"`, and `data` is either the message as a
bare string or an object with a `message` in it — both shapes occur. `status: false` puts
the reason in `error`.

**The portal does not say when the OTP expires.** There is no `valid_for` and no
equivalent. `challengeIssued.expiresAt` is therefore the adapter's own assertion, computed
from a configured TTL, and the mapping below says so.

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
    "expiresAt": $fromMillis($toMillis($now()) + $number($env.PMFBY_OTP_TTL_SECONDS) * 1000)
  }
)
```

`expiresAt` is ours, not the portal's — see the note on the response above. The pack
requires the field, so the adapter states a value rather than omitting it; `PMFBY_OTP_TTL_SECONDS`
is a provider constant to be set from whatever PMFBY confirms its window is. Until then it
is a stated expectation, and a caller must still be ready for the portal to reject an OTP
it considers stale earlier than that.

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
`message.support`: the enrolment identifier from `orderId`, and everything else from the
one channel the pack validates.

```yaml
required:
  - check: |
      ($s := beckn.message.support; $ch := $s.channels[0];
       $exists($s.orderId) and $exists($ch.cropYear) and $exists($ch.season))
    message: "lodging a PMFBY grievance needs the application number, crop year and season"
  - check: |
      ($ch := beckn.message.support.channels[0];
       $exists($ch.grievance.category.code) and $exists($ch.grievance.subCategory.code))
    message: "lodging a PMFBY grievance needs both category levels"
  - check: |
      ($c := beckn.message.support.channels[0].challenge;
       $c.method = "SMS_OTP" and $exists($c.value))
    message: "support carries an SMS OTP challenge"
```

These guards enforce what the pack deliberately cannot: which fields a *particular* action
must carry. The pack checks shape — the season enum, the phone pattern, the digits in each
category code — on whatever payload arrives. What it cannot say is that `support`
specifically needs a crop year, because `init` cannot have one. The guards live in
`grievance.support.yaml`, so they run only on `support`.

They are shorter than they were. The complaint now travels inside `channels[0]`, which the
pack validates, so the category pattern and the presence of the description are checked by
the schema on every leg and the guard only has to say *both levels are present on this
one*. Only `orderId` still sits outside the channel, unseen by the validator — and the
first guard covers it.

No pattern is restated here. A guard that repeats a rule the pack already holds is a second
copy to keep in step; the one the old version carried
(`descriptor.code must be <category>.<subCategory>`) is gone with the dotted code itself.

No guard checks `provider`, and none should: the binding key is built before the mapper
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
  $g   := $ch.grievance;
  $seasons := { "Kharif": "1", "Rabi": "2", "Zaid": "3" };
  {
    "requestorMobileNo":     $ch.applicantPhone,
    "applicationNo":         $s.orderId,
    "requestYear":           $ch.cropYear,
    "requestSeason":         $lookup($seasons, $ch.season),
    "ticketCategoryID":      $g.category.code,
    "ticketSubCategoryID":   $g.subCategory.code,
    "grievenceDescription":  $trim($g.description),
    "complaintDate":         $ch.complaintDate ? $ch.complaintDate
                               : $fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530"),
    "receiptSourceID":       $ch.receiptSourceId ? $ch.receiptSourceId : "134306"
  }
)
```

```json
{
  "requestorMobileNo": "9876543210",
  "applicationNo": "KA2026KH00123456",
  "requestYear": "2026",
  "requestSeason": "1",
  "ticketCategoryID": "3",
  "ticketSubCategoryID": "10",
  "grievenceDescription": "Cannot log in to the PMFBY portal to view my Kharif 2026 enrolment.",
  "complaintDate": "2026-09-28",
  "receiptSourceID": "134306"
}
```

These are the portal's own key names, `POST /krphapi/FGMS/AddKRPHNCIPGrievenceSupportTicket`.
Two of them are misspelled upstream -- `grievenceDescription`, and `Grievence` throughout
the path. The typos are preserved in the mapping and corrected at the network boundary.

#### Provider response

```json
{
  "responseCode": "1",
  "responseMessage": "Grievance registered successfully",
  "recordCount": 1,
  "responseDynamic": {
    "GrievenceSupportTicketNo": "100626000099001",
    "GrievenceSupportTicketID": 1
  }
}
```

Every FGMS reply wears this envelope: `responseCode`, `responseMessage`, `recordCount`,
`responseDynamic`. Success is `responseCode` equal to `"1"` — and it arrives as a string on
this leg and has been seen as a number on others, so the test is on the stringified value,
never on `===  1`. `responseMessage` is the portal's own prose and is never returned.

#### Mapping → Beckn (`on_support`)

There is no commitment on this leg and no offer or resource either — the reply is a
`Support` object. `orderId` carries the application number back unchanged, the `grievance`
band is the caller's own words echoed back, and `channels[0]` is the case record, its
`informationMode` flipped to `Direct` because it now carries a real case rather than a
request for one. The ticket the portal just issued is in that case record, not in `orderId`:
`orderId` means the thing support is required against, and that is the application both
before and after the call. From here on `case.ticketNo` identifies the case, and a later `status`
re-enters through a contract whose resource is the thin catalog pointer
`res:pmfby:grievance`.

The lodge reply carries two usable values inside the envelope. Every
field in `on_support` comes from one of four places:

- **Read from the provider** — `case.ticketNo`, from
  `responseDynamic.GrievenceSupportTicketNo`, and nothing else.
- **Stated by the mapping** — `case.status.code` is `Registered`, derived from
  `responseCode` being `"1"` (the portal has no status field on this leg; a freshly lodged
  grievance is registered by definition). No `name` accompanies it: the portal said
  nothing, and an absent `name` is how a caller tells an inferred status from a quoted
  one. `case.filedOn` restates the `complaintDate` the request just generated, not a value
  the portal echoed — an IST calendar date. `provider` names the portal the adapter routed
  to, taken from the registry entry.
- **Changed** — `informationMode`, `OnDemand` → `Direct`.
- **Echoed from the request** — `scheme` and the `grievance` band.

`GrievenceSupportTicketNo` maps to `case.ticketNo` and nowhere else. The spec says the provider
returns "the ticket reference" without naming the field that holds it, and `orderId` is the
obvious candidate only if you read it as a general-purpose reference slot — but it is
defined as "the order against which support is required", and the ticket is not that. The
`case` band is where the portal's own record lives, `ticketNo` is already one of its
fields, and it carries the same value on `on_status`, so one field means one thing on both
legs.
`GrievenceSupportTicketID` is dropped. It is the portal's own row id rather than the number
the farmer quotes, and no later call takes it — the read is keyed on
`GrievenceSupportTicketNo`. An internal key published to a network with no consumer. The
rule this pack follows is: map what the portal sends, or say in the README why not. The
README says why not. `recordCount` and `responseMessage` are dropped on the same ground —
one is a count of a single record, the other is the portal's own text. `applicantPhone` and `challenge` are not echoed:
neither belongs in a response.

`enrolmentId` **is** echoed here, in `orderId`, unchanged from the ask. It tells the caller
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
       $exists($ca.case.ticketNo) and $exists($ca.applicantPhone))
    message: "checking a PMFBY grievance needs the ticket number and the filing phone number"
```

#### Mapping → provider

```jsonata
(
  $ca := beckn.message.contract.commitments[0].commitmentAttributes;
  {
    "requestorMobileNo":        $ca.applicantPhone,
    "GrievenceSupportTicketNo": $ca.case.ticketNo
  }
)
```

The request body is the portal's, `POST /krphapi/FGMS/GetGrievenceTicketsStatus`. Note the
casing: `requestorMobileNo` is lower-cased, `GrievenceSupportTicketNo` is not. The upstream
misspells "Grievance" throughout; the typo is preserved in the mapping and corrected at the
network boundary.

#### Provider response

**Verified.** The names below are read from the v1 adapter on `Beckn`'s `main` branch —
the twelve `responseDynamic` fields from the mapping in `src/app.service.ts`, the envelope
from `src/services/pmfby/pmfby-greviance.service.ts`. See
`grievance-upstream-contracts.md` §0 for the line references; where it and this page
disagree, it wins.

```json
{
  "responseCode": "1",
  "responseMessage": "Fetched successfully",
  "recordCount": 1,
  "responseDynamic": {
    "GrievenceSupportTicketID": 109301,
    "TicketStatus": "Open",
    "ComplaintDate": "2026-09-28",
    "ApplicationNo": "KA2026KH00123456",
    "GrievenceDescription": "Cannot log in to the PMFBY portal to view my Kharif 2026 enrolment.",
    "TicketCategoryName": "Enrollment / Portal Issues",
    "TicketSubCategoryName": "Login",
    "CropName": "Paddy",

    "FarmerName": "…", "InsuranceCompany": "…",
    "StateMasterName": "…", "DistrictMasterName": "…"
  }
}
```

Four things about this reply, each of which changed the pack.

1. **No category ids.** The portal answers with `TicketCategoryName` and
   `TicketSubCategoryName` and no id beside either. The base pack therefore requires
   `code` **or** `name` on a classification rather than `code` outright — a response
   carrying what the portal actually sent could not satisfy a `code` requirement.
2. **No remark, and no remark date.** Nothing in the record resembles a reply from the
   portal. `latestRemark` was our own invention. The PMFBY pack refuses both
   `case.remark` and `case.remarkedOn` rather than leaving them permanently absent.
3. **No `requestYear` or `requestSeason` comes back.** They go up on the lodge and are
   not returned, so `cropYear` and `season` are absent from every response.
4. **The ticket number is not returned.** The read answers with
   `GrievenceSupportTicketID`, an internal key that is not mapped. On a read the adapter
   echoes the number the caller asked with.

On the envelope: success is `responseCode` equal to `"1"`, not an HTTP status; it has been
seen as both a string and a number, so test the stringified value. `recordCount` sits
beside `responseDynamic`, which v1 reads as a single object. **Whether a multi-ticket read
returns an array is unconfirmed** — the mapping must not gate on `recordCount`; read
`responseDynamic` and test that directly. See Open.

#### Mapping → Beckn (`on_status`)

`case.status` splits in two: `name` carries `TicketStatus` verbatim, and `code` is the
`CaseStatusCode` from `GrievanceBase` that the adapter maps that phrase to.

`ComplaintDate` becomes `case.filedOn`. The portal does not publish a format for it, so
the adapter normalises to an ISO calendar date rather than passing it through — unlike
`support`, where the adapter generates the date itself and the question does not arise.

The mapping is a lookup, not a transformation. A phrase the lookup does not hold falls to
`UnderReview` — never to a terminal code, which must come from the portal — and the phrase
still reaches the caller in `name`. So an unfamiliar status is rendered correctly and is
never mistaken for a finished case.

The response returns `ApplicationNo` and `GrievenceDescription`, which map straight
through to `enrolmentId` and `grievance.description`. It does **not** return the ticket
number, so `case.ticketNo` is echoed from the request — the one field on this leg the
portal does not supply.

`CropName` becomes `case.cropName`, a field this pack adds rather than inherits. PMFBY
tickets carry a crop because the scheme insures one; a grievance system that is not crop
insurance has nothing to put there. It sits in `case` because the portal authors it.

The two category levels stay two fields, and on this leg both arrive as names only:

| upstream | Beckn |
|---|---|
| `TicketCategoryName` | `grievance.category.name` — no id accompanies it |
| `TicketSubCategoryName` | `grievance.subCategory.name` — likewise |

The ids go up on the lodge, as `ticketCategoryID` and `ticketSubCategoryID`, and do not
come back. So the request carries `code` and the response carries `name`, which is why the
base pack requires one of the two rather than `code` outright.

An earlier draft joined the two names with a slash and the two codes with a dot. The join
was lossy and could not be undone: this portal's own category name contains the separator —
`Enrollment / Portal Issues` beside `Login` — so no split on ` / ` recovers the original
pair. Keeping them apart removes the split from both directions.

**There is no remark.** The record carries a status phrase and nothing resembling a reply
from the portal, and no date against one. The pack refuses `case.remark` and
`case.remarkedOn` outright rather than leaving two inherited fields permanently absent —
two members to delete if PMFBY ever starts publishing them.

`applicantPhone` is deliberately not echoed. The read does not return it anyway; the
request is keyed on it.

**The response mapping is an allow-list.** The record also carries the farmer's name, the
insurer, and the state and district. None of it is mapped, logged or traced. A passthrough
here would repeat the v1 `identity-no` echo at far greater scale. The table at the end of
this section lists every dropped field and why.

The commitment stays `ACTIVE` while the case is open and should become `CLOSED` on a
terminal status — but which `TicketStatus` values are terminal is not yet known, so
today everything maps to `ACTIVE`. See Open.

### Field mapping

Upstream names are the portal's own, verified against the v1 adapter on `main`.

| Beckn (`commitmentAttributes`) | Up to the provider | Back from the provider |
|---|---|---|
| `applicantPhone` | `mobile` on the OTP pair; `requestorMobileNo` on both FGMS calls | not returned, and never echoed |
| `enrolmentId` | `applicationNo` on the lodge; also carried in `support.orderId` | `ApplicationNo` on the case read |
| `cropYear` | `requestYear` on the lodge | **not returned** — absent from every response |
| `season` | `requestSeason`; the name is mapped to the portal's code: Kharif 1, Rabi 2, Zaid 3 | **not returned** |
| `grievance.category` | `ticketCategoryID` ← `.code` | `TicketCategoryName` → `.name`, with no id beside it |
| `grievance.subCategory` | `ticketSubCategoryID` ← `.code` | `TicketSubCategoryName` → `.name`. Two fields, not one joined string — see the note on the lossy join |
| `grievance.description` | `grievenceDescription` (the portal's spelling) | `GrievenceDescription` |
| `case.ticketNo` | `GrievenceSupportTicketNo` as the `status` query key | `responseDynamic.GrievenceSupportTicketNo` on the **lodge**. The read does not return it, so there the adapter echoes the number the caller asked with |
| `case.status` | not sent | `TicketStatus` verbatim into `name`; `code` is the `CaseStatusCode` it maps to, falling back to `UnderReview` |
| `case.cropName` | not sent | `CropName`. Added by this pack, not inherited |
| `case.filedOn` | not sent; on `support` it restates the generated `complaintDate` | `ComplaintDate`, format unpublished, so the adapter normalises to an ISO calendar date |
| `challenge` | `value` → `otp` on `verifyMobile` only, selected by `method`; never on the lodge call | never in a response. Not marked `writeOnly` — the validator would not act on it |
| `challengeIssued` | not sent | `on_init` only — `method` is the constant `SMS_OTP`, `sentTo` is the masked request phone, `expiresAt` is a configured TTL because **the portal publishes no expiry**. All three are required; `txnId` is refused |
| `informationMode` | not sent | `OnDemand` on requests, `Direct` on responses that carry a real case |
| `scheme` | not sent | echoed unchanged, it identifies the scheme |
| `provider` | not sent | the registry entry the adapter routed to. Returned on `on_support` only -- on a contract leg `commitments[].offer.provider` carries it |
| — | `complaintDate` = `$fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530")`, generated in IST — the farmer does not backdate | |
| `complaintDate`, `receiptSourceId` | sent as `complaintDate` and `receiptSourceID`. Both optional: the date falls back to the current IST date, the source id to the adapter's configured channel id (`134306` for Vistaar) | not returned |
| — | `otpType` = `SMS` | |

Refused by the pack rather than mapped: **`case.remark` and `case.remarkedOn`**. PMFBY
publishes neither.

Returned by the portal and deliberately not mapped:

| dropped | why |
|---|---|
| `responseCode` | consumed — drives the ACK/NACK decision and `case.status` on the lodge; not surfaced as a field |
| `responseMessage` | the portal's own text. Logged redacted, never returned: it may hold a stack trace, an internal hostname or a quoted-back credential |
| `recordCount` | a count of a single record, and v1 has seen it disagree with the body |
| `GrievenceSupportTicketID` | the portal's row id. Not the number the farmer quotes, and no call takes it |
| `FarmerName`, `InsuranceCompany`, `StateMasterName`, `DistrictMasterName` | **personal data.** Never mapped, logged or traced |

The policy and claim reads on the same portal return more of the same kind —
`RequestorMobileNo`, `Email`, `SubDistrictName`, `GramPanchayat`, `NyayPanchayat`,
`VillageName`, `InsurancePolicyNo`. None of it is in scope here, and none of it would be
mapped if it were. **The response mapping is an allow-list, not a passthrough.**

## PM-KISAN: mappings and guards

### 1. `support` — lodge the grievance

#### What the request carries

`support.orderId` carries the farmer's PM-KISAN registration number -- that is where
`enrolmentId` lands on this leg -- and the channel carries the scheme and the
`grievance` band. The
registration number is both the identity the portal authenticates on and the enrolment the
complaint is against — the upstream takes one reference, `IdentityNo`, and nothing else in
its API names a case — so it fills the same field PMFBY fills with the application number.

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
      ($exists(beckn.message.support.channels[0].grievance.category.code))
    message: "lodging a PM-KISAN grievance needs a category"
```

Two guards, where there were three. The category vocabulary and the ten-character minimum
on the description are both in the pack now, checked on `channels[0]` on every leg, so the
guard only has to say that a category is present on *this* action — the pack cannot,
because a case read legitimately carries none.

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
    "GrievanceType":        $s.channels[0].grievance.category.code,
    "GrievanceDescription": $trim($s.channels[0].grievance.description)
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
{ "Responce": "True", "GrievanceID": "PMK2026091234", "message": "Grievance registered successfully" }
```

**Three fields, and every one of them arrives under more than one name.** The portal is
inconsistent about casing and spelling, so the mapping reads each through a fallback
chain — this is the v1 adapter's behaviour on `main`,
`src/services/pmkisan-grievance/pmkisan-grievance.service.ts`, and it is reproduced here
because it is the portal that is inconsistent, not the client:

| value | names seen |
|---|---|
| the case identifier | `GrievanceID`, `grievanceId`, `GrievanceNo` |
| the sentinel | `Status`, `Responce`, `Rsponce` |
| the prose | `Message`, `message`, `Remark` |

```jsonata
$ident := response.GrievanceID ? response.GrievanceID
          : response.grievanceId ? response.grievanceId : response.GrievanceNo;
```

The sentinel's value is the **string** `"True"` or `"False"`, not a boolean, and the
mapping compares strings. **It is also optional here.** The direct client models it as
`Responce: Optional[str]` and treats an absent field as success, noting that the response
"typically carries `message` and sometimes `Responce`". So the mapping must test for
failure, not for success: `"False"` is the error path, and anything else — `"True"` or
nothing at all — is a registered grievance. A mapping written the other way round would
`502` every successful lodge on which the portal omitted the field.
`/GrievanceStatusCheck` is different: the sentinel is always present there and is tested
directly.

#### Mapping → Beckn (`on_support`)

**The portal does issue a case identifier.** It arrives as `GrievanceID`, or `GrievanceNo`
where that is absent, and it becomes `case.ticketNo`. An earlier revision of this page said
PM-KISAN issued nothing of the kind; that reading came from the direct client, whose
Pydantic model simply does not declare the field, so it was dropped before anyone saw it.

What the identifier does **not** buy is a read. `/GrievanceStatusCheck` returns every
grievance on the identity and is not documented to repeat the handle per record, so the
case read below still matches on `case.filedOn`. The ticket number is a handle for the
farmer to quote, not a key the portal accepts.

`orderId` still comes back as it went up, the registration number unchanged, exactly as on
PMFBY — the ticket rides in the `case` band on both providers. Provenance of each field in
`on_support`:

- **Read from the provider** — `case.ticketNo`, and the success/failure decision.
- **Stated by the mapping** — `case.status.code` is `Registered`, asserted whenever
  the sentinel is not `"False"`, with no `name`: the portal said nothing, and the absent
  `name` is how a caller tells an inferred status from a quoted one. `case.filedOn` is
  `$fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530")`, generated in IST,
  because the portal returns no date. `provider` names the portal the adapter routed to,
  taken from the registry entry.
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
identity is split: `case.ticketNo` is the portal's handle and `contract.id` is the
caller's, but neither is a read key. `filedOn` is what a later `status` uses to find the
grievance at the portal. That is where PM-KISAN stays weaker than PMFBY — not for want of
an identifier, but because the portal accepts none on a read. See `status` below.

**`case.filedOn` is shifted to IST before truncating**, which is why the mapping above does not
simply truncate `$now()`. Because `status` matches on it, a day's drift would not merely
mislabel the grievance — it would fail to find it. See Open.

The caller should tell the farmer to keep their registration number, because that — not
anything in this response — is what retrieves the grievance later.

### 2. `status` — read the replies

#### What the request carries

**`case.filedOn` is the discriminator, and it is why the request carries three fields rather
than two.** The portal has no per-grievance endpoint: `/GrievanceStatusCheck` takes an
identity and returns *every* grievance on it. But `/on_status` answers for **one**
contract, so the adapter has to select the record this contract is about, and the adapter
is stateless — it stored nothing at `support`. The caller did: `on_support` returned
`filedOn`, and the caller sends it back here. The portal's `GrievanceDate` is then matched
against it.

That is the honest limit of this design. Two grievances filed on the same identity on the
same day are indistinguishable, because the portal returns nothing else stable to key on.
The portal does issue a grievance id on the lodge, but it accepts none on a read and is
not documented to repeat it per record — so the id cannot be the discriminator until
PM-KISAN changes one of those two things. See Open.

#### Guards

The identity guard, restated in `grievance.status.yaml` against the contract this action
does carry — the registration number must be non-empty alphanumeric, read from
`commitmentAttributes.enrolmentId` rather than from `orderId`, because a `Contract` has no
`orderId` — plus one this action needs on its own: `case.filedOn` must be present and ISO, since
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
    "IdentityNo": $ca.enrolmentId
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
      "GrievanceStatus": "Disposed",
      "OfficerReply": "Bank account seeded with Aadhaar; payment in next cycle.",
      "OfficeReplyDate": "06-10-2026"
    },
    {
      "Reg_No": "UP12345678A",
      "GrievanceDate": "14-03-2026",
      "GrievanceDescription": "Name spelling incorrect.",
      "GrievanceStatus": "Pending",
      "OfficerReply": null,
      "OfficeReplyDate": null
    }
  ]
}
```

#### Mapping → Beckn (`on_status`)

**`details` is a list; `on_status` returns the one record that matches.** The mapping
converts each `GrievanceDate` from `dd-MM-yyyy` to ISO and keeps the record whose date
equals the request's `case.filedOn`. The rest are discarded — they belong to other contracts.
The legacy client rendered `details[0]` and discarded the rest too, but that was a display
shortcut that happened to land on the newest record; this is a match, not a guess.

```jsonata
$ca.filedOn = $fromMillis($toMillis($.GrievanceDate, "[D01]-[M01]-[Y0001]"),
                          "[Y0001]-[M01]-[D01]")
```

`case.status` carries the portal's `GrievanceStatus` verbatim in `name`, with `code` the
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

`case.remark` and `case.remarkedOn` are omitted when null rather than emitted as `null` —
an absent field reads as "nothing recorded yet," which is what it means. `case.remark` is
capped at 2000 characters by the pack, being unvalidated upstream free text.

### Field mapping

| Beckn (`commitmentAttributes`) | Provider |
|---|---|
| `enrolmentId` | `IdentityNo` — the registration number, sent as it arrived. It rides in `support.orderId` on the lodge leg and in `commitmentAttributes` on the read |
| action | selects the `Type` suffix: `_Details` on `support`, `_Status` on `status` |
| `grievance.category.code` | `GrievanceType` (`G001`–`G010`, verbatim) — **outbound only**. `Reg_No_Status` returns no category, so a case read cannot populate it and the field is absent from a PM-KISAN `on_status`. That is why the base requires only `description` of a `grievance`. PMFBY round-trips its category; this one does not |
| `grievance.subCategory` | refused by the pack — PM-KISAN classifies one level deep |
| `grievance.description` | `GrievanceDescription` out; `GrievanceDescription` in on `status` |
| `case.status` | `code` only: `Registered` on `support` unless `Responce` is `"False"`; on `status`, `GrievanceStatus` into `name` with the mapped `CaseStatusCode` in `code`, falling back to `Replied`/`UnderReview` from `OfficerReply` when the portal publishes no status |
| `case.filedOn` | sent on `status` and matched against `GrievanceDate`; generated on `support`, which returns no date |
| `case.remark`, `case.remarkedOn` | `OfficerReply`, `OfficeReplyDate` — omitted when null. The network does not adopt the portal's field name; the portal itself is inconsistent about the author (`OfficeReplyDate`, not `OfficerReplyDate`) |
| `case.ticketNo` | `GrievanceID`, or `GrievanceNo` where that is absent — the portal uses both names for one handle. Returned on a lodge; the status call is not documented to repeat it per record, so a case read may carry none. Never sent: the portal accepts no ticket number on a read |
| `informationMode` | not sent; `OnDemand` on requests, `Direct` on responses |
| `scheme` | not sent; echoed unchanged |
| `provider` | not sent; the registry entry the adapter routed to. Returned on `on_support` only -- on a contract leg `commitments[].offer.provider` carries it |
| — | `TokenNo` = the portal's static service token, from the prerequisite |
| consumed | `Responce` — drives `case.status`, the empty-result path, and the error path |
| dropped | `Reg_No` — returned on every `status` record and discarded; it appears in no field and in no id |
| dropped (PII) | `Farmer_Name`, `Father_Name`, `Gender`, `MobileNo`, `StateName`, `DistrictName`, `BlockName`, `RevenueVillageName` — returned on every `status` record; never mapped, logged or traced |
| dropped | `message`, `__type` — portal prose and envelope chatter |
| echoed once | `enrolmentId` — returned in `orderId` on `on_support` to the caller who sent it, and nowhere else; never logged, never traced, never surfaced on a case read |

## When it fails

The status codes and the NACK body are in the Errors section. What produces them:

**Guards run before any upstream call**, so a malformed payload never reaches the portal.

**An empty answer is not an error, but it is still said out loud.** An unknown ticket, or no
PM-KISAN record matching `case.filedOn`, returns `202` with an `AckNoCallback` body carrying
`BIZ_NO_RESULTS_FOUND` — `status: "ACK"`, because the request was accepted and processed.
This needs no schema change: no field of either pack is involved.

Returning a commitment with `commitmentAttributes` omitted would also be spec-legal, and is
what an earlier draft did. It was dropped because a missing field is not a message — a
consumer cannot distinguish it from an adapter bug that lost the field.

**The portals signal failure in the body, not the status line.** PMFBY's FGMS calls send
`responseCode` other than `"1"` — test the stringified value, it has been seen as both a
string and a number — and its OTP pair sends `status: false` with the reason in `error`.
PM-KISAN sends its sentinel under `Status`, `Responce` or `Rsponce`, spellings included,
and `"False"` is the refusal; on `/LodgeGrievance` an *absent* sentinel is success rather
than failure. All of these become `502` / `NET_DOWNSTREAM_UNAVAILABLE`.

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

No error path carries `challenge.value`, `enrolmentId`, or the service token.

## What gets added

```
network-specs/api-schemas/Grievance/v0.1/                + challengeMethods
network-specs/api-schemas/PMFBYGrievance/v0.1/           (published; must be live first)
network-specs/api-schemas/PMKISANGrievance/v0.1/         (published; must be live first)
  each pack: a pin for challengeMethods, one anyOf branch,
             one examples/on-demand-capability.json

catalogs: 2 × catalog/publish, submitted once to the discovery service
          (documents, not config -- the publisher takes them over HTTP)

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

**Discovery adds no adapter code either.** The two catalogs are documents submitted to
the discovery service, the declaration they carry is validated by the packs like any
other payload, and the binding path a catalog entry uses is the adapter default that
already exists. Nothing in `pkg/plugin/implementation/Grievance/` answers a `discover`.

## Open

### Blocking

- **PMFBY `init` has no guard.** `grievance.init.yaml` carries no `required:` block, and
  the pack's top-level `anyOf` is satisfied by `enrolmentId` alone. A payload with an
  enrolment and no `applicantPhone` therefore passes validation and the mapping emits
  `{"mobile": null, "otpType": "SMS"}` to the portal. Add the one check —
  `$exists($ca.applicantPhone)` — before `init` is wired. The use-case document already
  lists `applicantPhone` as required on `init`.

- **Is PMFBY's `responseDynamic` an object or an array?** This is what is left of the
  case-read question, and it is the only open item on the PMFBY mapping. The reply carries
  a `recordCount` beside the payload, and v1 reads the payload as a single object while
  also dumping it to a string as a hedge — so a multi-ticket read may well return an array
  and nobody has checked. The mapping below assumes an object. Settle it by observation:
  the telemetry store keeps raw upstream bodies in
  `beckn_ext_events.ext_api_response`, filtered on `service_name = 'pmfby-greviance'`.

  The rest of the case-read contract is **closed**. The twelve `responseDynamic` fields,
  the three FGMS endpoints, the bare-`Authorization` token and the absence of a
  grievance-specific OTP are all read from the v1 adapter on `Beckn`'s `main` branch.
  `grievance-upstream-contracts.md` §0 has the line references. An earlier revision of
  this page recorded these names as unverifiable; that search had covered only the
  checked-out working tree.

- **The adapter's own network identity is not fixed.** Every catalog names the adapter in
  `senderId`, and a caller resolves its address from the registry under that id. The
  payloads in the use-case document use `grievance.adapter.openagrinet.org`, which is a
  placeholder: the registry block on that same page records the two *upstream* hosts and
  says nothing about the adapter's own.
  The discovery service's `receiverId` is a placeholder for the same reason. Both must be settled before a catalog is published, because a published
  catalog is what callers route on and correcting one means republishing.

- **PMFBY publishes no OTP expiry.** `getOtp` answers `{status, data, error}` and nothing
  more, so `challengeIssued.expiresAt` is a configured TTL the adapter asserts. Confirm
  the real window with PMFBY and set `PMFBY_OTP_TTL_SECONDS` from it.

- **Body codec** — nothing on PM-KISAN ships without it. Designed under "The blocker"
  above; needs approval before implementation, since it is the only change to shared
  adapter code in this design. It carries a second, smaller change with it:
  `ParseProviderAuth` rejects any setting outside the closed `authFields` vocabulary, so
  the codec keys need their own parser before the config will start. JSONata cannot
  encrypt, the six auth schemes all attach a credential rather than transform a body, and
  a prerequisite runs before the outbound body exists — so this is a transport-level
  change and there is no cheaper place to put it. PMFBY is unaffected and can ship first.

### Applies to both providers

- **Publish what each action requires, or keep it documentation?** The packs state what a
  *response* must carry; what a *request* must carry lives only in the mapping guards and
  in the use-case document's "What each call must carry". A published annotation —
  `x-oan-required-by-action`, listing the mandatory fields per action per pack — would let
  a caller generate its payloads instead of reading prose, and would cover `support` as
  well as `status`. It is a new vendor keyword the validator does not read, so it is
  documentation either way; the question is whether it belongs in the pack. Undecided.

- **Every `/catalog/*` path in v2.0.0 is marked `deprecated: true`, with no replacement
  named.** All seven — `publish`, `on_publish`, `push`, `subscription`, `pull`,
  `on_pull`, `search` — carry the flag, as do `CatalogPublishAction` and
  `CatalogOnPublishAction`. `/discover` and `/on_discover` do not, so the retrieval half
  of this design is on current surface and only the publishing half is flagged. Nothing
  in the spec says what supersedes it, and the discovery service implements
  `catalog/publish` and ships working examples for it, so that is what this design uses.
  Goes to the network's spec authority with the other deviations: either the flag is
  stale and should be cleared, or there is a successor action nobody here has seen.

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
  00:00 and 05:30 IST. On PMFBY the generated `complaintDate` is *sent to the portal*, so
  the drift would be written upstream; on PM-KISAN the generated `case.filedOn` is what a later
  `status` matches against, so it would not merely mislabel the grievance — it would fail
  to find it. Both mappings above therefore shift before truncating, with
  `$fromMillis($toMillis($now()), "[Y0001]-[M01]-[D01]", "+0530")`. What remains open is
  only the premise: confirm with both portals that they record dates in IST.

- **The filing vocabulary is not published in the catalog, and for now need not be.**
  A caller rendering a category picker needs the list of categories the portal accepts.
  PM-KISAN's is a closed enum in its pack, so fetching `attributes.yaml` already yields
  it. PMFBY has no published list at all — see its own Open entry below — so there is
  nothing to declare and a `grievanceCategories` field would carry one hard-coded pair.
  The day PMFBY publishes its list it goes in the pack as an enum, exactly as
  PM-KISAN's did, and the catalog still carries nothing: the pack is already the
  machine-readable answer and a second copy in the catalog could only drift from it.

- **Terminal statuses — ask both portals for their full status vocabulary.** Neither
  documents which values close a case, so every commitment sits at `ACTIVE` and
  `status.descriptor.code` carries a constant. `case.status` holds the real state, but in
  each portal's own words, so a consumer without this pack cannot tell an open case from a
  finished one — which is the only job the coarse enum has. What is needed is the complete
  list of values each portal can return and which of them are terminal; `CLOSED` is then a
  mapping rather than a guess. Until then the field stays `ACTIVE`: inventing a terminal
  set would be worse than leaving it flagged.

- **Endpoint paths are confirmed; the hosts and credentials are not.** PMFBY's five paths
  are read from the v1 adapter on `main`: `POST /krphapi/FGMS/NICUsersLogin`,
  `/AddKRPHNCIPGrievenceSupportTicket` and `/GetGrievenceTicketsStatus` on the FGMS realm,
  and `POST /api/v1/services/nic/getOtp` and `/verifyMobile` on the policy realm behind
  `POST /api/v2/external/service/login`. One host, two unrelated tokens, and the FGMS one
  goes in a bare `Authorization` header with no `Bearer`. PM-KISAN's three paths and its
  `TokenNo` are taken from the legacy client. What remains unconfirmed on both:
  the production base URL — `pmfbydemo.amnex.co.in` is a demo host — the credentials'
  real values, and whether they are per-integrator.

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

- **`grievance.category`** — only `3`/`10` is used today. The pack carries both levels as
  values rather than writing them into the mapping, so when the portal publishes its real
  list the experience layer starts sending a genuine choice and nothing here changes. If
  that list arrives, enumerate it in the PMFBY pack the way PM-KISAN's `G001`–`G010` is
  enumerated: the two schemes bind `grievance.category` to different IRIs precisely so each
  can hold its own closed list.
- **`ticket-id`** — dropped, after a draft that mapped it. Nothing reads it back and it is
  an internal key, so publishing it costs without paying. Ask PMFBY whether the case read
  keys on it; if it does, the field comes back, which is far cheaper than retiring a
  published one.
- **`receiptSourceID` `134306`** — assumed fixed for the Vistaar channel.

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
- **Case identity** — `case.ticketNo` exists on PM-KISAN after all (`GrievanceID` /
  `GrievanceNo` on the lodge), but the portal accepts no ticket number on a read. So
  `case.filedOn` is still what retrieves the grievance, and it still collides for two
  grievances filed on the same identity the same day. What would close it is a
  `/GrievanceStatusCheck` that takes the grievance id, or at minimum one that repeats it
  per record so the adapter can match on it instead of the date. Ask PM-KISAN for either.
- **Category is write-only** — `Reg_No_Status` returns fourteen fields and `GrievanceType`
  is not among them, so a grievance the network filed under `G003` comes back with no
  category at all. The adapter will not infer one: a category reconstructed from a remark would be a
  guess wearing a governed code. Ask PM-KISAN whether the case read can return
  `GrievanceType`; if it can, the field round-trips as it does on PMFBY and the
  `on_status` payload gains it with no schema change — the pack already defines it.
- **`case.status` vocabulary** — the portal does return a `GrievanceStatus` field (the v1
  adapter formats it and exposes it as a `grievance-status` tag); the direct client's
  `GrievanceStatusDetail` model simply drops it. Carry it verbatim in `case.status.name`
  and map it to a `CaseStatusCode` for `code`; fall back to inferring
  `Replied`/`UnderReview` from `OfficerReply` only when it is absent. What is still needed
  is the phrase list itself, so the lookup can be written — until then every phrase falls
  to `UnderReview`, which is safe but uninformative.
- **"Show me all my grievances"** — the portal read returns every grievance on an identity,
  and `status` deliberately discards all but the matching one, because `/on_status` answers
  for one contract. If the network wants the full list as a farmer-facing view, it is a
  separate read and needs its own action — a discovery, not a contract query. Not specced
  here.
