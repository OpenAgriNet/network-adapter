package AgricultureFacility_test

// fanout_test.go covers the concurrency POLICY this package owns: how many
// fan-out calls run at once, in what order, and what happens when one fails.
// mappings_test.go already covers that fan-out happens at all and produces
// the right facilities (TestATwoTypeSearchIsAnsweredWithBothTypes and
// neighbours) and that each call carries its own identity
// (TestEachFanOutCallCarriesItsOwnRequestId) -- this file is what moved out
// of internal/upstream/upstream_test.go when fan-out concurrency moved into
// this package's own fanout.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/AgricultureFacility"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// runFanOutTimed is runFanOut's sibling: it hands the caller a raw HTTP
// handler (so a test can measure timing or count in-flight calls) and an
// explicit Config (so a test can set FanOutConcurrency), rather than a
// canned per-category response map.
func runFanOutTimed(t *testing.T, types []string, cfg *AgricultureFacility.Config, handler http.HandlerFunc) error {
	t.Helper()

	mappings := serveMappings(t)
	defer mappings.Close()

	upstream := httptest.NewServer(handler)
	defer upstream.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey: shippedBindingKey, BaseURL: upstream.URL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodPost, Path: "/search",
				Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000},
		},
	}}

	cfg.BindingKeys = []string{shippedBindingKey}
	step, closeStep, err := AgricultureFacility.New(context.Background(), registry, mapper, cfg)
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	defer closeStep()

	var payload map[string]any
	if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	attributes(t, payload)["supportedFacilityTypes"] = toAny(types)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("could not rebuild the payload: %v", err)
	}

	return step.Run(&model.StepContext{Context: t.Context(), Body: body})
}

// Fan-out calls are sequential unless the deployment raised the limit.
//
// The default is not a performance choice. A provider that answers one
// question at a time is often not built to be asked several at once, and
// POCRA's failure mode when pushed is a 200 with an empty catalog --
// indistinguishable from having no results, so the loss is silent. Parallel
// is opt-in per deployment for that reason, and this pins the default so it
// cannot drift.
func TestFanOutCallsAreSequentialByDefault(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var inFlight, peak int
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		// Long enough that overlapping calls would be caught. Without a wait
		// each call could finish before the next begins and a parallel step
		// would look sequential.
		time.Sleep(40 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		fmt.Fprint(w, providerResponse)
	}

	types := []string{"KrishiVigyanKendra", "CustomHiringCentre", "Warehouse"}
	if err := runFanOutTimed(t, types, &AgricultureFacility.Config{}, handler); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Errorf("%d fan-out calls were in flight at once, want 1 by default", peak)
	}
}

// A deployment that raises the limit gets calls in parallel, bounded by it.
func TestFanOutHonoursTheConfiguredConcurrency(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var inFlight, peak int
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		fmt.Fprint(w, providerResponse)
	}

	types := []string{"KrishiVigyanKendra", "CustomHiringCentre", "Warehouse", "SoilTestingFacility"}
	cfg := &AgricultureFacility.Config{FanOutConcurrency: 2}
	if err := runFanOutTimed(t, types, cfg, handler); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Errorf("peak concurrency was %d, want the configured 2", peak)
	}
}

// A call not yet issued when an earlier one fails is skipped, not made and
// discarded. Sequential (the default) makes this deterministic: the first
// value's failure cancels the shared context before the second value's call
// is ever issued.
func TestFanOutSkipsCallsNotYetIssuedAfterAFailure(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var calls int
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, "no", http.StatusInternalServerError)
	}

	types := []string{"KrishiVigyanKendra", "CustomHiringCentre", "Warehouse"}
	err := runFanOutTimed(t, types, &AgricultureFacility.Config{}, handler)
	if err == nil {
		t.Fatal("Run() served a partial answer, want it refused")
	}

	mu.Lock()
	defer mu.Unlock()
	// Sequential (limit 1): the first value fails and cancels the shared
	// context before the loop ever reaches the second or third value's call.
	if calls != 1 {
		t.Errorf("the provider was called %d times, want 1 -- later fan-out values "+
			"must be skipped once an earlier one fails", calls)
	}
}

// A payload asking for more calls than the ceiling is refused, not clamped.
//
// Uses a repeated governed type to exceed MaxFanOut, which the mapping's own
// duplicate-type required check ALSO refuses -- both guards agree the
// payload is bad, so the assertion that matters is that the provider was
// never called, not which guard fired first.
func TestFanOutRefusesOverTheCeiling(t *testing.T) {
	t.Parallel()

	handler := func(w http.ResponseWriter, r *http.Request) {
		t.Error("the provider was called for a fan-out that should have been refused")
	}

	types := make([]string, AgricultureFacility.MaxFanOut+1)
	for i := range types {
		types[i] = "KrishiVigyanKendra"
	}
	err := runFanOutTimed(t, types, &AgricultureFacility.Config{}, handler)
	if err == nil {
		t.Fatal("a fan-out over the ceiling was served, want it refused")
	}
}
