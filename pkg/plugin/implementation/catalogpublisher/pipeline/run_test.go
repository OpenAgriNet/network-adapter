package pipeline

// run_test.go covers the parts of a run that must be decided BEFORE anything
// is fetched or published. The fetching itself is covered step by step
// elsewhere (client_test, catalog_test, publish_test) and against the real
// services in live_test; what is tested here is that a run which should not
// happen does not happen at all.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// publishingRecord is the live registry's answer, in the shape the gate reads.
func publishingRecord() *model.ProviderRecord {
	return &model.ProviderRecord{
		BindingKey: "exampleco|example:Thing",
		Actions: map[string]model.ActionPlan{
			"select":  {Method: "GET", Path: "/v1/fetch"},
			"publish": {Mappings: fakeRegistryPath},
		},
	}
}

// fakeRunLog records what a run asked of it, so a test can prove a run that
// did not happen also did not claim to have happened.
type fakeRunLog struct {
	last      time.Time
	readErr   error
	writeErr  error
	recorded  []time.Time
	readCalls int
}

func (f *fakeRunLog) LastPipelineRun(context.Context, string) (time.Time, error) {
	f.readCalls++
	return f.last, f.readErr
}

func (f *fakeRunLog) RecordPipelineRun(_ context.Context, _ string, at time.Time) error {
	f.recorded = append(f.recorded, at)
	return f.writeErr
}

// An upstream address that cannot be reached, so any test whose run reaches
// the fetching stage fails loudly there instead of quietly going to a real
// service.
func unreachableEnv(name string) (string, bool) {
	switch name {
	case "EXAMPLE_BASE_URL":
		// Reserved for documentation examples (RFC 2606) and resolvable by
		// nothing.
		return "http://upstream.invalid", true
	case "EXAMPLE_TOKEN_USER":
		return "test-user", true
	case "EXAMPLE_TOKEN_SECRET":
		return "test-secret", true
	case "EXAMPLE_PUBLISH_URL":
		return "http://publish.invalid", true
	}
	return "", false
}

// The whole reason the run log exists: a restart minutes after a run must not
// publish the day's catalogues a second time.
func TestRunSkipsWhenTheRunLogSaysItAlreadyRanThisFiring(t *testing.T) {
	collector := newFakeCollector()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	// Fired at 00:00 IST, ran at 00:01, restarted at 00:05.
	log := &fakeRunLog{last: time.Date(2026, 9, 21, 0, 1, 0, 0, ist)}

	report, err := Run(context.Background(), RunOptions{
		Record:    publishingRecord(),
		Collector: collector,
		RunLog:    log,
		Now:       time.Date(2026, 9, 21, 0, 5, 0, 0, ist),
		Lookup:    unreachableEnv,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Due {
		t.Fatal("ran again five minutes after running; this publishes twice")
	}
	if len(log.recorded) != 0 {
		t.Errorf("a run that did not happen recorded %d run(s)", len(log.recorded))
	}
	if report.Reason == "" {
		t.Error("no reason given for doing nothing")
	}
	assertNotCollected(t, collector)
}

// A capability the registry never sanctioned must fail before any work, and
// must not leave a run recorded against it.
func TestRunRefusesACapabilityThatDoesNotPublish(t *testing.T) {
	collector := newFakeCollector()
	log := &fakeRunLog{}
	_, err := Run(context.Background(), RunOptions{
		Collector: collector,
		Record: &model.ProviderRecord{
			BindingKey: "mausamgram|openagrinet:WeatherObservation",
			Actions:    map[string]model.ActionPlan{"select": {Method: "GET"}},
		},
		RunLog: log,
		Lookup: unreachableEnv,
	})
	if err == nil {
		t.Fatal("a select-only capability produced a run")
	}
	assertNotCollected(t, collector)
	if len(log.recorded) != 0 {
		t.Error("a refused run was recorded as having run")
	}
}

// If the run log cannot be read, whether today's run already happened is
// unknown. Running anyway is a guess, and the guess that publishes is the
// wrong one to make silently.
func TestRunRefusesWhenTheRunLogCannotBeRead(t *testing.T) {
	collector := newFakeCollector()
	log := &fakeRunLog{readErr: errors.New("database is down")}
	_, err := Run(context.Background(), RunOptions{
		Record:    publishingRecord(),
		Collector: collector,
		RunLog:    log,
		Lookup:    unreachableEnv,
	})
	if err == nil {
		t.Fatal("an unreadable run log was treated as 'never ran'")
	}
	assertNotCollected(t, collector)
	if len(log.recorded) != 0 {
		t.Error("a refused run was recorded as having run")
	}
}

// Without a run log there is nothing to consult, so every tick is due. That is
// the pre-persistence behaviour and it is a foot-gun, so the report has to say
// so rather than look identical to a persisted run.
func TestRunWithoutARunLogSaysSoInTheReport(t *testing.T) {
	collector := newFakeCollector()
	report, err := Run(context.Background(), RunOptions{
		Record:    publishingRecord(),
		Collector: collector,
		Lookup:    unreachableEnv,
		DryRun:    true,
		Publish:   false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Due {
		t.Fatalf("not due with no run log: %s", report.Reason)
	}
	if !report.Unpersisted {
		t.Error("report does not flag that this run's outcome is not persisted")
	}
}

// DryRun stops after the decision. It exists so an operator can ask "would
// this run, and with what" against production config without fetching
// anything.
func TestRunDryRunDoesNoWork(t *testing.T) {
	collector := newFakeCollector()
	log := &fakeRunLog{}
	report, err := Run(context.Background(), RunOptions{
		Record:    publishingRecord(),
		Collector: collector,
		RunLog:    log,
		Lookup:    unreachableEnv,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Due {
		t.Fatalf("not due: %s", report.Reason)
	}
	if report.PipelinePath == "" {
		t.Error("report does not say which pipeline it resolved")
	}
	// Nothing ran, so nothing may be recorded as having run.
	if len(log.recorded) != 0 {
		t.Errorf("a dry run recorded %d run(s)", len(log.recorded))
	}
	assertNotCollected(t, collector)
}

// A run that fails must NOT be recorded, or a transient upstream outage at
// midnight silently costs the whole day's publication.
func TestRunDoesNotRecordAFailedRun(t *testing.T) {
	collector := newFakeCollector()
	log := &fakeRunLog{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := Run(ctx, RunOptions{
		Record:    publishingRecord(),
		Collector: collector,
		RunLog:    log,
		Lookup:    unreachableEnv, // the upstream does not resolve
	})
	if err == nil {
		t.Fatal("a run against an unreachable upstream reported success")
	}
	if len(log.recorded) != 0 {
		t.Errorf("a failed run was recorded as having run; the day's run is now lost")
	}
}

// Missing credentials are a configuration error, not something to discover
// halfway through a fetch.
func TestRunRefusesIncompleteConfiguration(t *testing.T) {
	collector := newFakeCollector()
	_, err := Run(context.Background(), RunOptions{
		Record:    publishingRecord(),
		Collector: collector,
		Lookup:    func(string) (string, bool) { return "", false },
	})
	if err == nil {
		t.Fatal("a run with no upstream credentials was attempted")
	}
	assertNotCollected(t, collector)
}
