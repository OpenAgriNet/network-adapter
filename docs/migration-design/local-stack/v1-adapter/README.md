# Can a V1 caller be served out of the V2 network?

This directory is a spike, not a product. It asks one question and answers it
with a measurement: can BharatVistaar keep sending the request it sends today,
get back the reply it expects today, and have the answer come from the new
network rather than from the provider directly?

The answer is yes, with six fields out of several hundred that differ. Those six
are worth more than the hundreds that match, and they are written up at the
bottom.

The capability under test is the Mausamgram five day weather forecast. It was
chosen because it is the busiest V1 capability, because its reply is the most
intricate of the fifteen (nested tag groups, unit suffixes, two separate
fallback chains), and because the V2 network already publishes a mapping for it
that this spike can use unmodified.

## The shape of it

```
V1 caller --search--> V1 adapter --select--> V2 adapter --GET--> Mausamgram
              :3001                 :3002               :9100
```

Both adapters are the same beckn-onix binary and the same plugins. They differ
in a config file and a mapping. V2 is what travels between them, so the
V2 representation is genuinely in the path and anything it cannot carry is
genuinely lost. V1 never leaves the outer edges.

Nothing about the inner hop is special-cased. The V1 adapter reaches the V2
adapter the way any upstream is reached: a base URL off a participant record, a
method and a path off an action plan, a mapping in both directions. That is the
second claim this spike makes and the more useful one. Serving V1 is a
deployment of what already exists, not a second thing to build.

## The other mode

The diagram above is one of two paths, and the one that took the work. The other
is the one almost every request takes on day one:

```
V1 caller --search--> V1 adapter --> legacy BharatVistaar Beckn
              :3001                           :3000
```

Anything the adapter is not configured to answer is forwarded to the old service
and the reply comes back untouched. That is what makes the migration
incremental: a capability starts forwarded, moves to answered when its mapping
is ready, and moves back by deleting a line. On day one nothing is answered
locally and the old service sees exactly the traffic it sees today.

It needed one change in beckn-onix and no new code beyond it. The routing step
already matched a request to an address and the proxy already streamed the reply
back verbatim; what it could not do was stand behind a step that had already
answered, because setting a route discards the answer and calls upstream for one
we were holding. It now leaves an answered request alone. The order of the steps
is then the whole of the dispatch, with forwarding last.

Measured both ways. A capability the adapter does not serve returns the same
status and the same body through :3001 as calling :3000 directly, differing only
in the timestamp the legacy service regenerates per call. The weather request is
answered locally in a tenth of a second and never forwarded.

Two things this turned up that a design document would not have:

**The write timeout is a forwarding decision, not a serving one.** The legacy
knowledge path takes about seventy-five seconds to fail in this stack. The
adapter's write timeout was sixty, so the forwarded reply arrived after the
deadline, the server discarded it, and the caller got a dropped connection. It
reads like the adapter failing when it was the old service being slow. It must
exceed the slowest thing it forwards to, so the real value cannot be chosen
until the old service's live latency is known.

**Two of the old service's paths cannot be forwarded as written.**
`feedback/:id` and `submit-feedback/:id` carry a path parameter, and the routing
table matches an exact path. Neither is a Beckn action and neither carries a
Beckn envelope, so neither reaches this adapter today. If BharatVistaar is ever
pointed here for them, they fail.

## What is running

Five processes. Two come from the compose stack in the directory above.

| what | port | where it comes from |
|---|---|---|
| mock Mausamgram | 9100 | `../mock-mausamgram`, five fixtures chosen by latitude |
| legacy BharatVistaar Beckn | 3000 | the real V1 service, in Docker, pointed at the mock |
| mock registry and mapping host | 9200 | `registry.py` here |
| the V2 provider adapter | 3002 | the stock beckn-onix binary, `v2-adapter.yaml` here |
| the V1 adapter | 3001 | the same binary, `adapter.yaml` here |

The legacy service on :3000 is the contract. It calls the provider directly and
has never heard of V2. Whatever it returns is what :3001 has to return.

## Running it

```
cd ..            # the local-stack directory
docker compose up -d
cd v1-adapter
./run.sh
```

`run.sh` builds the adapter and its plugins, starts the registry and both
adapters, sends the same five payloads to :3000 and :3001, diffs every field of
the two replies, and tears the processes down again.

To keep the stack up and poke at it yourself, use `./up.sh` and `./down.sh`
directly. There is an API collection in `../collection` that drives it.

## The five scenarios

The mock provider picks its answer from the latitude, so one request file
covers all five.

