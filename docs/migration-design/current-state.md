# Current state

If Bharat Vistaar keeps speaking Beckn v1.1.0 exactly as it does today, how much of it can OpenAgriNet V2 already answer?

**Six of fifteen.** The other nine are not variations on the six, and four of them need a capability pack that does not exist yet.

*[back to the approach](README.md)*

## What Bharat Vistaar actually calls

The clients hold one address and append the action to it. Across the Orchestrator and Voice, three actions are ever appended:

| | Used by |
|---|---|
| `POST /search` | every discovery capability: weather, mandi, knowledge, scheme search |
| `POST /init` | PM-Kisan and PMFBY, to start an application or grievance |
| `POST /status` | PM-Kisan and PMFBY, to check one |

The old service exposes more than this: `/mobility/select`, `/confirm`, `/rating`, a parallel `/dsep/*` set, a browser proxy, and two feedback routes (`app.controller.ts:30-238`). **Nothing in the Orchestrator, Voice, App or UI calls any of them.**

### Fifteen capabilities, one endpoint

They do not line up one to one. Almost everything arrives as `POST /search`, and the capability is identified by a field in the body rather than by the URL (`app.controller.ts:72-99`):

| `category.descriptor` value | Capability | Matched on |
|---|---|---|
| `knowledge-advisory` | knowledge advisory | name, **exact case** |
| `Weather-Forecast` | weather, IMD | name, **exact case** |
| `Weather-Forecast-Mausamgram` | weather, Mausamgram | name, **exact case** |
| `schemes-agri` | PM-Kisan | code, or lowercased name |
| `icar-schemes` | ICAR schemes | code, or lowercased name |
| `pmfby`, or any code starting with it | PMFBY | code prefix, or lowercased name |
| `price-discovery` | mandi prices | code, **plus** `item.descriptor.code = mandi` |
| anything else | knowledge advisory | the `default:` arm |

Three things to carry into the port:

1. **The first three are case-sensitive and the rest are not.** A payload sending `weather-forecast` in lower case does not reach the weather handler; it falls to the default arm and comes back as a knowledge answer.
2. **Mandi is the only capability needing a second field.** `price-discovery` with any other item code resolves to `unknown`.
3. **There is no "not found".** The `default:` arm answers everything else with a knowledge search, so V1 has no 404 and no unmatched-request error. Anything the V1 Adapter does not recognise has to be forwarded, so it lands on this same arm and behaves exactly as it does today.

`/init` and `/status` dispatch on different fields again: `/init` on whether the body carries an order and which provider it names, `/status` on the shape of the body (`app.controller.ts:142-196`, `app.service.ts:1043-1108`).

## What V2 can already serve

| Capability | V2 pack | Covered? |
|---|---|---|
| Mandi prices by commodity code | `MandiPrice` / `agmarknet` | **yes** |
| Weather — Mausamgram | `WeatherObservation` / `mausamgram` | **yes** |
| Knowledge advisory | `KnowledgeAdvisory` / `bharat-vistaar` | **yes** |
| PM-Kisan application status | `SchemeApplication` / `pmkisan` | **yes** |
| PM-Kisan grievance | `Grievance` / `pmkisan` | **yes** |
| PMFBY grievance | `Grievance` / `pmfby` | **yes** |
| Mandi prices by commodity **name** | — | no |
| Scheme content (Hasura) | — | no |
| PMFBY policy and claim status | — | **no pack** |
| Soil health card | — | **no pack** |
| SMAM benefit status | — | **no pack** |
| Crop registry + recommendation (GFR) | — | **no pack** |
| Seed availability (SATHI) | — | **no pack** |
| Weather — IMD | — | **no pack**, live via Voice |
| ICAR schemes | — | no known sender |

Four of the nine need a new capability pack built first: soil health, insurance policy and claim, seed availability, crop recommendation. SMAM may fit `SchemeApplication` and the scheme content may fit `KnowledgeAdvisory` — both need checking against the pack rather than assuming.

Mandi-by-name is the cheapest of the nine. Same provider, same capability, just a different request shape. V2's mapping requires Agmarknet's governed commodity codes and refuses a payload without them (`mandi-price.select.yaml:52+`); V1 takes a commodity name and a lat/lon. Something has to resolve one into the other.

## "Covered" does not mean identical

A binding existing is not the same as it answering the same call. Three gaps found by comparing field by field:

