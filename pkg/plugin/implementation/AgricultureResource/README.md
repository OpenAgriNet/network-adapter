# Agriculture Resource Plugin

A **provider step plugin** that serves `openagrinet:AgricultureResource` by
calling an ordinary HTTP API that has never heard of Beckn.

Today that API is SATHI's certified seed stock lookup, which answers
`sathi|openagrinet:AgricultureResource` for the `select` action.

## What lives here

Almost nothing. Recognising a capability, resolving the call plan,
authenticating, calling with the registry's budget and translating in both
directions are all `internal/common`'s. This package owns its name and its
prerequisites, which are empty: SATHI's select carries every value the call
needs (crop code, LGD state and district codes).

Named for the schema pack rather than for SATHI: a second provider publishing
AgricultureResource would be another registry row and another mapping, not
another package.

## Configuration

```yaml
providerSteps:
  - id: AgricultureResource
    config:
      bindingKeys: "sathi|openagrinet:AgricultureResource"
      sathi:
        authScheme: body
        bodyField: apiKey
        bodyValueEnv: SATHI_API_KEY
        answerStatuses: "409"
```

The block under the participant id is flattened to `authScheme-sathi` and
friends before the plugin sees it, and read by `common.ParseStepConfig`.

### `authScheme: body`

SATHI accepts its key only as a field of the JSON request body; in a header or
the query string it answers 400. `body` sets `bodyField` from the environment
variable `bodyValueEnv` on the mapped body at call time, keeps every other
field, and redacts the value like the other schemes. It refuses an unset
variable, a body that is not a JSON object, and a method with no body.

### `answerStatuses`

SATHI answers "no stock" with HTTP 409 and a normal JSON body. Without this, the
step turns every non-2xx into a 502 and the mapping never sees it.
`answerStatuses` lists the 4xx codes a provider uses for a real answer; their
body goes to the response mapping like a 2xx. 2xx and 5xx are refused in the
list, so 5xx stays retryable.

## Registry rows

| Row | Value |
|---|---|
| Participant | `sathi`, with SATHI's base URL |
| Binding | `sathi\|openagrinet:AgricultureResource`, action `select`, `POST /inv-apis2/stock/getSeedAvailability` |
| Mapping | helmcharts `quick-start/config/mappings/sathi/agriculture-resource.select.yaml` |

The mapping copies SATHI's request fields (`cropCode`, `seedClass`, `stateCode`,
`districtCode`) from the select's `resourceAttributes` and returns SATHI's reply
in `on_select` `resourceAttributes`. The key never appears in the mapping or the
select.

## Testing

```sh
go test ./pkg/plugin/implementation/AgricultureResource/... \
        ./pkg/plugin/implementation/internal/common/ -run 'Body|Answer|AgricultureResource'
```

`internal/common/bodyauth_test.go` and `answerstatus_test.go` cover the two
features above; `cmd/plugin_test.go` covers reading SATHI's provider block.
