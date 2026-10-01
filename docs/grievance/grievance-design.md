# Grievance — Design

Date: 2026-09-28 · Revised: 2026-09-30 · Status: draft

## Introduction

When farmers have a complaint about crop insurance or income support, they must use that
specific scheme's portal. PMFBY and PM-KISAN both have their own separate portals. Each
portal uses different technical setups, defines grievances differently, and checks identity
in its own way.

Our grievance system connects both portals using a single standard interface. This lets
applications log complaints and check status without needing to learn how each individual
portal works.

To do this, the system converts standard requests into calls that each portal understands,
and then converts the portal's answers back into standard responses. **This document
explains how we chose that translation method over other options.**

## Solution 1 — an agent in the network layer

Two agents, one boundary between them.

```
farmer  →  experience-layer agent  →  network-layer agent  →  PM-KISAN
                                                           →  PMFBY
                                                           →  …
```

The **experience-layer agent** talks to the farmer and states what is wanted — file a
grievance on this scheme, read this case. It knows nothing about any provider.

The **network-layer agent** is schema-aware. It reads the provider's published schema at
request time, works out which provider serves the request, builds the upstream call and
interprets the reply — no mapping file for anyone to write. It also owns everything the
experience layer should never see: the AES-GCM envelope PM-KISAN insists on, Beckn request
signing, credentials, retries, timeouts. One place where transport lives, for every
provider on the network.

Onboarding a provider is publishing its schema and nothing else. That is what makes this
attractive once there are many of them: the cost of the tenth provider is the same as the
cost of the second. It also dissolves the PM-KISAN envelope problem — a provider that
speaks in ciphertext is the network agent's business, not a special case bolted onto a
mapping.

## Solution 2 — an adapter plugin driven by the schema

The same schema, read ahead of time by a person rather than at request time by an agent,
and frozen into one JSONata mapping per action. A plugin covers what a mapping cannot
express — an OTP verify, an AES-GCM envelope.

Onboarding a provider is writing those mappings and seeding three registry rows.

## The choice

**We are starting with the schema-driven adapter plugin**, because two providers do not pay
for the generality of the agent.

**We will move to the agent in the network layer as the number of use cases grows.**
Nothing built now blocks that: the packs, the registry records and the Beckn contracts all
stay as they are, and only the middle of the request path changes.