| scenario | lat | what the provider does |
|---|---|---|
| happy | 18.4 | all five forecast days present |
| gaps | 1.1 | day 2 missing; some fields null, zero, or empty |
| late-only | 2.2 | only days 3, 4 and 5 present |
| provider-down | 3.3 | HTTP 500 |
| zero-coords | 0 | never called; the coordinates fail the guard first |

## The result

All five return 201, the status the legacy Nest service returns. Three match
field for field. Two differ, by six fields in total.

```
DIFFERS  gaps       tags.0.list.9  'Dry spell'             vs 'N/A'
                    tags.2.list.9  'Cloudy, light showers' vs 'N/A'
                    tags.3.list.8  'N/A'                   vs 'North Easterly'
DIFFERS  happy      tags.1.list.9  'Mainly clear'          vs 'N/A'
                    tags.3.list.9  'Cloudy'                vs 'N/A'
                    tags.4.list.9  'Clear'                 vs 'N/A'
match    late-only
match    provider-down
match    zero-coords
```

Five of the six are the same loss, and the sixth is not a loss at all.

### Weather Condition, lost, and fixable only in the schema pack

V1 renders Weather Condition as the weather warning, else the provider's prose
cloud summary, else `N/A`. The V2 pack keeps the warning: there is an `Alert`
parameter and the warning survives the round trip intact. It has nowhere to put
the prose. The `parameter` enum in the WeatherObservation pack is a closed list
of measurements, and a sentence like "Mainly clear" is not one. So every day
where the provider sent no warning loses its cloud summary, and the V1 adapter
can only say `N/A`.

This is the one genuine fidelity loss in the five scenarios, and it is not a
mapping problem. No mapping can carry a field through a representation that has
no field for it. The decision belongs to whoever governs the pack, and it is
sharper than "add a CloudCover parameter": a numeric cloud percentage is the
obvious addition and would not help, because what V1 displays to a farmer is the
sentence, not the number. The pack needs somewhere for a prose condition
summary to live, or V1 callers lose that line.

Worth knowing how much this matters before deciding. Of the ten readings per
day, this is one, and it is the only descriptive one; the nine numeric readings
all survive. Whether the farmer-facing screen shows it is a question for the
BharatVistaar team.

### Wind Direction, where the adapter is better than V1

The provider sends wind direction twice: an abbreviation with a long form
alongside it, and the same bearing in degrees. V1 renders the long form. The V2
pack carries the degrees, which is the right choice for a machine-readable
network, so the V1 mapping turns degrees back into the long form with a
sixteen-point compass table.

The table agrees with V1 on every day where the provider sent a long form. The
one difference is a day where the provider sent the abbreviation with no long
form beside it, so V1 rendered a bare `N/A` while the adapter correctly says
`North Easterly` from the degrees.

That is an improvement, and it is still a difference. It is listed here rather
than suppressed because a V1 caller that has learned to expect `N/A` on some
days would now see a value, and someone should decide that deliberately rather
than discover it.

### What the three matching scenarios had to get right

"Identical" is only interesting if the thing reproduced was hard. Each of these
is a place where a careful reimplementation would plausibly have gone wrong, and
several are places where the V2 hop makes it harder than reimplementation would
have been:

- **Day numbers survive a representation that does not carry them.** V1 labels
  the days `Today`, `Day_2` and so on, numbering by slot so a missing day leaves
  a hole: with day 2 absent the reply reads `Today, Day_3, Day_4, Day_5`. The V2
  answer has no slot number anywhere. It has one resource per day with a
  validity window, and the label is recovered by subtracting today's date in
  IST. That works because `fcstday1` is today for this provider, which is a
  property of the provider rather than of the schema, and is the sort of thing
  worth writing down before someone changes the mapping.
- **V1's refusal to lead with Day_3 survives too.** When the provider sends only
  the later days, V1 returns an empty catalog rather than relabelling day 3 as
  today. The adapter reproduces that, which means the mapping has to know the
  difference between "a day is missing" and "the forecast starts too late to be
  worth sending".
- **Zero is a value, `null` is not.** `rain: 0` renders as `0 mm`. Only an absent
  or null reading becomes `N/A`. JSONata's `?:` collapses both, so the mapping
  distinguishes "exists" from "truthy" with two different helpers, on both sides
  of the hop.
- **`N/A` keeps its unit.** A missing humidity is `N/A %`, not bare `N/A`. The
  substitution happens on the reading, before the suffix.
