package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSecretStore stands in for OpenBao: entry -> key -> value.
type fakeSecretStore struct {
	entries map[string]map[string]string
	fetched bool
}

func (f *fakeSecretStore) Value(_ context.Context, entry, key string) (string, error) {
	f.fetched = true
	value, found := f.entries[entry][key]
	if !found {
		return "", fmt.Errorf("secret store entry %s has no key %s", entry, key)
	}
	return value, nil
}

func (f *fakeSecretStore) Held(entry, key string) (string, bool) {
	if !f.fetched {
		return "", false
	}
	value, found := f.entries[entry][key]
	return value, found
}

func headerProvider(t *testing.T, header string, got *string, calls *int) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		*got = r.Header.Get(header)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func okMapper() *stubMapper {
	return &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
}

func TestRowAuthReadsItsSecretFromTheStore(t *testing.T) {
	t.Setenv("MAUSAMGRAM_AUTH", "from-env")
	var got string
	var calls int
	url := headerProvider(t, "Authorization", &got, &calls)
	store := &fakeSecretStore{entries: map[string]map[string]string{
		testProvider: {"MAUSAMGRAM_AUTH": "from-store"}}}
	step := newStep(t, &stubRegistry{plan: rowPlanWithHeaderAuth(url, "Authorization", "MAUSAMGRAM_AUTH")},
		okMapper(), func(c *Config) { c.SecretStore = store })

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got != "from-store" {
		t.Errorf("provider received %q, want the store's value", got)
	}
}

func TestRowAuthReadsTheEnvironmentWhenNoStoreIsConfigured(t *testing.T) {
	t.Setenv("MAUSAMGRAM_AUTH", "from-env")
	var got string
	var calls int
	url := headerProvider(t, "Authorization", &got, &calls)
	step := newStep(t, &stubRegistry{plan: rowPlanWithHeaderAuth(url, "Authorization", "MAUSAMGRAM_AUTH")}, okMapper())

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got != "from-env" {
		t.Errorf("provider received %q, want the environment's value", got)
	}
}

func TestConfigAuthKeepsReadingTheEnvironmentWithAStore(t *testing.T) {
	t.Setenv("TEST_CFG_AUTH", "from-env")
	var got string
	var calls int
	url := headerProvider(t, "X-Key", &got, &calls)
	store := &fakeSecretStore{entries: map[string]map[string]string{testProvider: {"TEST_CFG_AUTH": "from-store"}}}
	step := newStep(t, &stubRegistry{plan: testPlan(url, http.MethodGet)}, okMapper(), func(c *Config) {
		c.SecretStore = store
		c.setProviderAuth(AuthProfile{Scheme: "header", HeaderName: "X-Key", HeaderValueEnv: "TEST_CFG_AUTH"})
	})

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got != "from-env" {
		t.Errorf("provider received %q, want the environment's value for config auth", got)
	}
}

func TestRowAuthMissingFromTheStoreIsRefusedBeforeTheCall(t *testing.T) {
	t.Setenv("MAUSAMGRAM_AUTH", "from-env")
	var got string
	var calls int
	url := headerProvider(t, "Authorization", &got, &calls)
	store := &fakeSecretStore{entries: map[string]map[string]string{}}
	step := newStep(t, &stubRegistry{plan: rowPlanWithHeaderAuth(url, "Authorization", "MAUSAMGRAM_AUTH")},
		okMapper(), func(c *Config) { c.SecretStore = store })

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("Run() served a provider whose secret is missing from the store")
	}
	if calls != 0 {
		t.Errorf("the provider was called %d times; want none, and never with the environment's value", calls)
	}
}

func TestRowTokenExchangeSendsTheStoresSecrets(t *testing.T) {
	issuer := &queryTokenServer{}
	tokenURL := issuer.start(t)
	var seen []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer provider.Close()

	plan := testPlan(provider.URL, http.MethodGet)
	plan.Auth = map[string]any{"scheme": "tokenQuery", "tokenQuery": map[string]any{
		"tokenUrl": tokenURL, "tokenUserField": "access_name", "tokenUserRef": "AGM_USER",
		"tokenSecretField": "password", "tokenSecretRef": "AGM_PASS", "tokenResponseField": "token",
		"queryName": "token", "tokenTtl": "600s"}}
	store := &fakeSecretStore{entries: map[string]map[string]string{
		testProvider: {"AGM_USER": "store-user", "AGM_PASS": "store-pass"}}}
	step := newStep(t, &stubRegistry{plan: plan}, okMapper(), func(c *Config) { c.SecretStore = store })

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if issuer.gotUser != "store-user" || issuer.gotSecret != "store-pass" {
		t.Errorf("token endpoint got %q/%q, want the store's user and password", issuer.gotUser, issuer.gotSecret)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "token=tok-1") {
		t.Errorf("provider saw %v, want the exchanged token", seen)
	}
}

func TestRedactionCoversASecretHeldFromTheStore(t *testing.T) {
	t.Parallel()

	store := &fakeSecretStore{entries: map[string]map[string]string{
		testProvider: {"MAUSAMGRAM_AUTH": "secret-from-the-store"}}}
	step := newStep(t, &stubRegistry{}, &stubMapper{}, func(c *Config) { c.SecretStore = store })
	plan := rowPlanWithHeaderAuth("http://unused", "Authorization", "MAUSAMGRAM_AUTH")
	auth, err := step.authFor(context.Background(), plan)
	if err != nil {
		t.Fatalf("authFor() = %v", err)
	}
	if got := step.redactString("before any read: secret-from-the-store"); !strings.Contains(got, "secret-from-the-store") {
		t.Errorf("redaction fetched from the store; it must only use what is already held")
	}
	if err := step.authenticate(auth, mustRequest(t)); err != nil {
		t.Fatalf("authenticate() = %v", err)
	}
	if got := step.redactString("provider said no: secret-from-the-store"); strings.Contains(got, "secret-from-the-store") {
		t.Errorf("the store's secret survived redaction: %s", got)
	}
}

func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://unused/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
