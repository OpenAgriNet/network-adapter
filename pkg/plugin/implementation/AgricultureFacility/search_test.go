package AgricultureFacility_test

// search_test.go covers the POLICY this package owns: how many of a
// multi-type search's calls run at once, in what order, and what happens when
// one fails. mappings_test.go already covers that splitting happens at all
// and produces the right facilities (TestATwoTypeSearchIsAnsweredWithBothTypes
// and neighbours) and that each call carries its own identity
// (TestEachSearchCallCarriesItsOwnRequestId).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/AgricultureFacility"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// runSearchWithExtract runs a search against the SHIPPED mapping with only its
// extract half replaced, so a test can make the mapping name values the payload
// does not and see which of the two the split follows.
//
// Everything else is the published file: patching one half rather than writing
// a mapping from scratch keeps the request and response halves honest, so the
// exchange completes and the assertion is about the split alone.
func runSearchWithExtract(t *testing.T, types []string, extract string, byCode map[string]string) *pocraByCategory {
	t.Helper()

	source, err := os.ReadFile(filepath.Join(mappingsDir, shippedMapping))
	if err != nil {
		t.Fatalf("could not read the shipped mapping: %v", err)
	}
	patched, err := withExtractHalf(string(source), extract)
	if err != nil {
		t.Fatalf("could not patch the shipped mapping: %v", err)
	}

	mappings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, patched)
	}))
	defer mappings.Close()

	pocra := &pocraByCategory{byCode: byCode}
	upstream := httptest.NewServer(pocra)
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

	step, closeStep, err := AgricultureFacility.New(context.Background(), registry, mapper,
		&AgricultureFacility.Config{BindingKeys: []string{shippedBindingKey}})
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

	stepCtx := &model.StepContext{Context: t.Context(), Body: body}
	if err := step.Run(stepCtx); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	return pocra
}

// withExtractHalf swaps the mapping's extract half for the given expression,
// leaving every other half as published.
//
// The half is found by its key and replaced up to the next top-level key, so
// this does not depend on how many lines the shipped expression spans. A
// mapping with no extract half at all is an error rather than a silent
// no-op: the test that patches one is asserting about the half, so its absence
// is the thing worth failing on.
func withExtractHalf(source, expression string) (string, error) {
	const key = "extract: |"
	start := strings.Index(source, key)
	if start < 0 {
		return "", fmt.Errorf("the mapping declares no %q half to replace", key)
	}
	rest := source[start+len(key):]
	// The next line that starts in column one is the next key, so the half ends
	// there. Every line of an expression is indented.
	end := len(rest)
	for offset := 0; offset < len(rest); {
		lineEnd := strings.IndexByte(rest[offset:], '\n')
		if lineEnd < 0 {
			break
		}
		next := offset + lineEnd + 1
		if next < len(rest) && rest[next] != ' ' && rest[next] != '\n' && rest[next] != '\t' {
			end = next
			break
		}
		offset = next
	}
	return source[:start] + key + "\n  " + expression + "\n\n" + rest[end:], nil
}

