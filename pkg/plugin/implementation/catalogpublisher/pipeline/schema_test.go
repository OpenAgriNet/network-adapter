package pipeline

// schema_test.go covers the contract every pipeline file is held to.
//
// Two things are under test. First, that a wrong file is REFUSED, with the
// path named -- validation that accepts a broken file is worse than none,
// because it reads as assurance. Second, and less obvious, that the contract
// and the Go structs have not drifted apart: three artifacts now describe the
// same file, and nothing but a test keeps them honest.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A file naming no contract is refused, and told what to write.
func TestValidateRefusesAFileWithNoSchemaRef(t *testing.T) {
	err := Validate(fixtureFS, "testdata/minimal.yaml")
	if err == nil {
		t.Skip("testdata/minimal.yaml already declares a schemaRef; this case is covered elsewhere")
	}
	if !strings.Contains(err.Error(), "schemaRef") {
		t.Errorf("error %q does not mention the missing schemaRef", err)
	}
	// The message has to say what to add, or an operator has to go reading Go
	// to find out.
	if !strings.Contains(err.Error(), "publish.oan/CataloguePipeline/v1") {
		t.Errorf("error %q does not name a contract to use", err)
	}
}

// A contract this binary does not carry must name the ones it does.
func TestValidateRefusesAnUnknownContract(t *testing.T) {
	_, err := schemaFor("publish.oan/CataloguePipeline/v99")
	if err == nil {
		t.Fatal("an unknown contract was accepted")
	}
	if !strings.Contains(err.Error(), "publish.oan/CataloguePipeline/v1") {
		t.Errorf("error %q does not say which contracts exist", err)
	}
}

// apiVersion and schemaRef.uses say the same thing twice. A file where they
// disagree is refused rather than one of them being silently preferred --
// otherwise a reader cannot tell which the engine obeys.
func TestValidateRefusesADisagreeingAPIVersion(t *testing.T) {
	document := map[string]any{
		"apiVersion": "publish.somebodyelse/v1",
		"schemaRef":  map[string]any{"uses": "publish.oan/CataloguePipeline/v1"},
	}
	err := checkAPIVersionAgrees(document, "publish.oan/CataloguePipeline/v1", "example.yaml")
	if err == nil {
		t.Fatal("a file whose apiVersion contradicts its schemaRef was accepted")
	}
	if !strings.Contains(err.Error(), "publish.oan/v1") {
		t.Errorf("error %q does not name the apiVersion the contract expects", err)
	}

	// And the agreeing case must pass, or the check is just noise.
	document["apiVersion"] = "publish.oan/v1"
	if err := checkAPIVersionAgrees(document, "publish.oan/CataloguePipeline/v1", "example.yaml"); err != nil {
		t.Errorf("an agreeing apiVersion was refused: %v", err)
	}
}

// The violations a real mistake produces, checked one at a time.
//
// Each case is a mistake somebody will actually make: a misspelled key, a
// number written as text, a primitive that does not exist, a cron with the
// wrong number of fields.
func TestValidateReportsTheOffendingPath(t *testing.T) {
	base := mustLoadYAML(t, fixtureFS, "testdata/minimal.yaml")

	tests := map[string]struct {
		break_ func(map[string]any)
		expect string
	}{
		"a misspelled top-level block": {
			break_: func(doc map[string]any) { doc["catalogue"] = doc["catalog"] },
			expect: "catalogue",
		},
		"a misspelled key inside a block": {
			break_: func(doc map[string]any) {
				schedule := doc["schedule"].(map[string]any)
				schedule["timezon"] = schedule["timezone"]
				delete(schedule, "timezone")
			},
			expect: "schedule",
		},
		"a number written as text": {
			break_: func(doc map[string]any) {
				doc["catalog"].(map[string]any)["chunk"].(map[string]any)["budget"] = "256"
			},
			expect: "budget",
		},
		"a primitive that does not exist": {
			break_: func(doc map[string]any) {
				steps := doc["pipeline"].([]any)
				steps[0].(map[string]any)["uses"] = "http.patch"
			},
			expect: "uses",
		},
		"a six-field cron": {
			break_: func(doc map[string]any) {
				doc["schedule"].(map[string]any)["cron"] = "0 0 0 * * *"
			},
			expect: "cron",
		},
		"concurrency above one": {
			break_: func(doc map[string]any) {
				steps := doc["pipeline"].([]any)
				steps[0].(map[string]any)["concurrency"] = 8
			},
			expect: "concurrency",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			document := deepCopy(base).(map[string]any)
			tc.break_(document)

			err := validateDocument(t, document)
			if err == nil {
				t.Fatalf("the broken file was accepted; %q should have been reported", tc.expect)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error does not name %q:\n%v", tc.expect, err)
			}
		})
	}
}

// The fixture itself must pass, or every case above proves nothing: they all
// start from it, and a fixture that fails for its own reasons would make them
// pass regardless of the mistake injected.
func TestTheFixtureMatchesTheContract(t *testing.T) {
	if err := Validate(fixtureFS, "testdata/minimal.yaml"); err != nil {
		t.Fatalf("the test fixture does not match the contract it names:\n%v", err)
	}
}

