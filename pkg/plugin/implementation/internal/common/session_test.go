package common

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// sessionBinding is AIF's shape: one capability buys the token, and both it
// and the grievance capability spend it.
const (
	sessionBinding   = "aif|openagrinet:SchemeApplication"
	sessionProvider  = "aif"
	sessionMapping   = "https://mappings.example.com/aif/scheme-application.yaml"
	sessionBodyStart = `{
  "context": { "version": "2.0.0", "action": "init", "transactionId": "txn-aif-1",
               "receiverId": "oan.test" },
  "message": { "contract": { "commitments": [{
    "resources": [{ "resourceAttributes": {
      "@type": "openagrinet:SchemeApplication", "beneficiaryId": "4242" } }],
    "offer": { "provider": { "id": "aif" } }
  }] } }
}`
	sessionBodyStatus = `{
  "context": { "version": "2.0.0", "action": "status", "transactionId": "txn-aif-1",
               "receiverId": "oan.test" },
  "message": { "contract": { "commitments": [{
    "resources": [{ "resourceAttributes": {
      "@type": "openagrinet:SchemeApplication", "beneficiaryId": "4242" } }],
    "offer": { "provider": { "id": "aif" } }
  }] } }
}`
)

// memoryCache stands in for the deployment's Redis. It records the TTL it was
// given, which is the only way to see the expiry margin was applied.
type memoryCache struct {
	mu     sync.Mutex
	values map[string]string
	ttls   map[string]time.Duration
	setErr error
}

func newMemoryCache() *memoryCache {
	return &memoryCache{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (c *memoryCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key], nil
}

func (c *memoryCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key], c.ttls[key] = value, ttl
	return nil
}

func (c *memoryCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, key)
	return nil
}

func (c *memoryCache) Clear(context.Context) error { return nil }

// sessionConfig is the block an operator writes for AIF, flattened as the
// plugin receives it.
func sessionConfig() map[string]string {
	return map[string]string{
		"sessionKeyAt-" + sessionBinding:      "context.transactionId,context.transaction_id",
		"sessionCaptureOn-" + sessionBinding:  "init",
		"sessionSendOn-" + sessionBinding:     "status",
		"sessionTokenField-" + sessionBinding: "Token",
		"sessionTtlField-" + sessionBinding:   "ExpiresIn",
		"sessionTtl-" + sessionBinding:        "1h",
		"sessionTtlMargin-" + sessionBinding:  "60s",
		"sessionBindAt-" + sessionBinding: "message.contract.commitments[].resources[]." +
			"resourceAttributes.beneficiaryId",
	}
}

