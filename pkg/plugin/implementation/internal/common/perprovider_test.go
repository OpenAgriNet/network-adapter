package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// bodyFor builds a select naming a particular provider, so one step serving
// two binding keys can be sent a request for either.
func bodyFor(provider, capability string) string {
	return fmt.Sprintf(`{
	  "context": { "version": "2.0.0", "action": "select", "transactionId": "txn-1" },
	  "message": { "contract": { "commitments": [{
	    "resources": [{ "resourceAttributes": { "@type": %q } }],
	    "offer": { "provider": { "id": %q } }
	  }] } }
	}`, capability, provider)
}

// planFor is testPlan for an arbitrary provider.
func planFor(provider, capability, baseURL string) *model.ProviderRecord {
	return &model.ProviderRecord{
		BindingKey:     provider + "|" + capability,
		ParticipantID:  provider,
		CapabilityCode: capability,
		BaseURL:        baseURL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodGet, Path: "/x", Mappings: testMappingRef,
				TimeoutMs: 2000},
		},
	}
}

// routingRegistry answers with a different record per binding key, the way the
// real registry does.
type routingRegistry struct {
	plans map[string]*model.ProviderRecord
}

func (r *routingRegistry) ProviderRecord(_ context.Context, key string) (*model.ProviderRecord, error) {
	plan, ok := r.plans[key]
	if !ok {
		return nil, fmt.Errorf("no record for %s", key)
	}
	return plan, nil
}

// THE WHOLE POINT OF PER-PROVIDER AUTH. One step, two providers of the same
// capability, each authenticating its own way -- which was impossible while a
// step carried a single scheme.
func TestTwoProvidersOnOneStepAuthenticateDifferently(t *testing.T) {
	t.Setenv("TEST_PP_QUERY_TOKEN", "query-secret")
	t.Setenv("TEST_PP_HEADER_TOKEN", "header-secret")

	const capability = "openagrinet:KnowledgeAdvisory"

	var gotQuery, gotHeader string
	var queryCalls, headerCalls int32
	queryProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&queryCalls, 1)
		gotQuery = r.URL.Query().Get("token")
		fmt.Fprint(w, `{"from":"query"}`)
	}))
	defer queryProvider.Close()
	headerProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&headerCalls, 1)
		gotHeader = r.Header.Get("X-Api-Key")
		fmt.Fprint(w, `{"from":"header"}`)
	}))
	defer headerProvider.Close()

	cfg := &Config{
		BindingKeys: []string{"alpha|" + capability, "beta|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			"alpha": {Scheme: util.AuthSchemeQuery, QueryName: "token", QueryValueEnv: "TEST_PP_QUERY_TOKEN"},
			"beta":  {Scheme: util.AuthSchemeHeader, HeaderName: "X-Api-Key", HeaderValueEnv: "TEST_PP_HEADER_TOKEN"},
		},
	}
	registry := &routingRegistry{plans: map[string]*model.ProviderRecord{
		"alpha|" + capability: planFor("alpha", capability, queryProvider.URL),
		"beta|" + capability:  planFor("beta", capability, headerProvider.URL),
	}}
	step, closer, err := New(context.Background(), registry,
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	if _, err := runStep(t, step, bodyFor("alpha", capability)); err != nil {
		t.Fatalf("alpha: %v", err)
	}
	if _, err := runStep(t, step, bodyFor("beta", capability)); err != nil {
		t.Fatalf("beta: %v", err)
	}

	if queryCalls != 1 || headerCalls != 1 {
		t.Fatalf("calls: query=%d header=%d, want 1 each", queryCalls, headerCalls)
	}
	if gotQuery != "query-secret" {
		t.Errorf("alpha received token %q, want its own query credential", gotQuery)
	}
	if gotHeader != "header-secret" {
		t.Errorf("beta received X-Api-Key %q, want its own header credential", gotHeader)
	}
}

