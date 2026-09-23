package agmarket

// live_test.go drives each pipeline step against the REAL services, one step
// at a time, so a person can watch what each stage actually produces before
// the orchestrator exists to chain them.
//
// Every test here is skipped unless MANDI_LIVE=1, so `go test ./...` stays
// hermetic. The publishing step needs a second opt-in of its own
// (MANDI_LIVE_PUBLISH=1) because it is the only one with a side effect
// somebody else can see: it puts catalogues onto the network.
//
// These live in the package rather than a cmd/ because everything they
// exercise is unexported. That is also the reason this file exists at all --
// see dev_docs/plan-crawler-mandi/live-testing-guide.md.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/sunbirdRegistry"
)

// liveOrSkip gates a test on real credentials being present, and reports which
// variable is missing rather than skipping silently.
func liveOrSkip(t *testing.T) map[string]string {
	t.Helper()

	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1 to run against the real services")
	}

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	resolved, err := pipeline.ResolveInputs(spec, os.LookupEnv)
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}

	for _, name := range []string{"baseUrl", "tokenUser", "tokenSecret"} {
		if resolved[name] == "" {
			t.Fatalf("live test: input %q is empty; source the repo's .env first", name)
		}
	}

	// Print what the run resolved to, redacted -- this is the first thing
	// worth seeing, because a wrong baseUrl or date explains most later
	// surprises on its own.
	t.Logf("resolved inputs: %v", pipeline.RedactedInputs(spec.Inputs, resolved))
	return resolved
}

// TestLive_0_RegistryLookup exercises the lookup the design's registry gate
// depends on, against the real Sunbird RC, using the same plugin the adapter
// would use -- not a curl approximation of it.
//
// Nothing in this package calls the registry yet (mandi-pipeline-document.md
// §4.2-4.3 describes the gate; it is unbuilt). This test exists to prove the
// path works and to show exactly what the registry does and does not hold
// today, because that determines whether the gate could pass at all.
func TestLive_0_RegistryLookup(t *testing.T) {
	resolved := liveOrSkip(t)
	record := liveProviderRecord(t, resolved)

	t.Logf("resolved: participant=%s capability=%s baseUrl=%s",
		record.ParticipantID, record.CapabilityCode, record.BaseURL)
	for action, plan := range record.Actions {
		t.Logf("  action %q -> %s %s (mappings: %s)", action, plan.Method, plan.Path, plan.Mappings)
	}
	if _, ok := record.Actions["publish"]; !ok {
		t.Logf("  NO \"publish\" action: the crawler would find no pipeline to run for this capability")
	}
}

// liveProviderRecord looks this pipeline's own bindingKey up in the real
// Sunbird RC, using the same plugin the adapter would use.
func liveProviderRecord(t *testing.T, resolved map[string]string) *model.ProviderRecord {
	t.Helper()

	// The declared default is a container-network name; from a shell it has
	// to be localhost. Override with SUNBIRD_REGISTRY_URL.
	registryURL := resolved["registryUrl"]
	if override := os.Getenv("SUNBIRD_REGISTRY_URL"); override != "" {
		registryURL = override
	}
	t.Logf("registry: %s", registryURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, closer, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL:            registryURL,
		Entity:         "Participant",
		ProviderEntity: "ProviderSchema",
		Timeout:        10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closer() }()

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}

	// The bindingKey the pipeline's own inputs produce. This is what the gate
	// looks up -- not a hand-typed one, or the test would prove nothing about
	// what a real run would ask for.
	bindingKey := resolved["participantId"] + "|" + spec.Metadata.Capability
	t.Logf("bindingKey from this pipeline's inputs: %s", bindingKey)

	record, err := client.ProviderRecord(ctx, bindingKey)
	if err != nil {
		t.Logf("NOT FOUND: %v", err)
		t.Logf("  the registry gate would REFUSE this run")
		override := os.Getenv("MANDI_LIVE_BINDING_KEY")
		if override == "" {
			t.Skip("set MANDI_LIVE_BINDING_KEY to a binding the registry actually holds to see a record")
		}
		t.Logf("  retrying with MANDI_LIVE_BINDING_KEY=%s", override)
		if record, err = client.ProviderRecord(ctx, override); err != nil {
			t.Fatalf("ProviderRecord(%s): %v", override, err)
		}
	}
	return record
}

