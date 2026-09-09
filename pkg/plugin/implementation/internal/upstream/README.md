# internal/upstream

The machinery for serving a Beckn capability by calling an ordinary HTTP API
that has never heard of Beckn. Every domain package in
`pkg/plugin/implementation/` (`MandiPrice`, `WeatherObservation`,
`AgricultureFacility`) is a thin wrapper around this package.

## What this package decides, and what it does not

Recognises which requests are its capability's (`Run`), resolves the call
plan from the registry, authenticates, retries within a budget, and
translates in both directions via a mapping. That is everything -- endpoint,
method, timeout, retries, auth scheme, credentials, and the mapping used all
come from the registry row and the deployment's own `Config`, so nothing
about a specific provider is written in this package.

It does NOT decide:

- **Whether one payload is more than one call.** It is not: one payload in,
  one call out, always. A capability whose provider cannot answer a whole
  payload in one exchange splits the payload before it reaches this package
  and runs this step once per part -- see below.
- **How many such calls run at once, in what order, or what ceiling is too
  many.** Every one of those is a fact about the provider, and this package
  serves several, so it holds no opinion, no config field and no ceiling for
  any of them.

## The request lifecycle (`serve`)

One HTTP request corresponds to one call into `Step.Run`, which does, in
order:

1. **Recognise** whether the payload's binding key is one of this step's. If
   not, `Run` returns `nil` and does nothing -- several provider steps sit in
   one pipeline, and passing through silently is what lets adding a provider
   be one more config entry rather than a routing change.
2. **Resolve the call plan** from the registry -- the endpoint, method,
   budget and which mapping file answers this binding key and action.
3. **Verify** the payload against the mapping's own precondition -- what a
   capability requires of a payload is the mapping's rule, so a different
   requirement is a different mapping file rather than a different build.
4. **Resolve prerequisites** -- whatever real I/O the mapping cannot do
   itself, keyed by binding key. Most capabilities need none.
5. **Make the call** -- exactly one, with the registry row's timeout and
   retry budget.
6. **Translate the response** -- handed whatever the call(s) produced under
   `response`, plus what the request carried and prerequisites resolved,
   under `_local`.

## A provider that answers one question at a time

Some providers cannot answer a whole payload in one exchange. POCRA's
agriculture facility search takes exactly one category code, so a Beckn
payload asking for three facility types has to become three calls.

**None of that happens in this package.** There is no hook to supply, no
mapping direction to declare, no ceiling to configure. A capability with that
problem solves it one layer up, by wrapping this step:

- `pkg/plugin/implementation/AgricultureFacility/search.go` is a
  `definition.Step` of its own. It reads the facility types out of the
  payload, splits one inbound payload into one single-type payload per type
  (each with a fresh `context.messageId`, because POCRA blends answers that
  share one), runs the step below once per part, and merges the answers into
  one. Its ceiling of 8, its sequential-by-default concurrency and its
  merge rules are all facts about POCRA.
- `pkg/plugin/implementation/internal/concurrent`'s `Map` and `Run` do the
  bounded, ordered, cancel-on-first-error execution. They know nothing about
  Beckn or any provider.
- This package serves each part exactly as it serves any other payload, and
  never learns that it was one part of three.

The mapping is written for what it is actually handed, which is always a
single-type payload -- so both halves read the type straight off the payload
and this package injects no variable of its own into either.

`BindingPaths` is exported for exactly one purpose: a wrapping step has to
answer "is this payload mine?" the same way this step does, and reading the
same two config fields twice is how the two drift apart.

## Configuration a domain package hands `New`

`New(ctx, registry, mapper, prerequisites, cfg)`:

- `registry` and `mapper` are required.
- `prerequisites` (`map[string]func(ctx, beckn) (map[string]any, error)`,
  keyed by binding key) is whatever real I/O a mapping cannot do. `nil` or
  empty means every capability this step serves resolves nothing.
- `cfg` (`*Config`) is the deployment's own settings: which binding keys this
  step answers to, the binding-key path override, the auth scheme and its
  credentials, and the response size cap.

## Retry and timeout budget

`call`/`attempt`/`budget` apply the registry row's `timeoutMs`/`retryMax`
within this deployment's ceilings (`MaxTimeout`, `MaxRetryMax`) -- a row
asking for more than the ceiling is clamped, logged at warn, and still
served, because failing every request for a capability over a config mistake
is worse than serving it with a sane budget. A 4xx other than 429 is marked
permanent and not retried -- retrying a request that will fail identically
every time is not resilience, and retrying a missing credential in
particular reports the operator's own misconfiguration as the provider being
down.

## Credential redaction

Every auth scheme's credential is stripped from anything this package logs
or returns -- a URL, an error's text, and a failed response's body all get
the same treatment, because a provider quoting the request it just rejected
is the ordinary shape of a 401 or 403 body. See `secretForms`'s own doc
comment for why each scheme leaks differently and needs its own handling.
