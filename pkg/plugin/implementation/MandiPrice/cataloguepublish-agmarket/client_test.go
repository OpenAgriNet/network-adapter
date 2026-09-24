package agmarket

// client_test.go exercises the frame's upstream Client against THIS pipeline's
// real mappings. That pairing is the point: the mappings decide what a request
// looks like and how a response is read, so testing the client without them
// would prove only that Go can make an HTTP call.
//
// The client itself is internal/pipeline's and is tested there against a
// synthetic upstream. What is here is what needs mandi's own mapping files.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/catalogpublish"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// testMapper serves this package's embedded mappings over a loopback
// listener and builds a real JSONata mapper against them, so a test
// exercises the actual mapping file rather than a stand-in for it.
//
// catalogpublish used to live under tools/publish/internal/, which Go's
// internal-import rule kept out of reach of anything outside that tree --
// this package briefly reimplemented ServeMappings and NewMapper locally for
// that reason. It now lives at implementation/catalogpublisher/catalogpublish, reachable by every
// package under implementation/, so this imports the real thing instead of a
// parallel copy.
func testMapper(t *testing.T) (pipeline.Mapper, string) {
	t.Helper()

	base, stop, err := catalogpublish.ServeMappings(Files, mappingsDir)
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

	client := pipeline.NewClient(upstream.URL)

	out, err := client.Get(context.Background(), mapper, mappingBase+"/master-states.yaml",
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

	client := pipeline.NewClient(upstream.URL)

	_, err := client.Get(context.Background(), mapper, mappingBase+"/master-states.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": "tok-abc"})
	if err == nil {
		t.Fatal("want an error for a no-data upstream response")
	}
	// errors.Is, not ==: the moment any caller wraps this, a == comparison
	// starts silently reporting "broken" for what is actually "no rows".
	if !errors.Is(err, pipeline.ErrNoUpstreamData) {
		t.Errorf("err = %v, want pipeline.ErrNoUpstreamData", err)
	}
}

// secretToken is distinctive enough that finding it anywhere in an error is
// unambiguous rather than a coincidental substring.
const secretToken = "tok-SECRET-do-not-leak-7f3a9c"

// TestPipelineClient_HTTPGet_NeverLeaksTheTokenInAnError is the test the
// comments throughout client.go were asserting and nothing was checking.
//
// This upstream carries its auth token in the QUERY STRING and echoes the
// request back inside error bodies, so the obvious implementations all leak:
// quoting a response body leaks it, wrapping Go's transport error leaks it
// (Go quotes the whole URL), and printing the decoded response leaks it. Each
// case below is one of those routes.
func TestPipelineClient_HTTPGet_NeverLeaksTheTokenInAnError(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	// An error body that echoes the request back, token and all -- this is
	// what the real upstream does.
	echoRequest := func(w http.ResponseWriter, r *http.Request, status int) {
		http.Error(w, `{"error":"bad request","received":"`+r.URL.String()+`"}`, status)
	}

	cases := map[string]http.HandlerFunc{
		"400 echoing the request": func(w http.ResponseWriter, r *http.Request) {
			echoRequest(w, r, http.StatusBadRequest)
		},
		"500 echoing the request": func(w http.ResponseWriter, r *http.Request) {
			echoRequest(w, r, http.StatusInternalServerError)
		},
		"non-JSON body echoing the request": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("upstream is down; your request was " + r.URL.String()))
		},
		"JSON object instead of an array": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"message":"no data found","received":"` + r.URL.String() + `"}`))
		},
	}

	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(handler)
			defer upstream.Close()

			client := pipeline.NewClient(upstream.URL)

			_, err := client.Get(context.Background(), mapper,
				mappingBase+"/master-states.yaml", "/v1/fetch-agmarknet-master-data",
				map[string]any{"token": secretToken})
			if err == nil {
				t.Fatal("want an error")
			}
			assertNoTokenLeak(t, err)
		})
	}

	t.Run("transport failure", func(t *testing.T) {
		// A closed listener: Go's own transport error quotes the whole URL,
		// which is exactly why get() rebuilds its message from the path.
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		addr := upstream.URL
		upstream.Close()

		client := pipeline.NewClient(addr)
		_, err := client.Get(context.Background(), mapper,
			mappingBase+"/master-states.yaml", "/v1/fetch-agmarknet-master-data",
			map[string]any{"token": secretToken})
		if err == nil {
			t.Fatal("want an error from an unreachable upstream")
		}
		assertNoTokenLeak(t, err)
	})
}

// assertNoTokenLeak checks every way an error commonly reaches a log or a
// ticket, not just err.Error().
func assertNoTokenLeak(t *testing.T, err error) {
	t.Helper()
	for _, form := range []string{err.Error(), fmt.Sprint(err), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
		if strings.Contains(form, secretToken) {
			t.Errorf("the token reached an error message: %s", form)
		}
	}
}

// TestPipelineClient_HTTPGet_RefusesANonArrayResponse covers the guard that
// stops an upstream error object from becoming a phantom market.
//
// JSONata's $map over an object yields one element whose every field is
// undefined, which Go then decodes as a zero-valued row -- marketId 0 -- that
// would flow on and be published as a real resource.
func TestPipelineClient_HTTPGet_RefusesANonArrayResponse(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"no data found"}`))
	}))
	defer upstream.Close()

	client := pipeline.NewClient(upstream.URL)

	_, err := client.Get(context.Background(), mapper, mappingBase+"/master-states.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": secretToken})
	if err == nil {
		t.Fatal("an object response was accepted where an array was required")
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("error %q does not name the shape that was received", err)
	}
	if strings.Contains(err.Error(), "no data found") {
		t.Errorf("error %q quotes the response body", err)
	}
}