// A step-wide token cache would hand the first provider's token to the second,
// which is authenticating as somebody else. Each profile holds its own, so two
// issuers are exchanged with separately and neither token crosses over.
func TestTwoOAuth2ProvidersDoNotShareAToken(t *testing.T) {
	t.Setenv("TEST_PP_ID_A", "id-a")
	t.Setenv("TEST_PP_SECRET_A", "secret-a")
	t.Setenv("TEST_PP_ID_B", "id-b")
	t.Setenv("TEST_PP_SECRET_B", "secret-b")

	const capability = "openagrinet:KnowledgeAdvisory"

	var exchangesA, exchangesB int32
	issuerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&exchangesA, 1)
		fmt.Fprint(w, `{"access_token":"token-A","expires_in":3600}`)
	}))
	defer issuerA.Close()
	issuerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&exchangesB, 1)
		fmt.Fprint(w, `{"access_token":"token-B","expires_in":3600}`)
	}))
	defer issuerB.Close()

	var mu sync.Mutex
	seen := map[string][]string{}
	record := func(provider string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[provider] = append(seen[provider],
				strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			mu.Unlock()
			fmt.Fprint(w, `{"ok":true}`)
		}
	}
	providerA := httptest.NewServer(record("alpha"))
	defer providerA.Close()
	providerB := httptest.NewServer(record("beta"))
	defer providerB.Close()

	cfg := &Config{
		BindingKeys: []string{"alpha|" + capability, "beta|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			"alpha": {Scheme: util.AuthSchemeOAuth2, TokenURL: issuerA.URL,
				ClientIDEnv: "TEST_PP_ID_A", ClientSecretEnv: "TEST_PP_SECRET_A"},
			"beta": {Scheme: util.AuthSchemeOAuth2, TokenURL: issuerB.URL,
				ClientIDEnv: "TEST_PP_ID_B", ClientSecretEnv: "TEST_PP_SECRET_B"},
		},
	}
	registry := &routingRegistry{plans: map[string]*model.ProviderRecord{
		"alpha|" + capability: planFor("alpha", capability, providerA.URL),
		"beta|" + capability:  planFor("beta", capability, providerB.URL),
	}}
	step, closer, err := New(context.Background(), registry,
		&stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	// Twice each: the second call of a pair must reuse that provider's token.
	for i := 0; i < 2; i++ {
		if _, err := runStep(t, step, bodyFor("alpha", capability)); err != nil {
			t.Fatalf("alpha call %d: %v", i, err)
		}
		if _, err := runStep(t, step, bodyFor("beta", capability)); err != nil {
			t.Fatalf("beta call %d: %v", i, err)
		}
	}

	if exchangesA != 1 || exchangesB != 1 {
		t.Errorf("exchanges: A=%d B=%d, want 1 each -- each profile caches its own",
			exchangesA, exchangesB)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, got := range seen["alpha"] {
		if got != "token-A" {
			t.Errorf("alpha was sent %q, want token-A -- a token crossed between providers", got)
		}
	}
	for _, got := range seen["beta"] {
		if got != "token-B" {
			t.Errorf("beta was sent %q, want token-B -- a token crossed between providers", got)
		}
	}
	if len(seen["alpha"]) != 2 || len(seen["beta"]) != 2 {
		t.Errorf("calls: alpha=%d beta=%d, want 2 each", len(seen["alpha"]), len(seen["beta"]))
	}
}

// Redaction is about what could appear in a piece of text, not about which
// provider the request was for. An error raised while serving one provider can
// quote another's credential, so every profile's secrets are covered.
func TestRedactionCoversEveryProvidersSecret(t *testing.T) {
	t.Setenv("TEST_PP_RED_A", "secret-of-alpha")
	t.Setenv("TEST_PP_RED_B", "secret-of-beta")

	const capability = "openagrinet:KnowledgeAdvisory"
	cfg := &Config{
		BindingKeys: []string{"alpha|" + capability, "beta|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			"alpha": {Scheme: util.AuthSchemeHeader, HeaderName: "X-A", HeaderValueEnv: "TEST_PP_RED_A"},
			"beta":  {Scheme: util.AuthSchemeHeader, HeaderName: "X-B", HeaderValueEnv: "TEST_PP_RED_B"},
		},
	}
	step, closer, err := New(context.Background(), &routingRegistry{}, &stubMapper{}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	text := "provider said no: alpha=secret-of-alpha beta=secret-of-beta"
	got := step.redactString(text)
	if strings.Contains(got, "secret-of-alpha") {
		t.Errorf("alpha's credential survived redaction: %s", got)
	}
	if strings.Contains(got, "secret-of-beta") {
		t.Errorf("beta's credential survived redaction: %s", got)
	}
	if !strings.Contains(got, "provider said no") {
		t.Errorf("redaction ate the message: %s", got)
	}
}

// One provider's credential can be a substring of another's. The longer has to
// be replaced first, or it is left half-redacted with the tail exposed.
func TestRedactionSortsAcrossProvidersNotWithinOne(t *testing.T) {
	t.Setenv("TEST_PP_SHORT", "abc")
	t.Setenv("TEST_PP_LONG", "abcdef")

	const capability = "openagrinet:KnowledgeAdvisory"
	cfg := &Config{
		BindingKeys: []string{"shorty|" + capability, "longy|" + capability},
		AuthByProvider: map[string]*AuthProfile{
			"shorty": {Scheme: util.AuthSchemeHeader, HeaderName: "X-S", HeaderValueEnv: "TEST_PP_SHORT"},
			"longy":  {Scheme: util.AuthSchemeHeader, HeaderName: "X-L", HeaderValueEnv: "TEST_PP_LONG"},
		},
	}
	step, closer, err := New(context.Background(), &routingRegistry{}, &stubMapper{}, nil, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	got := step.redactString("token=abcdef")
	if strings.Contains(got, "def") {
		t.Errorf("the longer credential was half-redacted, leaving its tail: %s", got)
	}
}
