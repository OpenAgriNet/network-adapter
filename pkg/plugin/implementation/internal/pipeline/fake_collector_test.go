package pipeline

// fake_collector_test.go is the frame's stand-in for a capability.
//
// The frame must be testable without any real pipeline, or its tests would
// silently become tests of whichever capability they borrowed -- and the first
// thing a second capability breaks is an assumption nobody knew was mandi's.

import (
	"context"
	"embed"
	"testing"
)

//go:embed testdata/*.yaml
var fakeFiles embed.FS

const (
	fakeCapability   = "example:Thing"
	fakePipelinePath = "testdata/minimal.yaml"
	fakeRegistryPath = "pkg/plugin/implementation/Example/cataloguepublish-example/testdata/minimal.yaml"
)

// fakeCollector answers with whatever it is given, so a test states the one
// thing it is about and nothing else.
type fakeCollector struct {
	capability string
	files      Files

	result CollectResult
	err    error

	// calls counts Collect invocations, so a test can prove a run that should
	// not have collected did not.
	calls int
}

func newFakeCollector() *fakeCollector {
	return &fakeCollector{
		capability: fakeCapability,
		files:      Files{FS: fakeFiles, Path: fakePipelinePath, RegistryPath: fakeRegistryPath},
	}
}

func (f *fakeCollector) Capability() string { return f.capability }
func (f *fakeCollector) Pipeline() Files    { return f.files }

func (f *fakeCollector) Collect(context.Context, RunEnv) (CollectResult, error) {
	f.calls++
	return f.result, f.err
}

// assertNotCollected is the assertion most of the decision tests share: the
// run refused or stood down, so no upstream was touched.
func assertNotCollected(t *testing.T, collector *fakeCollector) {
	t.Helper()
	if collector.calls != 0 {
		t.Errorf("collected %d time(s); this run should not have reached the upstream at all", collector.calls)
	}
}
