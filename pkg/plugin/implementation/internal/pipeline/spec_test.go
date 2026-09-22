package pipeline

// spec_test.go proves LoadSpec reads each block of a pipeline file into the
// shape the frame relies on, and that UnmappedKeys reports what the schema
// cannot hold.
//
// These run against fixtures in testdata/, not against any capability's real
// file. The frame has no pipeline of its own, and the assertion that matters
// about a real file -- that ITS keys all reach code -- belongs with that file,
// in the capability's own package (see spec_conformance_test.go there).

import (
	"embed"
	"testing"
)

//go:embed testdata/*.yaml
var testFiles embed.FS

func TestLoadSpecReadsEveryBlock(t *testing.T) {
	spec, err := LoadSpec(testFiles, "testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}

	if got, want := spec.Metadata.Capability, "example:Thing"; got != want {
		t.Errorf("Metadata.Capability = %q, want %q", got, want)
	}
	if got, want := spec.Metadata.Provider, "exampleco"; got != want {
		t.Errorf("Metadata.Provider = %q, want %q", got, want)
	}
	if got, want := spec.Schedule.Cron, "0 0 * * *"; got != want {
		t.Errorf("Schedule.Cron = %q, want %q", got, want)
	}
	if got, want := spec.Schedule.Timezone, "Asia/Kolkata"; got != want {
		t.Errorf("Schedule.Timezone = %q, want %q", got, want)
	}

	input, ok := spec.Inputs["baseUrl"]
	if !ok {
		t.Fatal("Inputs has no baseUrl")
	}
	if input.Flag != "base-url" || input.Env != "EXAMPLE_BASE_URL" || input.Default != "http://example.test" {
		t.Errorf("Inputs[baseUrl] = %+v", input)
	}

	if got, want := spec.Upstream.Auth.Token.At, "$.token"; got != want {
		t.Errorf("Upstream.Auth.Token.At = %q, want %q", got, want)
	}
	if len(spec.Pipeline) != 1 || spec.Pipeline[0].ID != "things" {
		t.Errorf("Pipeline = %+v, want one step named things", spec.Pipeline)
	}
	// Step.When is one of the eight keys that once parsed into nothing.
	if got, want := spec.Pipeline[0].When, "always"; got != want {
		t.Errorf("Pipeline[0].When = %q, want %q", got, want)
	}
	if len(spec.Publish.Accept) != 1 || spec.Publish.Accept[0] != "ACCEPTED" {
		t.Errorf("Publish.Accept = %v, want [ACCEPTED]", spec.Publish.Accept)
	}
}

// A file whose every key the schema holds must report nothing -- otherwise the
// check cries wolf and gets ignored on the day it matters.
func TestUnmappedKeysIsSilentOnACompleteFile(t *testing.T) {
	unmapped, err := UnmappedKeys(testFiles, "testdata/minimal.yaml")
	if err != nil {
		t.Fatalf("UnmappedKeys: %v", err)
	}
	if len(unmapped) != 0 {
		t.Errorf("UnmappedKeys = %v, want none", unmapped)
	}
}

// The failure this whole mechanism exists for: LoadSpec SUCCEEDS on a file
// carrying keys the schema cannot hold, at the top level and nested inside a
// step, and only UnmappedKeys can tell you.
func TestUnmappedKeysFindsWhatTheSchemaCannotHold(t *testing.T) {
	if _, err := LoadSpec(testFiles, "testdata/unknown-key.yaml"); err != nil {
		t.Fatalf("LoadSpec refused the file; the silent-drop failure it guards "+
			"depends on parsing succeeding: %v", err)
	}

	unmapped, err := UnmappedKeys(testFiles, "testdata/unknown-key.yaml")
	if err != nil {
		t.Fatalf("UnmappedKeys: %v", err)
	}

	found := make(map[string]bool, len(unmapped))
	for _, key := range unmapped {
		found[key] = true
	}
	for _, want := range []string{
		"thisBlockIsNotInTheSchema",        // a whole top-level block
		"pipeline.[0].aKeyStepDoesNotHave", // one key inside a sequence entry
	} {
		if !found[want] {
			t.Errorf("UnmappedKeys did not report %q; got %v", want, unmapped)
		}
	}
}

func TestLoadSpecMissingFile(t *testing.T) {
	if _, err := LoadSpec(testFiles, "does-not-exist.yaml"); err == nil {
		t.Fatal("LoadSpec(\"does-not-exist.yaml\") returned nil error, want non-nil")
	}
}