// TestLive_8_Tick runs the pre-flight a scheduled tick runs, end to end and
// against the real registry: look the capability up, let the gate find the
// publish action, load the pipeline that action names, and ask the schedule
// whether now is the time.
//
// It stops there deliberately. Everything after this decision is steps 1-7,
// which the earlier tests already cover one at a time; what has never been
// exercised against the live registry is the chain that decides to run them.
func TestLive_8_Tick(t *testing.T) {
	resolved := liveOrSkip(t)
	record := liveProviderRecord(t, resolved)

	pipelinePath, err := pipeline.PipelinePathFor(record)
	if err != nil {
		t.Fatalf("registry gate refused the live record: %v", err)
	}
	t.Logf("gate: the registry sanctions pipeline %s", pipelinePath)

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	t.Logf("pipeline: %s (capability %s, provider %s)",
		spec.Metadata.Name, spec.Metadata.Capability, spec.Metadata.Provider)
	t.Logf("schedule: cron %q %s", spec.Schedule.Cron, spec.Schedule.Timezone)

	// DryRun is the real entry point, stopped before it fetches: it runs the
	// gate, the run log and the schedule exactly as a scheduled tick would.
	now := time.Now()
	decision, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: Pipeline(),
		Record:   record,
		Now:      now,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("tick against the live record: %v", err)
	}
	t.Logf("decision at %s: due=%v (%s)", now.Format(time.RFC3339), decision.Due, decision.Reason)

	// Never-run is the state a fresh deployment is in, and this pipeline
	// fires at 00:00 -- so at any wall-clock time today the answer must be
	// "due". A false here means a tick would never fire at all.
	if !decision.Due {
		t.Errorf("a never-run pipeline is not due: %s", decision.Reason)
	}

	// Same record, with a run log saying it just ran: the tick must go quiet
	// rather than publishing a second time the same day.
	repeat, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: Pipeline(),
		Record:   record,
		RunLog:   &fakeRunLog{last: now},
		Now:      now,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("tick after a run: %v", err)
	}
	t.Logf("decision having just run: due=%v (%s)", repeat.Due, repeat.Reason)
	if repeat.Due {
		t.Error("due again immediately after running; this would publish on every tick")
	}
}

// TestLive_9_BuildAgainstLiveUpstream runs the WHOLE pipeline against the real
// Agmarknet, exactly as a scheduled tick would, and stops before publishing.
//
// This is the test that proves the YAML is the program: nothing in Go names an
// Agmarknet path, a mapping file or a market any more. If the file's steps,
// its coordinate rules or its chunking were wrong, this is where it shows.
func TestLive_9_BuildAgainstLiveUpstream(t *testing.T) {
	resolved := liveOrSkip(t)
	record := liveProviderRecord(t, resolved)

	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	report, err := pipeline.Run(ctx, pipeline.RunOptions{
		Pipeline: Pipeline(),
		Record:   record,
		Now:      time.Now(),
		OutDir:   outDir,
		// Publish deliberately left false: this reads from the upstream and
		// writes to disk, and nothing else.
	})
	if err != nil {
		t.Fatalf("the pipeline failed against the live upstream: %v", err)
	}
	if !report.Due {
		t.Skipf("not due, so nothing ran: %s", report.Reason)
	}

	t.Logf("catalogues: %d in %s", len(report.Catalogues), report.OutDir)
	for name, count := range report.Counters {
		t.Logf("  %-34s %d", name, count)
	}
	for i, catalogue := range report.Catalogues {
		if i == 5 {
			t.Logf("  ... and %d more", len(report.Catalogues)-5)
			break
		}
		t.Logf("  %-10s %s (%d bytes)", catalogue.Slug, catalogue.CatalogID, len(catalogue.Content))
	}

	if len(report.Catalogues) == 0 {
		t.Error("the live upstream produced no catalogues at all")
	}
	if report.Errors > 0 {
		t.Errorf("%d parts of the collection failed; publishing would be refused", report.Errors)
	}
}