// runSearchTimed is runSplitSearch's sibling: it hands the caller a raw HTTP
// handler (so a test can measure timing or count in-flight calls) and an
// explicit Config (so a test can set SearchConcurrency), rather than a canned
// per-category response map.
func runSearchTimed(t *testing.T, types []string, cfg *AgricultureFacility.Config, handler http.HandlerFunc) error {
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

// The values a search splits on come from the MAPPING, not from a path
// compiled into this package.
//
// The payload here names one facility type and the mapping's extract half names
// two. What reaches POCRA has to follow the mapping: that is the whole point of
// declaring the split in configuration, and it is what lets a provider whose
// payload puts the types somewhere else ship a mapping rather than a build.
//
// Written this way round -- mapping and payload deliberately disagreeing --
// because a test where they agree passes whichever one the code actually reads.
func TestSearchSplitsOnWhatTheMappingExtracts(t *testing.T) {
	t.Parallel()

	pocra := runSearchWithExtract(t,
		[]string{"Warehouse"},
		`["KrishiVigyanKendra","Warehouse"]`,
		map[string]string{"kvk": providerResponse, "warehouse": warehouseResponse})

	if want := []string{"kvk", "warehouse"}; !slices.Equal(pocra.asked(), want) {
		t.Errorf("POCRA was asked for %v, want %v -- the split did not follow the mapping",
			pocra.asked(), want)
	}
}

// A search's calls are sequential unless the deployment raised the limit.
//
// The default is not a performance choice. A provider that answers one
// question at a time is often not built to be asked several at once, and
// POCRA's failure mode when pushed is a 200 with an empty catalog --
// indistinguishable from having no results, so the loss is silent. Parallel
// is opt-in per deployment for that reason, and this pins the default so it
// cannot drift.
func TestSearchCallsAreSequentialByDefault(t *testing.T) {
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
	if err := runSearchTimed(t, types, &AgricultureFacility.Config{}, handler); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Errorf("%d calls were in flight at once, want 1 by default", peak)
	}
}

// A deployment that raises the limit gets calls in parallel, bounded by it.
func TestSearchHonoursTheConfiguredConcurrency(t *testing.T) {
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
	cfg := &AgricultureFacility.Config{SearchConcurrency: 2}
	if err := runSearchTimed(t, types, cfg, handler); err != nil {
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
// type's failure cancels the shared context before the second type's call is
// ever issued.
func TestSearchSkipsCallsNotYetIssuedAfterAFailure(t *testing.T) {
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
	err := runSearchTimed(t, types, &AgricultureFacility.Config{}, handler)
	if err == nil {
		t.Fatal("Run() served a partial answer, want it refused")
	}

	mu.Lock()
	defer mu.Unlock()
	// Sequential (limit 1): the first type fails and cancels the shared
	// context before the loop ever reaches the second or third type's call.
	if calls != 1 {
		t.Errorf("the provider was called %d times, want 1 -- later facility types "+
			"must be skipped once an earlier one fails", calls)
	}
}

// A payload asking for more calls than the ceiling is refused, not clamped.
//
// Uses a repeated governed type to exceed MaxFacilityTypes, which the
// mapping's own duplicate-type required check ALSO refuses -- both guards
// agree the payload is bad, so the assertion that matters is that the provider
// was never called, not which guard fired first.
func TestSearchRefusesOverTheCeiling(t *testing.T) {
	t.Parallel()

	handler := func(w http.ResponseWriter, r *http.Request) {
		t.Error("the provider was called for a search that should have been refused")
	}

	types := make([]string, AgricultureFacility.MaxFacilityTypes+1)
	for i := range types {
		types[i] = "KrishiVigyanKendra"
	}
	err := runSearchTimed(t, types, &AgricultureFacility.Config{}, handler)
	if err == nil {
		t.Fatal("a search over the ceiling was served, want it refused")
	}
}

// A payload naming one type is one call.
func TestSearchOfOneTypeIsOneCall(t *testing.T) {
	t.Parallel()

	var calls int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, providerResponse)
	}

	if err := runSearchTimed(t, []string{"KrishiVigyanKendra"}, &AgricultureFacility.Config{}, handler); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("the provider saw %d calls, want 1 for a one-type search", got)
	}
}

// A payload naming no types at all is refused before the provider is called.
//
// The mapping's own required checks refuse this first, with a better message.
// This asserts the Go read refuses it too, because a payload with nothing to
// split across would otherwise become zero calls and an empty answer -- the
// silent-loss failure this whole facility exists to prevent.
func TestSearchRefusesAPayloadNamingNoTypes(t *testing.T) {
	t.Parallel()

	handler := func(w http.ResponseWriter, r *http.Request) {
		t.Error("the provider was called for a payload naming no facility types")
	}

	if err := runSearchTimed(t, []string{}, &AgricultureFacility.Config{}, handler); err == nil {
		t.Fatal("a payload naming no facility types was served, want it refused")
	}
}
