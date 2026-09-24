package agmarket

// spec_conformance_test.go holds this pipeline's side of the YAML<->struct
// contract: that mandi-price-agmarket.yaml really does parse into the values
// the code then relies on, and that the file is not saying anything the schema
// cannot hear.
//
// The schema itself, and the machinery that checks it, live in the frame
// (internal/pipeline). What cannot live there is THIS file -- the frame has no
// pipeline YAML of its own, and a fixture in the frame would happily drift
// away from the real thing. So every pipeline asserts against its own file,
// and this is mandi's.

import (
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// TestTheRealPipelineFileDropsNoKeys is the guard that has already caught a
// real bug: Step was missing eight keys -- when, else, forEach, as,
// concurrency, onError, onEmptyOutput, failWhenEmpty -- so the conditional,
// the deliberate sequentiality and the "no rows is not a failure"
// classification all parsed into nothing while the file looked obeyed.
func TestTheRealPipelineFileDropsNoKeys(t *testing.T) {
	unmapped, err := pipeline.UnmappedKeys(Files, PipelinePath)
	if err != nil {
		t.Fatalf("UnmappedKeys: %v", err)
	}
	for _, key := range unmapped {
		t.Errorf("%s declares %q, but the Spec schema has no field for it -- "+
			"yaml.v3 is silently dropping it", PipelinePath, key)
	}
}

func TestTheRealPipelineFileLoads(t *testing.T) {
	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec(%q): %v", PipelinePath, err)
	}

	if got, want := spec.Metadata.Capability, "openagrinet:MandiPrice"; got != want {
		t.Errorf("Metadata.Capability = %q, want %q", got, want)
	}
	if got, want := spec.Metadata.Provider, "agmarknet"; got != want {
		t.Errorf("Metadata.Provider = %q, want %q", got, want)
	}

	if got, want := spec.Schedule.Cron, "0 0 * * *"; got != want {
		t.Errorf("Schedule.Cron = %q, want %q", got, want)
	}
	if got, want := spec.Schedule.Timezone, "Asia/Kolkata"; got != want {
		t.Errorf("Schedule.Timezone = %q, want %q", got, want)
	}

	wantInputs := map[string]pipeline.Input{
		// No Default: a registry hostname baked into the pipeline is wrong for
		// every deployment that did not happen to use it.
		"registryUrl": {
			Flag: "registry-url",
			Env:  "SUNBIRD_REGISTRY_URL",
		},
		"baseUrl": {
			Flag:    "base-url",
			Env:     "MANDI_API_URI",
			Default: "http://34.0.4.235:8080",
		},
		"states": {
			Flag:    "states",
			Type:    "list",
			Default: []interface{}{},
		},
		"fromDate": {
			Flag:    "from",
			Env:     "MANDI_FROM_DATE",
			Type:    "date",
			Format:  "dd-MM-yyyy",
			Default: "today",
		},
		"toDate": {
			Flag:    "to",
			Env:     "MANDI_TO_DATE",
			Type:    "date",
			Format:  "dd-MM-yyyy",
			Default: "today",
		},
		"participantId": {
			Flag:    "participant-id",
			Env:     "MANDI_PARTICIPANT_ID",
			Default: "agmarknet",
		},
		"networkId": {
			Flag:    "network-id",
			Env:     "APP_NETWORK_ID",
			Default: "oan-dev",
		},
		"catalogOut": {
			Flag:    "catalog-out",
			Default: "catalog",
		},
		"withoutGeometry": {
			Flag:    "without-geometry",
			Enum:    []string{"publish", "skip"},
			Default: "publish",
		},
		// A one-time migration switch. It must be DECLARED, because the
		// publish step refuses an enable flag it cannot resolve rather than
		// reading it as "off" -- which is how a retirement an operator asked
		// for would be silently skipped.
		"retireOld": {
			Flag:    "retire-old",
			Env:     "MANDI_RETIRE_OLD",
			Default: false,
		},
		"publishUrl": {
			Flag: "publish-url",
			Env:  "CATALOG_PUBLISH_URL",
		},
		"tokenUser": {
			Env:    "MANDI_TOKEN_USER",
			Secret: true,
		},
		"tokenSecret": {
			Env:    "MANDI_TOKEN_SECRET",
			Secret: true,
		},
	}

	if got, want := len(spec.Inputs), len(wantInputs); got != want {
		t.Errorf("len(Inputs) = %d, want %d", got, want)
	}
	for name, want := range wantInputs {
		got, ok := spec.Inputs[name]
		if !ok {
			t.Errorf("Inputs[%q] missing", name)
			continue
		}
		if got.Flag != want.Flag {
			t.Errorf("Inputs[%q].Flag = %q, want %q", name, got.Flag, want.Flag)
		}
		if got.Env != want.Env {
			t.Errorf("Inputs[%q].Env = %q, want %q", name, got.Env, want.Env)
		}
		if got.Type != want.Type {
			t.Errorf("Inputs[%q].Type = %q, want %q", name, got.Type, want.Type)
		}
		if got.Format != want.Format {
			t.Errorf("Inputs[%q].Format = %q, want %q", name, got.Format, want.Format)
		}
		if got.Secret != want.Secret {
			t.Errorf("Inputs[%q].Secret = %v, want %v", name, got.Secret, want.Secret)
		}
		if want.Default != nil {
			gotSlice, gotIsSlice := got.Default.([]interface{})
			wantSlice, wantIsSlice := want.Default.([]interface{})
			if wantIsSlice {
				if !gotIsSlice || len(gotSlice) != len(wantSlice) {
					t.Errorf("Inputs[%q].Default = %#v, want %#v", name, got.Default, want.Default)
				}
			} else if got.Default != want.Default {
				t.Errorf("Inputs[%q].Default = %#v, want %#v", name, got.Default, want.Default)
			}
		}
		if len(want.Enum) != 0 {
			if len(got.Enum) != len(want.Enum) {
				t.Errorf("Inputs[%q].Enum = %#v, want %#v", name, got.Enum, want.Enum)
			} else {
				for i := range want.Enum {
					if got.Enum[i] != want.Enum[i] {
						t.Errorf("Inputs[%q].Enum = %#v, want %#v", name, got.Enum, want.Enum)
					}
				}
			}
		}
	}

	if got, want := spec.Upstream.BaseURL, "${inputs.baseUrl}"; got != want {
		t.Errorf("Upstream.BaseURL = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Kind, "tokenExchange"; got != want {
		t.Errorf("Upstream.Auth.Kind = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Request.Method, "POST"; got != want {
		t.Errorf("Upstream.Auth.Request.Method = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Request.Path, "/v1/generate-dynamic-token-agmarknet"; got != want {
		t.Errorf("Upstream.Auth.Request.Path = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Request.Body["access_name"], "${inputs.tokenUser}"; got != want {
		t.Errorf("Upstream.Auth.Request.Body[access_name] = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Request.Body["password"], "${inputs.tokenSecret}"; got != want {
		t.Errorf("Upstream.Auth.Request.Body[password] = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Token.At, "$.token"; got != want {
		t.Errorf("Upstream.Auth.Token.At = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Token.CarriedAs, "query"; got != want {
		t.Errorf("Upstream.Auth.Token.CarriedAs = %q, want %q", got, want)
	}
	if got, want := spec.Upstream.Auth.Token.Name, "token"; got != want {
		t.Errorf("Upstream.Auth.Token.Name = %q, want %q", got, want)
	}
	if got, want := len(spec.Upstream.Auth.Token.ReexchangeOn), 2; got != want {
		t.Fatalf("len(Upstream.Auth.Token.ReexchangeOn) = %d, want %d", got, want)
	}
	if got, want := spec.Upstream.Auth.Token.ReexchangeOn[0], 401; got != want {
		t.Errorf("Upstream.Auth.Token.ReexchangeOn[0] = %d, want %d", got, want)
	}
	if got, want := spec.Upstream.Auth.Token.ReexchangeOn[1], 403; got != want {
		t.Errorf("Upstream.Auth.Token.ReexchangeOn[1] = %d, want %d", got, want)
	}

	wantStepIDs := []string{"states", "masterMarkets", "stateRows", "join", "coordinates", "dedupe"}
	if got, want := len(spec.Pipeline), len(wantStepIDs); got != want {
		t.Fatalf("len(Pipeline) = %d, want %d", got, want)
	}
	for i, id := range wantStepIDs {
		if got := spec.Pipeline[i].ID; got != id {
			t.Errorf("Pipeline[%d].ID = %q, want %q", i, got, id)
		}
	}

	states := spec.Pipeline[0]
	if got, want := states.Uses, "http.get"; got != want {
		t.Errorf("states.Uses = %q, want %q", got, want)
	}
	if got, want := states.With.Path, "/v1/fetch-agmarknet-master-data"; got != want {
		t.Errorf("states.With.Path = %q, want %q", got, want)
	}
	if got, want := states.With.Mapping, "mappings/master-states.yaml"; got != want {
		t.Errorf("states.With.Mapping = %q, want %q", got, want)
	}

	masterMarkets := spec.Pipeline[1]
	if got, want := masterMarkets.Uses, "http.get"; got != want {
		t.Errorf("masterMarkets.Uses = %q, want %q", got, want)
	}
	if got, want := masterMarkets.With.Mapping, "mappings/master-markets.yaml"; got != want {
		t.Errorf("masterMarkets.With.Mapping = %q, want %q", got, want)
	}

	stateRows := spec.Pipeline[2]
	if got, want := stateRows.Uses, "http.get"; got != want {
		t.Errorf("stateRows.Uses = %q, want %q", got, want)
	}
	if got, want := stateRows.With.Mapping, "mappings/market-commodity.yaml"; got != want {
		t.Errorf("stateRows.With.Mapping = %q, want %q", got, want)
	}

	join := spec.Pipeline[3]
	if got, want := join.Uses, "join"; got != want {
		t.Errorf("join.Uses = %q, want %q", got, want)
	}
	if got, want := join.With.On, "marketId"; got != want {
		t.Errorf("join.With.On = %q, want %q", got, want)
	}
	if got, want := len(join.With.Carry), 2; got != want {
		t.Fatalf("len(join.With.Carry) = %d, want %d", got, want)
	}
	if got, want := join.With.Carry[0], "latitude"; got != want {
		t.Errorf("join.With.Carry[0] = %q, want %q", got, want)
	}
	if got, want := join.With.Carry[1], "longitude"; got != want {
		t.Errorf("join.With.Carry[1] = %q, want %q", got, want)
	}

	// The coordinate step is a correctness rule, not a classification: it
	// marks whether a point can be believed and CLEARS the ones that cannot,
	// so a market whose location is unknown is published without a location
	// rather than with a wrong one.
	coordinates := spec.Pipeline[4]
	if got, want := coordinates.Uses, "derive"; got != want {
		t.Errorf("coordinates.Uses = %q, want %q", got, want)
	}
	if got, want := coordinates.With.Field, "hasUsableCoordinate"; got != want {
		t.Errorf("coordinates.With.Field = %q, want %q", got, want)
	}
	// The `then: clear:` is the whole point -- without it a suspect
	// coordinate survives and is published as though it were good.
	if len(coordinates.With.Then) == 0 {
		t.Error("the coordinate step clears nothing; an unusable point would still be published")
	}

	dedupe := spec.Pipeline[5]
	if got, want := dedupe.Uses, "dedupe"; got != want {
		t.Errorf("dedupe.Uses = %q, want %q", got, want)
	}
	if got, want := dedupe.With.Key, "marketId"; got != want {
		t.Errorf("dedupe.With.Key = %q, want %q", got, want)
	}

	if got, want := spec.Catalog.GroupBy, "stateCode"; got != want {
		t.Errorf("Catalog.GroupBy = %q, want %q", got, want)
	}
	if got, want := spec.Catalog.Chunk.Budget, 256; got != want {
		t.Errorf("Catalog.Chunk.Budget = %d, want %d", got, want)
	}
	if got, want := spec.Catalog.Render.Mapping, "mappings/catalog.yaml"; got != want {
		t.Errorf("Catalog.Render.Mapping = %q, want %q", got, want)
	}

	if got, want := spec.Publish.URL, "${inputs.publishUrl}/publish"; got != want {
		t.Errorf("Publish.URL = %q, want %q", got, want)
	}
	if got, want := len(spec.Publish.Accept), 1; got != want {
		t.Fatalf("len(Publish.Accept) = %d, want %d", got, want)
	}
	if got, want := spec.Publish.Accept[0], "ACCEPTED"; got != want {
		t.Errorf("Publish.Accept[0] = %q, want %q", got, want)
	}
}

// lookupFrom makes an os.LookupEnv-shaped function out of a map, so a test
// states the environment it means instead of mutating the process's.
func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

// The real file, not a fixture: this is what catches an input being renamed or
// dropped in the YAML while code still reads the old name.
func TestResolveInputsAgainstRealSpec(t *testing.T) {
	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec(%q): %v", PipelinePath, err)
	}

	got, err := pipeline.ResolveInputs(spec, lookupFrom(map[string]string{
		"CATALOG_PUBLISH_URL":  "http://publish.test/catalog",
		"MANDI_TOKEN_USER":     "user",
		"MANDI_TOKEN_SECRET":   "secret",
		"MANDI_API_URI":        "http://upstream.test:8080",
		"MANDI_PARTICIPANT_ID": "",
	}))
	if err != nil {
		t.Fatalf("resolveInputs against the real spec: %v", err)
	}

	for _, key := range []string{
		"registryUrl", "baseUrl", "states", "fromDate", "toDate",
		"participantId", "networkId", "catalogOut", "withoutGeometry",
		"publishUrl", "tokenUser", "tokenSecret",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s declares input %q but it is missing from the resolved map", PipelinePath, key)
		}
	}

	for key, want := range map[string]string{
		"publishUrl":    "http://publish.test/catalog", // env only, no default
		"tokenSecret":   "secret",                      // secret, env only
		"baseUrl":       "http://upstream.test:8080",   // env beats default
		"participantId": "agmarknet",                   // empty env falls back
		// registryUrl has NO default on purpose -- see the file's own comment.
		// It must resolve to empty here, so a deployment that forgot to set it
		// is told that rather than sent to a hostname that resolves nowhere.
		"registryUrl":     "",
		"withoutGeometry": "publish", // enum default
		"states":          "",        // `default: []` means all states
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}