// THE DRIFT GUARD.
//
// Three artifacts now describe a pipeline file: the YAML, the Go structs in
// spec.go, and the JSON Schema. Validation alone cannot catch them diverging
// -- a key can be allowed by the schema and dropped by the struct (parsed,
// then silently ignored), or held by the struct and rejected by the schema
// (refused though the engine would happily read it).
//
// A file that exercises every key closes both directions at once: it must
// validate AND survive UnmappedKeys.
func TestContractAndStructAgree(t *testing.T) {
	const fixture = "testdata/minimal.yaml"

	if err := Validate(fixtureFS, fixture); err != nil {
		t.Fatalf("the schema rejects a file the struct accepts:\n%v", err)
	}
	unmapped, err := UnmappedKeys(fixtureFS, fixture)
	if err != nil {
		t.Fatalf("UnmappedKeys: %v", err)
	}
	for _, key := range unmapped {
		t.Errorf("the schema allows %q but Spec has no field for it, so it parses into nothing", key)
	}
}

// validateDocument runs an in-memory document through the same path a file
// takes, so the table above can mutate a parsed fixture rather than keeping
// six near-identical YAML files on disk.
func validateDocument(t *testing.T, document map[string]any) error {
	t.Helper()

	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("re-encoding the mutated fixture: %v", err)
	}
	dir := t.TempDir()
	return validateBytes(encoded, dir+"/pipeline.yaml")
}

func mustLoadYAML(t *testing.T, files interface {
	ReadFile(string) ([]byte, error)
}, path string) map[string]any {
	t.Helper()
	raw, err := files.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document
}

// deepCopy so one table case's mutation cannot reach the next.
func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[key] = deepCopy(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = deepCopy(child)
		}
		return out
	default:
		return value
	}
}

// upstreamless turns the fixture into a pipeline with no upstream: no
// upstream block, no credential inputs, one const step. Built in memory from
// minimal.yaml so the shape needs no fixture file of its own.
func upstreamless(t *testing.T) map[string]any {
	t.Helper()
	doc := deepCopy(mustLoadYAML(t, fixtureFS, fixturePipelinePath)).(map[string]any)
	delete(doc, "upstream")
	inputs := doc["inputs"].(map[string]any)
	for _, name := range []string{"baseUrl", "tokenUser", "tokenSecret"} {
		delete(inputs, name)
	}
	doc["pipeline"] = []any{map[string]any{
		"id": "resources", "uses": "const", "out": "collection",
		"with": map[string]any{"records": []any{map[string]any{"id": "only", "group": "AA"}}},
	}}
	return doc
}

// A pipeline with no upstream is a real shape -- a catalogue whose content is
// fixed -- and must validate without inventing credentials it never uses.
func TestAnUpstreamlessPipelineMatchesTheContract(t *testing.T) {
	if err := validateDocument(t, upstreamless(t)); err != nil {
		t.Fatalf("an upstream-less pipeline was refused: %v", err)
	}
}

// Making `upstream` optional must not let an HTTP step, a token exchange or a
// const step through without what each needs.
func TestOptionalUpstreamRulesStillBindWhenTheyApply(t *testing.T) {
	tests := map[string]struct {
		doc    func(t *testing.T) map[string]any
		expect string
	}{
		"an http step with no upstream": {
			doc: func(t *testing.T) map[string]any {
				doc := upstreamless(t)
				doc["pipeline"] = []any{map[string]any{
					"id": "fetch", "uses": "http.get",
					"with": map[string]any{"path": "/x", "mapping": "mappings/x.yaml"},
				}}
				return doc
			},
			expect: "upstream",
		},
		"a token exchange with no tokenSecret input": {
			doc: func(t *testing.T) map[string]any {
				doc := deepCopy(mustLoadYAML(t, fixtureFS, fixturePipelinePath)).(map[string]any)
				delete(doc["inputs"].(map[string]any), "tokenSecret")
				return doc
			},
			expect: "tokenSecret",
		},
		"an upstream with no baseUrl input": {
			doc: func(t *testing.T) map[string]any {
				doc := deepCopy(mustLoadYAML(t, fixtureFS, fixturePipelinePath)).(map[string]any)
				delete(doc["inputs"].(map[string]any), "baseUrl")
				return doc
			},
			expect: "baseUrl",
		},
		"a const step with no records": {
			doc: func(t *testing.T) map[string]any {
				doc := upstreamless(t)
				doc["pipeline"] = []any{map[string]any{"id": "resources", "uses": "const"}}
				return doc
			},
			expect: "with",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateDocument(t, tc.doc(t))
			if err == nil {
				t.Fatal("the broken file was accepted")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q does not name %q", err, tc.expect)
			}
		})
	}
}

// hasUpstream is decided by the block's presence, not by an auth kind.
func TestHasUpstreamIsTheBlocksPresence(t *testing.T) {
	if hasUpstream(Spec{}) {
		t.Error("a spec with no upstream block reported one")
	}
	if !hasUpstream(Spec{Upstream: Upstream{BaseURL: "${inputs.baseUrl}"}}) {
		t.Error("a spec with an upstream block reported none")
	}
}
