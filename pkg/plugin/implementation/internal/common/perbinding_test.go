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

// newForAuth builds a step whose registry answers for every binding key in the
// config, so a test only has to say what the credentials are.
func newForAuth(t *testing.T, cfg *Config, baseURL string) error {
	t.Helper()
	plans := map[string]*model.ProviderRecord{}
	for _, key := range cfg.BindingKeys {
		provider, capability, _ := strings.Cut(key, "|")
		plans[key] = planFor(provider, capability, baseURL)
	}
	_, closer, err := New(context.Background(), nil, &routingRegistry{plans: plans},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if closer != nil {
		t.Cleanup(func() { _ = closer() })
	}
	return err
}

// THE WHOLE POINT OF PER-BINDING AUTH. PMFBY's grievance and insurance APIs
// share a hostname and are two systems behind two logins, which a profile
// keyed by participant could not express: configuring one took the other's
// credential away. See docs/adr/0006-a-binding-may-carry-its-own-credential.md
func TestOneParticipantAuthenticatesEachCapabilityAsItself(t *testing.T) {
	t.Setenv("TEST_PB_GRIEVANCE", "grievance-secret")
	t.Setenv("TEST_PB_INSURANCE", "insurance-secret")

	const (
		provider  = "pmfby"
		grievance = "openagrinet:Grievance"
		insurance = "openagrinet:SchemeApplication"
	)

	seen := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen["Authorization"] = r.Header.Get("Authorization")
		seen["token"] = r.Header.Get("token")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		BindingKeys: []string{provider + "|" + grievance, provider + "|" + insurance},
		AuthByProvider: map[string]*AuthProfile{
			provider + "|" + grievance: {Scheme: util.AuthSchemeHeader,
				HeaderName: "Authorization", HeaderValueEnv: "TEST_PB_GRIEVANCE"},
			provider + "|" + insurance: {Scheme: util.AuthSchemeHeader,
				HeaderName: "token", HeaderValueEnv: "TEST_PB_INSURANCE"},
		},
	}
	plans := map[string]*model.ProviderRecord{
		provider + "|" + grievance: planFor(provider, grievance, upstream.URL),
		provider + "|" + insurance: planFor(provider, insurance, upstream.URL),
	}
	step, closer, err := New(context.Background(), nil, &routingRegistry{plans: plans},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	if _, err := runStep(t, step, bodyFor(provider, grievance)); err != nil {
		t.Fatalf("grievance: %v", err)
	}
	if seen["Authorization"] != "grievance-secret" {
		t.Errorf("grievance sent Authorization %q, want its own credential", seen["Authorization"])
	}
	if seen["token"] != "" {
		t.Errorf("grievance also sent token %q, want nothing", seen["token"])
	}

	seen = map[string]string{}
	if _, err := runStep(t, step, bodyFor(provider, insurance)); err != nil {
		t.Fatalf("insurance: %v", err)
	}
	if seen["token"] != "insurance-secret" {
		t.Errorf("insurance sent token %q, want its own credential", seen["token"])
	}
	if seen["Authorization"] != "" {
		t.Errorf("insurance also sent Authorization %q, want nothing", seen["Authorization"])
	}
}

// Every configuration written before per-binding auth names participants, so
// the binding lookup must miss and the participant's profile must still serve
// every one of its bindings. This is the whole back-compatibility claim.
func TestAProviderProfileStillServesEveryBindingOfThatProvider(t *testing.T) {
	t.Setenv("TEST_PB_SHARED", "shared-secret")

	const provider = "pmkisan"
	var calls int
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		got = r.Header.Get("X-Api-Key")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	capabilities := []string{"openagrinet:Grievance", "openagrinet:SchemeApplication"}
	cfg := &Config{
		BindingKeys: []string{provider + "|" + capabilities[0], provider + "|" + capabilities[1]},
		AuthByProvider: map[string]*AuthProfile{
			provider: {Scheme: util.AuthSchemeHeader,
				HeaderName: "X-Api-Key", HeaderValueEnv: "TEST_PB_SHARED"},
		},
	}
	plans := map[string]*model.ProviderRecord{}
	for _, capability := range capabilities {
		plans[provider+"|"+capability] = planFor(provider, capability, upstream.URL)
	}
	step, closer, err := New(context.Background(), nil, &routingRegistry{plans: plans},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	for _, capability := range capabilities {
		if _, err := runStep(t, step, bodyFor(provider, capability)); err != nil {
			t.Fatalf("%s: %v", capability, err)
		}
		if got != "shared-secret" {
			t.Errorf("%s sent X-Api-Key %q, want the participant's credential", capability, got)
		}
	}
	if calls != 2 {
		t.Fatalf("upstream saw %d calls, want 2", calls)
	}
}

// The binding's own profile beats its participant's, since otherwise an
// operator adding one capability's login would not be able to tell whether it
// had taken effect.
func TestABindingProfileBeatsItsParticipants(t *testing.T) {
	t.Setenv("TEST_PB_WIDE", "participant-secret")
	t.Setenv("TEST_PB_NARROW", "binding-secret")

	const provider = "aif"
	const capability = "openagrinet:SchemeApplication"

	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Api-Key")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		BindingKeys: []string{provider + "|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			provider: {Scheme: util.AuthSchemeHeader,
				HeaderName: "X-Api-Key", HeaderValueEnv: "TEST_PB_WIDE"},
			provider + "|" + capability: {Scheme: util.AuthSchemeHeader,
				HeaderName: "X-Api-Key", HeaderValueEnv: "TEST_PB_NARROW"},
		},
	}
	plans := map[string]*model.ProviderRecord{
		provider + "|" + capability: planFor(provider, capability, upstream.URL),
	}
	step, closer, err := New(context.Background(), nil, &routingRegistry{plans: plans},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	if _, err := runStep(t, step, bodyFor(provider, capability)); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != "binding-secret" {
		t.Errorf("sent %q, want the binding's own credential", got)
	}
}

// Both startup refusals survive the relaxation: they are what stops a
// misspelled block from reading as a provider that needs no credential.
func TestStartupStillRefusesAStrayAuthBlock(t *testing.T) {
	t.Setenv("TEST_PB_STRAY", "x")
	cfg := &Config{
		BindingKeys: []string{"alpha|openagrinet:Grievance"},
		AuthByProvider: map[string]*AuthProfile{
			"alpha":                  {Scheme: util.AuthSchemeNone},
			"alpha|openagrinet:Typo": {Scheme: util.AuthSchemeNone},
		},
	}
	err := newForAuth(t, cfg, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "openagrinet:Typo") {
		t.Fatalf("New: %v, want a refusal naming the stray block", err)
	}
}

func TestStartupStillRefusesAServedBindingWithNoAuthBlock(t *testing.T) {
	cfg := &Config{
		BindingKeys:    []string{"alpha|openagrinet:Grievance"},
		AuthByProvider: map[string]*AuthProfile{},
	}
	err := newForAuth(t, cfg, "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "has no auth block") {
		t.Fatalf("New: %v, want a refusal for the unconfigured binding", err)
	}
}

// PMFBY's insurance API checks an exchanged token and a fixed password on the
// same request, which a scheme cannot express: it sends exactly one thing.
func TestTheExtraHeaderRidesBesideTheSchemesOwn(t *testing.T) {
	t.Setenv("TEST_PB_TOKEN", "the-token")
	t.Setenv("TEST_PB_PASSWORD", "the-password")

	const provider = "pmfby"
	const capability = "openagrinet:SchemeApplication"

	var token, password string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("token")
		password = r.Header.Get("password")
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		BindingKeys: []string{provider + "|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			provider: {
				Scheme: util.AuthSchemeHeader, HeaderName: "token", HeaderValueEnv: "TEST_PB_TOKEN",
				ExtraHeaderName: "password", ExtraHeaderValueEnv: "TEST_PB_PASSWORD",
			},
		},
	}
	plans := map[string]*model.ProviderRecord{
		provider + "|" + capability: planFor(provider, capability, upstream.URL),
	}
	step, closer, err := New(context.Background(), nil, &routingRegistry{plans: plans},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	if _, err := runStep(t, step, bodyFor(provider, capability)); err != nil {
		t.Fatalf("select: %v", err)
	}
	if token != "the-token" {
		t.Errorf("token header %q, want the scheme's credential", token)
	}
	if password != "the-password" {
		t.Errorf("password header %q, want the extra credential", password)
	}
}