// sessionStep wires a step for the two AIF calls against one test upstream.
func sessionStep(t *testing.T, cache *memoryCache, upstream string, mapper *stubMapper) *Step {
	t.Helper()

	sessions, rest, err := ParseProviderSessions(sessionConfig())
	if err != nil {
		t.Fatalf("ParseProviderSessions() refused a valid block: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("every setting was a session setting, yet %v was left over", rest)
	}

	cfg := &Config{
		BindingKeys:      []string{sessionBinding},
		SessionByBinding: sessions,
		AuthByProvider: map[string]*AuthProfile{
			sessionProvider: {Provider: sessionProvider, Scheme: util.AuthSchemeNone},
		},
	}
	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey:     sessionBinding,
		ParticipantID:  sessionProvider,
		CapabilityCode: "openagrinet:SchemeApplication",
		BaseURL:        upstream,
		Actions: map[string]model.ActionPlan{
			"init":   {Method: http.MethodPost, Path: "/validate_beneficiaries/", Mappings: sessionMapping, TimeoutMs: 2000},
			"status": {Method: http.MethodGet, Path: "/Status", Mappings: sessionMapping, TimeoutMs: 2000},
		},
	}}

	step, closer, err := New(context.Background(), cache, registry, mapper, nil, cfg)
	if err != nil {
		t.Fatalf("New() returned an unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = closer() })
	return step
}

func TestSessionIsHeldOnVerifyAndSentOnStatus(t *testing.T) {
	var sent []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Header.Get("Authorization"))
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"Success":true,"Token":"tok-abc","ExpiresIn":3600}`))
			return
		}
		_, _ = w.Write([]byte(`"Disbursed"`))
	}))
	defer upstream.Close()

	cache := newMemoryCache()
	mapper := &stubMapper{requestResult: []byte(`{"beneficiaryId":"4242"}`), responseResult: []byte(`{"ok":true}`)}
	step := sessionStep(t, cache, upstream.URL, mapper)

	if _, err := runStep(t, step, sessionBodyStart); err != nil {
		t.Fatalf("the verify call failed: %v", err)
	}
	held, _ := cache.Get(t.Context(), "session|"+sessionBinding+"|txn-aif-1")
	if !strings.Contains(held, "tok-abc") {
		t.Fatalf("the token the provider handed out was not held; cache holds %q", held)
	}
	// 3600 stated, less the 60s margin.
	if got := cache.ttls["session|"+sessionBinding+"|txn-aif-1"]; got != 59*time.Minute {
		t.Errorf("session kept for %v, want the stated hour less the 60s margin", got)
	}

	if _, err := runStep(t, step, sessionBodyStatus); err != nil {
		t.Fatalf("the status call failed: %v", err)
	}
	if len(sent) != 2 || sent[1] != "Bearer tok-abc" {
		t.Errorf("status carried Authorization %q, want the held token", sent)
	}
	if sent[0] != "" {
		t.Errorf("the call that buys the session carried a session header %q", sent[0])
	}
}

func TestStatusWithoutASessionIsRefusedBeforeCalling(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := sessionStep(t, newMemoryCache(), upstream.URL, mapper)

	_, err := runStep(t, step, sessionBodyStatus)
	if err == nil || !strings.Contains(err.Error(), "verified session") {
		t.Fatalf("status with no session returned %v, want a refusal naming the missing session", err)
	}
	if called {
		t.Error("the provider was called for a request that had no session")
	}
}

func TestASessionIsNotSpentByAnotherBeneficiary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"Token":"tok-abc","ExpiresIn":3600}`))
			return
		}
		_, _ = w.Write([]byte(`"Disbursed"`))
	}))
	defer upstream.Close()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := sessionStep(t, newMemoryCache(), upstream.URL, mapper)

	if _, err := runStep(t, step, sessionBodyStart); err != nil {
		t.Fatalf("the verify call failed: %v", err)
	}
	// Same transaction, different farmer: the id alone must not be enough.
	someoneElse := strings.Replace(sessionBodyStatus, `"beneficiaryId": "4242"`, `"beneficiaryId": "9999"`, 1)
	_, err := runStep(t, step, someoneElse)
	if err == nil || !strings.Contains(err.Error(), "different beneficiary") {
		t.Fatalf("a different beneficiary on the same transaction returned %v, want a refusal", err)
	}
}

func TestTheOtpCallWithNoTokenHoldsNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// AIF's first init call: the OTP went to the farmer's phone and there
		// is no token yet.
		_, _ = w.Write([]byte(`{"Success":true,"MobileNumber":"XXXXXX4242"}`))
	}))
	defer upstream.Close()

	cache := newMemoryCache()
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := sessionStep(t, cache, upstream.URL, mapper)

	if _, err := runStep(t, step, sessionBodyStart); err != nil {
		t.Fatalf("the OTP call failed: %v", err)
	}
	if len(cache.values) != 0 {
		t.Errorf("an answer with no token held %v", cache.values)
	}
}

func TestAVerifiedSessionThatCannotBeHeldFailsTheCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Token":"tok-abc","ExpiresIn":3600}`))
	}))
	defer upstream.Close()

	cache := newMemoryCache()
	cache.setErr = context.DeadlineExceeded
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	step := sessionStep(t, cache, upstream.URL, mapper)

	// Failing is the point: the OTP cannot be spent twice, so answering
	// "verified" while holding nothing leaves the farmer stuck.
	_, err := runStep(t, step, sessionBodyStart)
	if err == nil || !strings.Contains(err.Error(), "could not be held") {
		t.Fatalf("a session that could not be stored returned %v, want a failure", err)
	}
}

