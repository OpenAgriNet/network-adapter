# Capabilities

One page per capability. Each covers only its own data — the shared rules (always emit an envelope, error handling, golden tests) are in [how it works](../README.md).

## The two modes

| Mode | What happens |
|---|---|
| **forward** | The request goes to the old service untouched, and its reply comes back untouched. This is the default, and what anything unrecognised gets. |
| **answer** | We answer it ourselves: V1 request → provider → V1 reply. Never through V2. |

**There is no third option.** The V1 Adapter never refuses a request, because V1 never refuses one. An unmatched payload falls through to a knowledge answer rather than an error. A capability with no row is forwarded.

## What may move

A capability may only move to **answer** if a V2 capability pack **already exists** for it. For those, V2 already covers the provider, so the V1 mapping is duplication that gets deleted later. Writing a V1 mapping for an *uncovered* capability would leave a V1-only integration as the only copy of that provider's logic, to be rewritten from scratch when Bharat Vistaar migrates for real.

That caps the work at six capabilities and takes all four missing packs off the critical path by construction, not by promise.

## Status

Nothing is in answer mode yet. The order below is provisional — **the traffic count comes first** and may change it.

| Capability | V2 pack exists | Mode | Page |
|---|---|---|---|
| Weather — Mausamgram | `WeatherObservation` / `mausamgram` | forward → **first candidate** | **[written](weather-mausamgram.md)** |
| Mandi prices by code | `MandiPrice` / `agmarknet` | forward → candidate 2 | next |
| Knowledge advisory | `KnowledgeAdvisory` / `bharat-vistaar` | forward → candidate 3 | — |
| PM-Kisan grievance | `Grievance` / `pmkisan` | forward — needs OTP review | — |
| PM-Kisan application status | `SchemeApplication` / `pmkisan` | forward — needs OTP review | — |
| PMFBY grievance | `Grievance` / `pmfby` | **forward, permanently** | — |
| Weather — IMD | — | forward | live, Voice only |
| Mandi prices by name | — | forward | — |
| Scheme content | — | forward | — |
| PMFBY policy / claim | — *no pack* | **forward, permanently** | — |
| Soil health card | — *no pack* | forward | — |
| SMAM benefit status | — *no pack* | forward | — |
| Crop registry (GFR) | — *no pack* | forward | — |
| Seed availability (SATHI) | — *no pack* | forward | — |
| ICAR schemes | — | forward | no known sender |

### Two things that will change this order

**Knowledge advisory is not the obvious first move it looks like.** Its main calls go straight to Marqo and never touch Beckn at all (`Orchestrator/agents/tools/search.py:484,580`). The one knowledge call on the Beckn path matches no case in the dispatcher and falls through to `"unknown"`, and the Qdrant migration is retiring it anyway.

**Agmarknet credentials are the main risk on mandi.** The old service holds a cached token reused until rejected, with a single-flight refresh and a mutex serialising calls (`services/mandi/agmarknet-api.service.ts:18-47`). Whether our `oauth2` auth scheme covers refresh-on-401-with-single-flight is unverified. Settle it before writing that mapping.

### Why PMFBY is excluded permanently

The old service keeps a set of transaction IDs that have passed OTP verification (`app.service.ts:90`). A `/status` call adds to it after verifying (`:1808`), and a **later** `/search` refuses unless the ID is in it (`:3029`).

That is live state held between calls. Move the PMFBY search to a mapping while `/status` is still proxied and the OTP gate silently disappears, serving insurance data to someone who never verified.

The V1 Adapter holds no state and must never grow any. PM-Kisan grievance and status carry the same risk and move only if the existing OTP machinery covers them without one.

## Page layout

Every capability page has the same eight sections, so they can be read against each other.

| | |
|---|---|
| 1 **Status** | its mode, and what has to happen before it changes |
| 2 **The V1 call** | what the client sends and expects back, with a real payload |
| 3 **The provider call** | what the upstream API takes and returns |
| 4 **Request map** | V1 field → provider request |
| 5 **Response map** | provider field → V1 field |
| 6 **Behaviour deltas** | what changes for the farmer, and the fix. Should be near-empty. |
| 7 **Verification checklist** | what has to be ticked to sign it off |
| 8 **Open** | questions specific to this capability |

Section 6 being near-empty is the whole point of going V1 → provider → V1. A page with a long section 6 has drifted back towards routing through V2.