// Unset is the state every existing config is in, and it must send nothing.
func TestNoExtraHeaderIsSentWhenItIsNotConfigured(t *testing.T) {
	t.Setenv("TEST_PB_TOKEN2", "the-token")

	const provider = "pmkisan"
	const capability = "openagrinet:Grievance"

	var extras int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			if name != "Token" && name != "Accept-Encoding" && name != "User-Agent" &&
				name != "Content-Length" && name != "Content-Type" {
				extras++
			}
		}
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()

	cfg := &Config{
		BindingKeys: []string{provider + "|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			provider: {Scheme: util.AuthSchemeHeader, HeaderName: "token", HeaderValueEnv: "TEST_PB_TOKEN2"},
		},
	}
	plans := map[string]*model.ProviderRecord{
		provider + "|" + capability: planFor(provider, capability, upstream.URL),
	}
	step, closer, err := New(context.Background(), nil, &routingRegistry{plans: plans},
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	if _, err := runStep(t, step, bodyFor(provider, capability)); err != nil {
		t.Fatalf("select: %v", err)
	}
	if extras != 0 {
		t.Errorf("upstream saw %d unexpected headers, want none", extras)
	}
}

func TestAHalfWrittenExtraHeaderIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile *AuthProfile
		want    string
	}{
		{"name without variable",
			&AuthProfile{Scheme: util.AuthSchemeNone, ExtraHeaderName: "password"},
			"set together or not at all"},
		{"variable without name",
			&AuthProfile{Scheme: util.AuthSchemeNone, ExtraHeaderValueEnv: "TEST_PB_X"},
			"set together or not at all"},
		{"the header the scheme already sets",
			&AuthProfile{Scheme: util.AuthSchemeHeader, HeaderName: "token",
				HeaderValueEnv: "TEST_PB_X", ExtraHeaderName: "Token",
				ExtraHeaderValueEnv: "TEST_PB_Y"},
			"one would overwrite the other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				BindingKeys:    []string{"alpha|openagrinet:Grievance"},
				AuthByProvider: map[string]*AuthProfile{"alpha": tc.profile},
			}
			err := newForAuth(t, cfg, "http://127.0.0.1:1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New: %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}