| | Bharat Vistaar today | V2 today |
|---|---|---|
| **PM-Kisan grievance OTP** | `POST /GrievanceAadhaarToken` (`pmkisan-grievance.service.ts:224, 431`) | `POST /ChatbotOTP` (`pmkisan/grievance.init.yaml:1`) |
| **Agmarknet auth** | credentials exchanged for a token, cached and refreshed on rejection (`agmarknet-api.service.ts:16-19`) | documented as a static `token` query param |
| **GFR backend** | shares `SOIL_HEALTH_BASE_URL`, GraphQL `getCropRegistries` (`gfr.service.ts:58, 118`) | not present |

PMFBY grievance is the one that lines up cleanly. V1's endpoints are exactly what our `pmfby/grievance.{status,support}` mappings call. That binding was clearly built from the live branch, and it is the proof the approach works.

## There are two clients, not one

This matters for every decision downstream.

| Client | Sends | Gets |
|---|---|---|
| **Orchestrator** (`bh-dev`) | `Weather-Forecast-Mausamgram` | Mausamgram |
| **Voice** (`bh-voice-prod`) | `Weather-Forecast` | **IMD** |

Both also send `price-discovery` for mandi. The dispatcher matches the raw name case-sensitively (`app.controller.ts:184-188`), so Voice's string matches exactly and the IMD route is live. An earlier pass over the Orchestrator alone had written it off as dead.

So **every traffic count, golden test, metric label and migration decision is per client.** `icar-schemes` still has no sender in either client, but that is an argument from absence. Traffic data settles it, not grep.

Not everything the agent does goes over Beckn. Document, video and pest search call Marqo directly (`search.py:484-492, 580-589`); image analysis calls NPSS; geocoding calls a maps API. None of that is adapter work.

**One route already misbehaves.** `search_schemes` sends category `scheme-agri-qdrant`, which matches no case in the dispatcher and falls through to a hardcoded vector index at a bare IP over plain HTTP (`app.service.ts:457`). The branch that was supposed to move it to a Beckn provider is unmerged. Confirm against the running deployment before transforming it.

## Shape differences the V1 Adapter absorbs

These apply to every call, including the six we "have".

| | V1 | V2 |
|---|---|---|
| **Version** | `1.1.0` | `2.0.0` |
| **Actions** | `search`, `select`, `init`, `confirm`, `rating`, `status` | `discover`, `select`, `init`, `confirm`, `status`, `support`, `rate`, … |
| **Payload** | `message.intent` / `message.order`, `message.catalog` back | `message.contract.commitments[].resources[].resourceAttributes` |
| **Identity** | `category.descriptor.code` or `.name`, plus `provider.id` | binding key `providerId\\|capabilityCode` |
| **Tags** | `context.tags` is an object | Beckn tags are a list |

Four things have no clean V2 equivalent:

1. **OTP smuggled in `order_id`.** A 4-to-6-digit `order_id` means "verify this OTP".
2. **Status calls with no order and no provider.** PM-Kisan `/status` has to be correlated on `transaction_id` from the earlier `/init` — which means holding state.
3. **Routing on absence.** PM-Kisan application status works by sending empty strings and falling through a default arm. The fall-through has to be reproduced, not just the matches.
4. **V1 never refuses a search.** Returning errors where V1 answered gives the agent failure modes it has never handled.

The V1 Adapter does build a V2 payload: it asks the V2 network, in the network's own shape, and maps the answer back. That was tried both ways and this is the way that was kept, because the alternative — V1 to provider to V1 — reproduces V1 without ever putting the V2 representation on the wire, and so proves nothing about whether the new network can serve the old callers. The cost is that whatever V2 cannot carry is genuinely lost, and the point is that the loss is now measurable instead of hypothetical.

The four above still have no clean V2 equivalent, and they matter for the capabilities beyond weather. Nothing in this section is settled for the OTP, status and routing-on-absence cases.

## What the spike measured, and what it changed in beckn-onix

Measured, not predicted. Two beckn-onix adapters were stood up beside the running legacy service, chained so that a V1 search leaves the first one as a V2 select and the V2 answer is mapped back:

```
V1 caller --search--> V1 adapter --select--> V2 adapter --GET--> Mausamgram
```

Both were driven with the same five requests for the Mausamgram forecast, against the legacy service running in parallel. All five came back with the status V1 sends. Three matched field for field, including every awkward detail the legacy generator has: day numbering that skips a missing day, `0 mm` where a reading is zero but `N/A %` where it is absent, two different coordinates in the same reply, and V1's refusal to answer at all when the forecast starts too late. Six fields across the other two differ.

**Writing V1 replies as a mapping works, even with the V2 representation in the middle.** Day numbers are the sharpest case: V1 labels days `Today`, `Day_2` and so on, and a V2 answer carries no slot number anywhere. The label is recovered from the validity window, which works because `fcstday1` is today for this provider.

