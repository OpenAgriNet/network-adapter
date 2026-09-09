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

- **What a payload has to be split across.** That is a mapping feature
  (`definition.DirectionFanOut`), evaluated generically by `fanOut()` -- any
  capability's mapping can declare a fan-out half, and this package resolves
  it into a list of values with no domain knowledge involved.
- **How many of those calls run at once, in what order, or what ceiling is
  too many.** That is the fan-out hook's job (below), and this package has no
  built-in one -- nor even a name for it.

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
5. **Resolve the fan-out** -- evaluate the mapping's fan-out half, if it has
   one, into `nil` (no fan-out; one call) or a `[]any` of values.
6. **Make the call(s)** -- `oneCall` makes exactly one; if `fan == nil` it is
   called directly, otherwise the step's configured fan-out hook decides how
   many times and how. A fan-out with no hook configured is refused.
7. **Translate the response** -- handed whatever the call(s) produced under
   `response`, plus what the request carried and prerequisites resolved,
   under `_local`.

## The fan-out hook, and why it has no name here

There are two constructors, and which one a domain package calls is the
whole statement about whether its capability fans out:

- `New(ctx, registry, mapper, prerequisites, cfg)` -- one payload is one
  call. What `MandiPrice` and `WeatherObservation` call, unchanged since
  before fan-out existed, which is the point: adding fan-out for one
  capability did not touch the packages that do not have it.
- `NewWithFanOut(ctx, registry, mapper, prerequisites, gather, cfg)` -- the
  same, plus the hook below. What `AgricultureFacility` calls.

`NewWithFanOut`'s `gather` is a plain function value of this shape:

```go
func(ctx context.Context, values []any, one func(ctx context.Context, fanValue any) (any, error)) (any, error)
```

The inner `one` is everything this package knows how to do about calling a
provider correctly -- authenticate, retry within budget, decode. The outer
function is everything it deliberately does NOT know: how many times to call
`one`, in what order, how many at once, or what ceiling is too many --
because that is a fact about the specific PROVIDER a domain package wraps
this for.

**This package gives neither shape a name on purpose.** The name for the
outer one is `Gather`, and it lives in
`pkg/plugin/implementation/AgricultureFacility/fanout.go`, because "gather"
is a plugin-specific idea and that is the plugin that has it. Declaring
`type Gather` here would put a plugin's vocabulary in the generic machinery,
and referencing `AgricultureFacility.Gather` here would be an import cycle
(and backwards -- domain packages wrap this one, never the reverse). Go's
structural func typing means neither is necessary: a value of
`AgricultureFacility.Gather` is directly assignable to the unnamed parameter
declared here.

`nil` is valid, and is exactly what plain `New` passes -- a capability whose
mappings never declare a fan-out half has nothing to gather. A mapping that
declares one on a step built by `New` is refused rather than silently handled
by a built-in policy this package does not have.

The one real implementation splits across two packages, deliberately:

- `pkg/plugin/implementation/internal/concurrent`'s `Run` is the generic
  engine -- bounded concurrency, ordered results, cancel-on-first-error. It
  knows nothing about Beckn or POCRA; it would be exactly this useful to any
  future capability that needs to fan one request into several calls.
- `pkg/plugin/implementation/AgricultureFacility/fanout.go`'s
  `gatherFacilities` is the one caller: it knows `MaxFanOut` is 8 because
  POCRA is slow, that the default concurrency is 1 because POCRA's failure
  mode under load is silent, and that each call needs a fresh UUID because
  POCRA blends answers sharing a `message_id`. None of that belongs in
  `concurrent.Run`, and none of it belongs in this package either.

## Configuration a domain package hands `New`

`New(ctx, registry, mapper, prerequisites, cfg)`, or
`NewWithFanOut(ctx, registry, mapper, prerequisites, gather, cfg)`:

- `registry` and `mapper` are required.
- `prerequisites` (`map[string]func(ctx, beckn) (map[string]any, error)`,
  keyed by binding key) is whatever real I/O a mapping cannot do. `nil` or
  empty means every capability this step serves resolves nothing.
- `gather` (`NewWithFanOut` only) is the fan-out hook above.
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
