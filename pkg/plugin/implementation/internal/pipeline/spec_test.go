package agmarket

// spec_test.go proves LoadSpec actually parses mandi-price-agmarket.yaml into
// the shape the (not-yet-built) polling layer needs, per plan-1.md: metadata
// for registry lookup, the schedule cadence, every input the CLI/env surface
// declares, the upstream auth exchange, the pipeline steps in file order, and
// the catalog/publish blocks that turn a collection into published catalogs.
// It is not exercising business logic -- collect.go/build.go's ported logic
// is proven by steps_test.go -- this only guards the YAML<->struct mapping
// against drifting from the file it is meant to describe.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLoadSpecCapturesEveryTopLevelBlock guards the failure this struct is
// most exposed to: yaml.v3 drops unknown keys silently, so a block present in
// the file but absent from Spec parses "successfully" while vanishing. A rule
// written in the file -- an exclusion, a refusal, a catalogId template -- can
// then be believed to be in force while no code ever sees it. This compares
// the file's own top-level keys against the ones Spec declares.
func TestLoadSpecCapturesEveryTopLevelBlock(t *testing.T) {
	raw, err := Files.ReadFile(PipelinePath)
	if err != nil {
		t.Fatalf("read %s: %v", PipelinePath, err)
	}

	var asMap map[string]any
	if err := yaml.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("parse %s as a bare map: %v", PipelinePath, err)
	}

	// Round-tripping Spec back to a map yields exactly the keys Spec knows
	// how to hold, which is what the file's keys must be a subset of.
	spec, err := LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	encoded, err := yaml.Marshal(spec)
	if err != nil {
		t.Fatalf("re-marshal Spec: %v", err)
	}
	var known map[string]any
	if err := yaml.Unmarshal(encoded, &known); err != nil {
		t.Fatalf("parse re-marshalled Spec: %v", err)
	}

	for key := range asMap {
		if _, ok := known[key]; !ok {
			t.Errorf("%s declares top-level block %q, but Spec has no field for it -- "+
				"yaml.v3 is silently dropping it", PipelinePath, key)
		}
	}
}

func TestLoadSpec(t *testing.T) {
	spec, err := LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec(%q): %v", PipelinePath, err)
	}

	if got, want := spec.Metadata.Capability, "openagrinet:MandiPrice"; got != want {
		t.Errorf("Metadata.Capability = %q, want %q", got, want)
	}
	if got, want := spec.Metadata.Provider, "agmarknet"; got != want {
		t.Errorf("Metadata.Provider = %q, want %q", got, want)
	}

	if got, want := spec.Schedule.At, "00:00"; got != want {
		t.Errorf("Schedule.At = %q, want %q", got, want)
	}
	if got, want := spec.Schedule.Timezone, "Asia/Kolkata"; got != want {
		t.Errorf("Schedule.Timezone = %q, want %q", got, want)
	}

	wantInputs := map[string]Input{
		"registryUrl": {
			Flag:    "registry-url",
			Env:     "SUNBIRD_REGISTRY_URL",
			Default: "http://registry:8081/api/v1",
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
			Type:    "date",
			Format:  "dd-MM-yyyy",
			Default: "today",
		},
		"toDate": {
			Flag:    "to",
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
		"publishUrl": {
			Flag: "publish-url",
			Env:  "MANDI_PUBLISH_URL",
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

	wantStepIDs := []string{"states", "masterMarkets", "stateRows", "join", "quality", "dedupe"}
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

	quality := spec.Pipeline[4]
	if got, want := quality.Uses, "derive"; got != want {
		t.Errorf("quality.Uses = %q, want %q", got, want)
	}
	if got, want := quality.With.Field, "coordinateQuality"; got != want {
		t.Errorf("quality.With.Field = %q, want %q", got, want)
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

func TestLoadSpecMissingFile(t *testing.T) {
	if _, err := LoadSpec(Files, "does-not-exist.yaml"); err == nil {
		t.Fatal("LoadSpec(\"does-not-exist.yaml\") returned nil error, want non-nil")
	}
}