**One field is genuinely lost, and it is a schema pack question rather than a mapping one.** V1 renders Weather Condition as the weather warning, else the provider's prose cloud summary. The warning survives: the pack has an `Alert` parameter. The prose does not, because the `parameter` enum is a closed list of measurements and a sentence is not one. No mapping can carry a field through a representation with no field for it. Note this is sharper than "add a cloud parameter": a numeric cloud percentage is the obvious addition and would not help, because what a farmer is shown is the sentence.

**One field differs because the adapter is better than V1**, which is still a difference someone should accept deliberately. V1 renders the wind direction text the provider sends and shows `N/A` when it is absent; V2 carries the bearing in degrees, so the mapping turns degrees back into text and fills in the day V1 left blank.

Three changes were needed in beckn-onix. All are small, all are general rather than weather specific, and all are now merged and covered by its tests.

**A fallback half in a mapping.** beckn-onix had one way to not answer, and it was an error; V1 has one way to not answer, and it is a success. Every V1 failure path, provider outage and bad input alike, ends at a success status with a catalog that has no providers in it, and nothing downstream distinguishes them or expects anything else. A mapping now has a third outcome beside request and response: emit this fixed body, without calling anyone. One addition covers both the pre-call and post-call cases, because the body is the same. Most of the fifteen capabilities swallow provider failures the same way.

**`answersUnaddressed`.** The provider step refused any request that did not name a receiver, reading `bpp_id`, `bppId` or `receiverId`, before it looked at which capability was asked for. Bharat Vistaar sends none of them: searching its source for `bpp_id` finds nothing. That check is now a config setting, off by default, and the binding check still follows it.

**`responseStatus`.** V1 is a Nest service and answers 201 on POST. The adapter always wrote 200. Matching it costs one config line.

The exercise also turned up two silent-loss bugs in beckn-onix, both the same shape: a hand-written list of fields mirroring a struct, drifting from it with nothing to catch the drift. Neither was visible from reading the code.

Details and reproduction in [the V1 adapter spike](local-stack/v1-adapter/README.md). An API collection that drives the stack is in [local-stack/collection](local-stack/collection/README.md).

## Open questions

1. **Which capabilities carry real traffic, per client?** Grep is not a census. It missed that the IMD route was live. Telemetry on the production service settles it, and it decides what moves first.
2. **Who owns transaction state?** Needed by PM-Kisan `/status`, PMFBY `/status` and every OTP flow. Nothing in V2 holds it today, and the V1 Adapter must not.
3. **Is the V2 response synchronous?** V1 callers block on the HTTP response. If our V2 path is callback-based for any of these, the real migration has to bridge that. Not a V1 Adapter problem, but it lands here first.
4. **Four packs, or fewer?** Deciding before writing mappings avoids writing them twice.
5. **How long does V1 stay up?** A six-month bridge and a permanent compatibility layer are different artefacts.
6. **Where does a prose weather condition live in the pack?** The one field measurably lost crossing V2. It needs a decision from whoever governs the WeatherObservation pack, and it should be taken knowing that a numeric cloud percentage does not answer it.
7. **Who is a V1 request addressed to?** `answersUnaddressed` lets the request in, but the binding key still needs a participant on its left and a V1 payload has none. The spike synthesises one from the capability code, which works and means the identifier is invented by the adapter rather than supplied by the caller. It needs a deliberate decision rather than a default inherited from a spike.

## Sources

Read from source at these commits. Bharat Vistaar's own `docs/` tree describes branch `BharatVistaar`, which is **138 commits behind `main`** and reports 8 routes where `main` has 13. Nothing here comes from it.

| Repo | Commit |
|---|---|
| `BharatVistaar/Beckn`, branch **`main`** | `a091a23` (2026-07-01) |
| `BharatVistaar/Orchestrator`, branch `bh-dev` | `6d4b969` (2026-08-11) |
| `BharatVistaar/Voice`, branch `bh-voice-prod` | read 2026-11-04 |
| `OpenAgriNet/beckn-onix` | `076b836` (2026-10-28) |
| `OpenAgriNet/network-specs` | `0376813` (2026-11-04) |

## The legacy service cannot be rebuilt from its own Dockerfile

Its `Dockerfile` pins `node:20.0.0`, whose Debian release has been archived, so the `apt-get install curl` layer fails on a 404 from the security mirror. The curl being installed is unused; every outbound call goes through axios. The `start:prod` script also points at `dist/main`, while the build emits `dist/src/main`.

This does not affect the running deployment, which was built when those packages still resolved. It does mean that deployment cannot be reproduced from source today, which matters for the migration: if the old service needs a change while it is still carrying the capabilities we have not moved, that change cannot currently be shipped. Verified by building it, in [local stack](local-stack/README.md).
