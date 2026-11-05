package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

const weather = "openagrinet:WeatherObservation"

// countingRegistry answers from plans and counts every lookup.
type countingRegistry struct {
	plans   map[string]*model.ProviderRecord
	lookups int
}

func (r *countingRegistry) ProviderRecord(_ context.Context, key string) (*model.ProviderRecord, error) {
	r.lookups++
	if plan, ok := r.plans[key]; ok {
		return plan, nil
	}
	return nil, fmt.Errorf("no record for %s", key)
}

func capabilityStep(t *testing.T, registry *countingRegistry, cfg *Config) *Step {
	t.Helper()
	step, closer, err := New(context.Background(), registry, okMapper(), nil, cfg)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	t.Cleanup(func() { _ = closer() })
	return step
}

func TestAStepWithCapabilityCodesServesAProviderItWasNeverConfiguredFor(t *testing.T) {
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer provider.Close()

	plan := planFor("skymet", weather, provider.URL)
	plan.Auth = map[string]any{"scheme": "none"}
	registry := &countingRegistry{plans: map[string]*model.ProviderRecord{"skymet|" + weather: plan}}
	step := capabilityStep(t, registry, &Config{CapabilityCodes: []string{weather}})

	if _, err := runStep(t, step, bodyFor("skymet", weather)); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if calls != 1 {
		t.Errorf("skymet was called %d times, want once from its registry row alone", calls)
	}
}

func TestAStepWithCapabilityCodesPassesOtherCapabilitiesThrough(t *testing.T) {
	t.Parallel()

	registry := &countingRegistry{}
	step := capabilityStep(t, registry, &Config{CapabilityCodes: []string{weather}})

	ctx, err := runStep(t, step, bodyFor("agmarknet", "openagrinet:MandiPrice"))
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if registry.lookups != 0 || ctx.Route != nil {
		t.Errorf("registry lookups %d, route %v; want the request passed through untouched", registry.lookups, ctx.Route)
	}
}

func TestAProviderWithNoAuthAnywhereIsRefusedNotCalledBare(t *testing.T) {
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer provider.Close()

	registry := &countingRegistry{plans: map[string]*model.ProviderRecord{
		"skymet|" + weather: planFor("skymet", weather, provider.URL)}}
	step := capabilityStep(t, registry, &Config{CapabilityCodes: []string{weather}})

	if _, err := runStep(t, step, bodyFor("skymet", weather)); err == nil {
		t.Fatal("Run() served a provider with no auth in its row or the config")
	}
	if calls != 0 {
		t.Errorf("the provider was called %d times without a credential", calls)
	}
}

func TestNewTakesCapabilityCodesOrBindingKeysButNotBoth(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]*Config{
		"neither": {},
		"both":    {CapabilityCodes: []string{weather}, BindingKeys: []string{testBindingKey}},
		"blank":   {CapabilityCodes: []string{" "}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := New(context.Background(), &countingRegistry{}, okMapper(), nil, cfg)
			if err == nil || !strings.Contains(err.Error(), "capabilityCodes") {
				t.Errorf("New() = %v, want a refusal naming capabilityCodes", err)
			}
		})
	}
}

func TestNewWithCapabilityCodesStillChecksAConfiguredProfile(t *testing.T) {
	t.Parallel()

	_, _, err := New(context.Background(), &countingRegistry{}, okMapper(), nil, &Config{
		CapabilityCodes: []string{weather},
		AuthByProvider:  map[string]*AuthProfile{"mausamgram": {Scheme: "magic"}},
	})
	if err == nil || !strings.Contains(err.Error(), "magic") {
		t.Errorf("New() = %v, want the bad scheme refused", err)
	}
}

func TestNewWithCapabilityCodesRefusesAnEnvelopeForACapabilityItDoesNotServe(t *testing.T) {
	t.Parallel()

	_, _, err := New(context.Background(), &countingRegistry{}, okMapper(), nil, &Config{
		CapabilityCodes: []string{weather},
		AuthByProvider:  map[string]*AuthProfile{"p": {Scheme: util.AuthSchemeNone}},
		Envelopes:       map[string]Envelope{"p|openagrinet:MandiPrice": &reversingEnvelope{}},
	})
	if err == nil || !strings.Contains(err.Error(), "openagrinet:MandiPrice") {
		t.Errorf("New() = %v, want the unserved capability refused", err)
	}

	if _, _, err := New(context.Background(), &countingRegistry{}, okMapper(), nil, &Config{
		CapabilityCodes: []string{weather},
		AuthByProvider:  map[string]*AuthProfile{"p": {Scheme: util.AuthSchemeNone}},
		Envelopes:       map[string]Envelope{"p": &reversingEnvelope{}, "p|" + weather: &reversingEnvelope{}},
	}); err != nil {
		t.Errorf("New() = %v; an envelope for a provider, or a served binding, is fine", err)
	}
}

func TestParseStepConfigReadsCapabilityCodes(t *testing.T) {
	t.Parallel()

	cfg, err := ParseStepConfig(map[string]string{"capabilityCodes": " openagrinet:Grievance, openagrinet:SchemeApplication "})
	if err != nil {
		t.Fatalf("ParseStepConfig() = %v", err)
	}
	want := []string{"openagrinet:Grievance", "openagrinet:SchemeApplication"}
	if strings.Join(cfg.CapabilityCodes, ",") != strings.Join(want, ",") {
		t.Errorf("capability codes = %v, want %v", cfg.CapabilityCodes, want)
	}
}
