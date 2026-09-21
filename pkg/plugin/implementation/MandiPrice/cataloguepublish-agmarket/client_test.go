package agmarket

// client_test.go proves pipelineClient's two calls -- minting a token and
// making a mapped GET -- against both a synthetic upstream and, for the
// mapped GET, the real master-states.yaml mapping this package embeds. The
// no-data classification is exercised directly because it is the whole
// reason this client distinguishes an empty result from a failure: recording
// it as a failure previously turned 27 of 36 states into false outages (see
// errNoUpstreamData in client.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/tools/publish/catalogpublish"
)

// testMapper serves this package's embedded mappings over a loopback
// listener and builds a real JSONata mapper against them, so a test
// exercises the actual mapping file rather than a stand-in for it.
//
// catalogpublish used to live under tools/publish/internal/, which Go's
// internal-import rule kept out of reach of anything outside the
// tools/publish tree -- this package briefly reimplemented ServeMappings and
// NewMapper locally for that reason. catalogpublish has since moved to
// tools/publish/catalogpublish (no longer internal), so this now imports the
// real thing instead of a parallel copy.
func testMapper(t *testing.T) (mapperRunner, string) {
	t.Helper()

	base, stop, err := catalogpublish.ServeMappings(Files, "mappings")
	if err != nil {
		t.Fatalf("ServeMappings: %v", err)
	}
	t.Cleanup(stop)

	mapper, closer, err := catalogpublish.NewMapper(context.Background())
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	t.Cleanup(func() { _ = closer() })

	return mapper, base
}

func TestPipelineClient_Token_ExchangesCredentials(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"token":"tok-abc"}`))
	}))
	defer srv.Close()

	client := newPipelineClient(srv.URL)
	token, err := client.token(context.Background(), "user1", "secret1")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if token != "tok-abc" {
		t.Errorf("token = %q, want %q", token, "tok-abc")
	}
	if gotBody["access_name"] != "user1" || gotBody["password"] != "secret1" {
		t.Errorf("body = %v, want access_name=user1 password=secret1", gotBody)
	}
}

func TestPipelineClient_Token_RejectsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"invalid credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := newPipelineClient(srv.URL)
	if _, err := client.token(context.Background(), "user1", "secret1"); err == nil {
		t.Fatal("want an error for a 401 response")
	}
}

func TestPipelineClient_HTTPGet_MasterStates_TransformsRealMapping(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	// The upstream answers with a bare JSON array on success (as the reference
	// tool's tests and its call() establish); httpGet wraps that array under
	// "response" itself before invoking the mapping's response half, which is
	// what "response" resolves to in master-states.yaml's JSONata expression.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "option=4") {
			t.Errorf("query = %q, want option=4", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"agm_state_code":"MH","state_name":"Maharashtra"}]`))
	}))
	defer upstream.Close()

	client := newPipelineClient(upstream.URL)
	client.http = upstream.Client()

	out, err := client.httpGet(context.Background(), mapper, mappingBase+"/master-states.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": "tok-abc"})
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}

	var states []struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &states); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(states) != 1 || states[0].Code != "MH" || states[0].Name != "Maharashtra" {
		t.Fatalf("states = %+v, want one MH/Maharashtra entry", states)
	}
}

func TestPipelineClient_HTTPGet_ClassifiesNoDataAsEmptyResult(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"success":false,"message":"No data available."}`, http.StatusBadRequest)
	}))
	defer upstream.Close()

	client := newPipelineClient(upstream.URL)
	client.http = upstream.Client()

	_, err := client.httpGet(context.Background(), mapper, mappingBase+"/master-states.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": "tok-abc"})
	if err == nil {
		t.Fatal("want an error for a no-data upstream response")
	}
	if err != errNoUpstreamData {
		t.Errorf("err = %v, want errNoUpstreamData", err)
	}
}