- **Two different coordinates in one reply.** The Location tag echoes the
  coordinates that were asked for. The fulfillment stop and the item name carry
  the coordinates the provider resolved its grid cell to. They differ by a
  fraction of a degree, both are correct, and the asked-for pair has to survive
  two hops to come back out.
- **A provider outage and a bad input produce the same benign empty reply.**
  V1 has exactly one way to not answer: 200 with a catalog that has no providers
  in it. Both of those paths now reproduce exactly.

So the mapping is not matching by accident or by being vague. The shape of the
V1 reply, including its quirks, is expressible in configuration on top of a V2
hop.

## What had to change in beckn-onix

Four changes, all small, all general rather than weather specific, all now
merged into the adapter and covered by its test suite. The fourth, the routing
step leaving an answered request alone, is described under the other mode
above; the three the answer path needed are below.

**A fallback half in a mapping.** onix had exactly one way to not answer: a
NACK, which is an error status and an error body. V1 has exactly one way to not
answer: a 200 with an empty catalog. Every V1 failure path ends at the same
benign reply, and nothing downstream expects an error. So a mapping gained a
third outcome alongside request and response: produce this fixed body. It is
needed in two situations, pre-call when the input fails a guard and post-call
when the provider did not answer, and the same body serves both.

This will not be unique to weather. Any V1 capability that currently swallows
provider failures needs it, and from reading the legacy service that is most of
them.

**`answersUnaddressed`.** Before the provider step looks at which capability was
asked for, it checks whether the request names a receiver, reading `bpp_id`,
`bppId` or `receiverId` from the context. Searching the whole BharatVistaar
source for `bpp_id` finds nothing: V1 payloads name a category, never a
counterparty. The check was unconditional. It is now a config setting, off by
default, because the binding check still follows it and anything the step is not
configured for still passes through untouched.

**`responseStatus`.** V1 is a Nest service and answers 201 on POST. The adapter
always wrote 200. Whether anything on the BharatVistaar side reads the status
code is still unknown, but matching it costs one config line and removes the
question.

## Two bugs this found in beckn-onix

Both are the same shape, and the shape is worth naming: **a hand-written list of
fields that mirrors a struct, and drifts from it silently.**

Four provider plugins each carried their own `parseConfig` duplicating the
shared one. When two new settings were added to the shared parser, all four
ignored them, and the symptom was a correct-looking config that did nothing.
They now delegate.

The step instrumentor copied a `StepContext` in wholesale and copied a
hand-written field list back out. `ResponseStatus` was not on the list, so a
step could set a status and have it silently discarded, and every answer went
out on the default 200 with nothing failing. It is now a whole-struct
assignment, with a regression test.

Neither was visible from reading the code. Both turned up from running the two
stacks side by side and diffing, which is the argument for keeping this harness
and adding the second capability to it.

## Still open

**Who a V1 request is addressed to.** `answersUnaddressed` lets the request in,
but the binding key still needs a participant on its left, and a V1 payload has
no participant anywhere. The spike synthesises one from the capability's own
code, giving `WFC|Weather-Forecast-Mausamgram`. That works and it means the
participant identifier for V1 traffic is invented by the adapter rather than
supplied by the caller. It needs a deliberate decision before this is deployed,
not a default inherited from a spike.

**The prose cloud summary**, above. It needs a pack decision.

**Whether anything reads the 201.** Matching it was cheap, so this is no longer
blocking, but it is still unverified against the real caller.

## Files

- `adapter.yaml` — the V1 adapter config. Comments explain each omission as well
  as each inclusion, in particular why there is no key manager, signer or
  validator.
- `v2-adapter.yaml` — the V2 provider adapter. Deliberately spare: it relies on
  the Beckn v2 defaults for everything the V1 side next door has to spell out,
  and the contrast is the point.
- `registry.py` — the mock registry. Serves both participant rows and both
  action plans, and hosts the V1 mapping. Serves the V2 mapping straight out of
  the beckn-onix tree rather than from a copy, because a copy would drift and
  the whole question is what the real V2 mapping leaves out.
- `mappings/weather-forecast-mausamgram.search.yaml` — V1 to V2 on the way in,
  V2 to V1 on the way out, and the fallback body for when there is no answer.
  Commented against the legacy generator it reproduces.
- `routing.yaml` — where a request goes when no capability answered it. One row
  per endpoint of the old service, enumerated rather than wildcarded, so the
  file doubles as the inventory of what this adapter stands in front of.
- `up.sh`, `down.sh` — start and stop the three processes this directory adds.
- `run.sh` — up, exercise all five scenarios against both services, diff, down.

`plugins/` and `.run/` are build and run artifacts and are not checked in.
