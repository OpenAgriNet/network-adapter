package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstream serves all three calls, switching on path and option, so one
// server exercises the whole run.
func fakeUpstream(t *testing.T, failState string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-dynamic-token-agmarknet"):
			_, _ = w.Write([]byte(`{"token":"t-123"}`))
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-master-data"):
			switch r.URL.Query().Get("option") {
			case "4":
				_, _ = w.Write([]byte(`[
					{"state_id":20,"state_name":"Maharashtra","agm_state_code":"MH"},
					{"state_id":7,"state_name":"Chattisgarh","agm_state_code":"CG"}
				]`))
			case "6":
				_, _ = w.Write([]byte(`[
					{"market_id":1282,"state_name":"Maharashtra","market_name":"Jamkhed APMC",
					 "district_name":"Ahmednagar","market_latitude":"18.609873934158966",
					 "market_longitude":"74.69368182799322","agm_market_center_code":1430}
				]`))
			default:
				http.Error(w, "unexpected option", http.StatusBadRequest)
			}
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-market-commodity-mapping"):
			if state := r.URL.Query().Get("statecode"); state == failState {
				http.Error(w, "upstream is unwell", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`[
				{"mkt_name":"Jamkhed APMC","market_id":1282,"state_code":"MH",
				 "state_name":"Maharashtra","district_id":338,"district_name":"Ahmednagar",
				 "cmdt_details":[{"cmdt_id":4,"cmdt_name":"Maize"}]}
			]`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
}

func TestCollectRunsEndToEnd(t *testing.T) {
	upstream := fakeUpstream(t, "")
	defer upstream.Close()

	got, err := collect(context.Background(), config{
		baseURL: upstream.URL, user: "u", secret: "p",
		states: []string{"MH"}, fromDate: "01-07-2026", toDate: "01-12-2026",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got.Markets) != 1 || got.Markets[0].MarketID != 1282 {
		t.Fatalf("markets = %+v", got.Markets)
	}
	if got.Markets[0].CoordinateQuality != coordinateOK {
		t.Errorf("quality = %q, want ok", got.Markets[0].CoordinateQuality)
	}
	if got.Window.From != "01-07-2026" {
		t.Errorf("window = %+v", got.Window)
	}
	if len(got.StateErrors) != 0 {
		t.Errorf("stateErrors = %+v, want none", got.StateErrors)
	}
}

func TestCollectWithNoStatesUsesEveryStateFromMasterData(t *testing.T) {
	// Distinct markets per state, so the assertion proves both MH and CG were
	// actually walked -- separate from and not confused with deduplication,
	// which TestCollectDedupesMarketsAcrossDuplicateStates covers on its own.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-dynamic-token-agmarknet"):
			_, _ = w.Write([]byte(`{"token":"t-123"}`))
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-master-data"):
			switch r.URL.Query().Get("option") {
			case "4":
				_, _ = w.Write([]byte(`[
					{"state_id":20,"state_name":"Maharashtra","agm_state_code":"MH"},
					{"state_id":7,"state_name":"Chattisgarh","agm_state_code":"CG"}
				]`))
			case "6":
				_, _ = w.Write([]byte(`[]`))
			default:
				http.Error(w, "unexpected option", http.StatusBadRequest)
			}
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-market-commodity-mapping"):
			marketID := "1282"
			if r.URL.Query().Get("statecode") == "CG" {
				marketID = "555"
			}
			_, _ = w.Write([]byte(`[
				{"mkt_name":"Some APMC","market_id":` + marketID + `,"state_code":"` +
				r.URL.Query().Get("statecode") + `","state_name":"x","district_id":1,
				 "district_name":"x","cmdt_details":[{"cmdt_id":4,"cmdt_name":"Maize"}]}
			]`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	got, err := collect(context.Background(), config{
		baseURL: upstream.URL, user: "u", secret: "p",
		fromDate: "01-07-2026", toDate: "01-12-2026",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	// Both MH (1282) and CG (555) were fetched and kept -- distinct markets,
	// so this is unaffected by deduplication.
	if len(got.Markets) != 2 {
		t.Fatalf("got %d markets, want one per state fetched", len(got.Markets))
	}
}

func TestCollectKeepsGoingWhenOneStateFails(t *testing.T) {
	// One bad state must not lose the other 35.
	upstream := fakeUpstream(t, "CG")
	defer upstream.Close()

	got, err := collect(context.Background(), config{
		baseURL: upstream.URL, user: "u", secret: "p",
		fromDate: "01-07-2026", toDate: "01-12-2026",
	})
	if err != nil {
		t.Fatalf("collect must not fail the run for one state: %v", err)
	}
	if len(got.Markets) != 1 {
		t.Errorf("got %d markets, want the good state's", len(got.Markets))
	}
	if len(got.StateErrors) != 1 || got.StateErrors[0].StateCode != "CG" {
		t.Fatalf("stateErrors = %+v, want CG recorded", got.StateErrors)
	}
	// The reason is for a human reading the file later.
	if !strings.Contains(got.StateErrors[0].Reason, "500") {
		t.Errorf("reason = %q, want the status in it", got.StateErrors[0].Reason)
	}
}

func TestCollectRefusesMissingCredentials(t *testing.T) {
	_, err := collect(context.Background(), config{baseURL: "http://127.0.0.1:1", user: "", secret: ""})
	if err == nil {
		t.Fatal("want an error when the credentials are unset")
	}
	// The variable names are the operator's fix, so they belong in this
	// message -- unlike the adapter's, which answers a network peer.
	if !strings.Contains(err.Error(), "MANDI_TOKEN_USER") {
		t.Errorf("error should name the variables to set, got %v", err)
	}
}

func TestCollectFailsWhenStateListResolvesToZero(t *testing.T) {
	// An empty state list must be fatal: with no --states given, collect walks
	// whatever client.States() returns, and a zero-length answer would
	// otherwise produce a well-formed, zero-error {"markets":[],...} document
	// that looks like a complete run of an India with no markets.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-dynamic-token-agmarknet"):
			_, _ = w.Write([]byte(`{"token":"t-123"}`))
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-master-data") && r.URL.Query().Get("option") == "4":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected call to %s once the state list is empty", r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	_, err := collect(context.Background(), config{
		baseURL: upstream.URL, user: "u", secret: "p",
		fromDate: "01-07-2026", toDate: "01-12-2026",
	})
	if err == nil {
		t.Fatal("want an error when the state list resolves to zero states")
	}
}

func TestCollectRecordsAStateThatSucceedsWithZeroMarkets(t *testing.T) {
	// A state fetch that succeeds but returns no rows is suspicious, not
	// fatal -- it must be visible in the output rather than silently
	// indistinguishable from a state that has genuinely nothing trading.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-dynamic-token-agmarknet"):
			_, _ = w.Write([]byte(`{"token":"t-123"}`))
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-master-data") && r.URL.Query().Get("option") == "6":
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/fetch-agmarknet-market-commodity-mapping"):
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	got, err := collect(context.Background(), config{
		baseURL: upstream.URL, user: "u", secret: "p",
		states: []string{"MH"}, fromDate: "01-07-2026", toDate: "01-12-2026",
	})
	if err != nil {
		t.Fatalf("collect must not fail the run for a state with zero markets: %v", err)
	}
	if len(got.Markets) != 0 {
		t.Errorf("markets = %+v, want none", got.Markets)
	}
	if len(got.EmptyStates) != 1 || got.EmptyStates[0] != "MH" {
		t.Fatalf("emptyStates = %+v, want [MH]", got.EmptyStates)
	}
	if len(got.StateErrors) != 0 {
		t.Errorf("stateErrors = %+v, want none -- an empty state is not a failure", got.StateErrors)
	}
}

func TestCollectDedupesMarketsAcrossDuplicateStates(t *testing.T) {
	// --states MH,MH must not emit market 1282 twice: Plan 1 makes one catalog
	// resource per marketId, and a duplicate here becomes a duplicate ID there.
	upstream := fakeUpstream(t, "")
	defer upstream.Close()

	got, err := collect(context.Background(), config{
		baseURL: upstream.URL, user: "u", secret: "p",
		states: []string{"MH", "MH"}, fromDate: "01-07-2026", toDate: "01-12-2026",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got.Markets) != 1 {
		t.Fatalf("got %d markets, want 1 -- market 1282 must be deduplicated", len(got.Markets))
	}
	if got.Markets[0].MarketID != 1282 {
		t.Errorf("market = %+v", got.Markets[0])
	}
}