func TestNewRefusesASessionWithNowhereToKeepIt(t *testing.T) {
	t.Parallel()

	sessions, _, err := ParseProviderSessions(sessionConfig())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = New(context.Background(), nil, &stubRegistry{}, &stubMapper{}, nil, &Config{
		BindingKeys:      []string{sessionBinding},
		SessionByBinding: sessions,
		AuthByProvider: map[string]*AuthProfile{
			sessionProvider: {Provider: sessionProvider, Scheme: util.AuthSchemeNone},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no cache plugin") {
		t.Fatalf("a session with no cache returned %v, want a startup refusal", err)
	}
}

func TestParseProviderSessionsLeavesCredentialsAlone(t *testing.T) {
	t.Parallel()

	config := sessionConfig()
	config["authScheme-aif"] = "header"
	config["headerName-aif"] = "apiKey"
	config["bindingKeys"] = sessionBinding

	sessions, rest, err := ParseProviderSessions(config)
	if err != nil {
		t.Fatalf("ParseProviderSessions() refused a valid block: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("read %d sessions, want 1", len(sessions))
	}
	// What is left has to be exactly what auth understands: ParseProviderAuth
	// refuses any dashed setting it does not know.
	if _, err := ParseProviderAuth(rest); err != nil {
		t.Fatalf("ParseProviderAuth() refused what sessions left behind: %v", err)
	}
}

func TestParseProviderSessionsRefusesAHalfWrittenBlock(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		config map[string]string
		says   string
	}{
		{"no key path", map[string]string{
			"sessionCaptureOn-" + sessionBinding: "init", "sessionTokenField-" + sessionBinding: "Token",
		}, "sessionKeyAt"},
		{"captures nothing and sends nothing", map[string]string{
			"sessionKeyAt-" + sessionBinding: "context.transactionId",
		}, "sessionCaptureOn"},
		{"captures with no token field", map[string]string{
			"sessionKeyAt-" + sessionBinding:     "context.transactionId",
			"sessionCaptureOn-" + sessionBinding: "init",
		}, "sessionTokenField"},
		{"captures with no lifetime", map[string]string{
			"sessionKeyAt-" + sessionBinding:     "context.transactionId",
			"sessionCaptureOn-" + sessionBinding: "init", "sessionTokenField-" + sessionBinding: "Token",
		}, "expires"},
		{"a lifetime that is not a duration", map[string]string{
			"sessionKeyAt-" + sessionBinding:     "context.transactionId",
			"sessionCaptureOn-" + sessionBinding: "init", "sessionTokenField-" + sessionBinding: "Token",
			"sessionTtl-" + sessionBinding: "an hour",
		}, "duration"},
		{"set for the whole step", map[string]string{
			"sessionKeyAt": "context.transactionId",
		}, "one binding"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ParseProviderSessions(tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("got %v, want a refusal mentioning %q", err, tc.says)
			}
		})
	}
}

func TestASharedSessionIsReadUnderTheBindingThatBoughtIt(t *testing.T) {
	t.Parallel()

	grievance := &SessionProfile{
		Binding:    "aif|openagrinet:Grievance",
		StoreUnder: sessionBinding,
		KeyAt:      "context.transactionId",
	}
	beckn, err := decodeBody([]byte(sessionBodyStatus))
	if err != nil {
		t.Fatal(err)
	}
	key, err := grievance.cacheKey(beckn)
	if err != nil {
		t.Fatal(err)
	}
	if want := "session|" + sessionBinding + "|txn-aif-1"; key != want {
		t.Errorf("grievance read %q, want the scheme application's own key %q", key, want)
	}
}

func TestAProviderAnswerIsReadWhateverItsCasing(t *testing.T) {
	t.Parallel()

	answer, err := decodeBody([]byte(`{"data":{"token":"tok","expiresIn":"120"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := asText(lookupField(answer, "Data.Token")); got != "tok" {
		t.Errorf("read %q, want the token whatever the provider capitalised", got)
	}
	if got := asSeconds(lookupField(answer, "data.ExpiresIn")); got != 2*time.Minute {
		t.Errorf("read %v, want a quoted lifetime read as seconds", got)
	}
}

// The beneficiary a session was bought for may be written in one place by the
// action that buys it and another by the action that spends it -- KHC confirms
// on a contract and files on a support channel. A bind that reads only the
// first shape finds nothing on the second and lets the call through unchecked.
func TestSessionBindReadsEitherShape(t *testing.T) {
	const bind = "message.contract.commitments[].commitmentAttributes.farmerMobileNumber," +
		"message.support.channels[].farmerMobileNumber"

	contract := map[string]any{"message": map[string]any{"contract": map[string]any{
		"commitments": []any{map[string]any{
			"commitmentAttributes": map[string]any{"farmerMobileNumber": "9876543210"},
		}},
	}}}
	channel := map[string]any{"message": map[string]any{"support": map[string]any{
		"channels": []any{map[string]any{"farmerMobileNumber": "9876543210"}},
	}}}

	for name, body := range map[string]map[string]any{"contract": contract, "channel": channel} {
		if got := firstValueAt(body, bind); got != "9876543210" {
			t.Errorf("firstValueAt(%s) = %q, want the beneficiary", name, got)
		}
	}
	if got := firstValueAt(map[string]any{"message": map[string]any{}}, bind); got != "" {
		t.Errorf("firstValueAt(neither shape) = %q, want empty", got)
	}
}
