package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The upstream reports an empty result as an HTTP 400 carrying this body.
// Measured against the dev service on 2026-09-11: 27 of 36 states answer this
// way because it holds market-commodity mapping rows for 9 states only, while
// their markets are all present in the master data.
const upstreamNoDataBody = `{"success":false,"message":"No data available."}`

// noDataUpstream answers the mapping call the way the real service does for a
// state it has no rows for, and answers everything else normally.
func noDataUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "generate-dynamic-token"):
			_, _ = w.Write([]byte(`{"token":"45c313a8-5178-434b-a4d7-dfb78f47ac35"}`))
		case strings.Contains(r.URL.Path, "market-commodity-mapping"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(upstreamNoDataBody))
		case strings.Contains(r.URL.Path, "master-data"):
			switch r.URL.Query().Get("option") {
			case "4":
				_, _ = w.Write([]byte(`[{"state_id":32,"state_name":"Kerala","agm_state_code":"KL"}]`))
			case "6":
				_, _ = w.Write([]byte(`[{"market_id":1,"state_name":"Kerala","market_name":"Kochi APMC",
					"district_name":"Ernakulam","market_latitude":"9.9","market_longitude":"76.2",
					"agm_market_center_code":1}]`))
			default:
				http.Error(w, "unexpected option", http.StatusBadRequest)
			}
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
}

func TestStateMarketsReportsNoDataDistinctly(t *testing.T) {
	upstream := noDataUpstream(t)
	defer upstream.Close()

	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	client := newClient(upstream.URL)
	_, err = client.StateMarkets(ctx, mapper, base, "token", "KL", "01-07-2026", "01-12-2026")

	// "No data available." is the upstream's way of saying the result is
	// empty. option=7 proves malformed input answers differently
	// ({"error":"Option must be between 1 and 6"}), so this is semantic and
	// must not be reported as a failed call.
	if !errors.Is(err, errNoUpstreamData) {
		t.Fatalf("err = %v, want errNoUpstreamData", err)
	}
}

func TestNoDataErrorNeverQuotesTheToken(t *testing.T) {
	// The token rides in the query string and this error reaches a terminal
	// and gets pasted into tickets.
	upstream := noDataUpstream(t)
	defer upstream.Close()

	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	client := newClient(upstream.URL)
	_, err = client.StateMarkets(ctx, mapper, base, "s3cr3t-token-value", "KL", "01-07-2026", "01-12-2026")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "s3cr3t-token-value") {
		t.Errorf("error quotes the token: %q", err)
	}
}

func TestCollectCountsANoDataStateAsEmptyNotFailed(t *testing.T) {
	// A state the upstream has no rows for is a coverage fact, not a broken
	// run. Recording it as a stateError makes 27 states look like 27 outages
	// and exits non-zero on a collection that is exactly as complete as the
	// upstream allows.
	upstream := noDataUpstream(t)
	defer upstream.Close()

	collection, err := collect(context.Background(), config{
		baseURL:  upstream.URL,
		user:     "user",
		secret:   "secret",
		states:   []string{"KL"},
		fromDate: "01-07-2026",
		toDate:   "01-12-2026",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if len(collection.StateErrors) != 0 {
		t.Errorf("stateErrors = %+v, want none", collection.StateErrors)
	}
	if len(collection.EmptyStates) != 1 || collection.EmptyStates[0] != "KL" {
		t.Errorf("emptyStates = %v, want [KL]", collection.EmptyStates)
	}
	if len(collection.Markets) != 0 {
		t.Errorf("markets = %d, want none", len(collection.Markets))
	}
}
