package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ptr(f float64) *float64 { return &f }

func TestCoordinateQualityVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon *float64
		want     string
	}{
		{"a real market", ptr(18.609873934158966), ptr(74.69368182799322), "ok"},
		{"both absent", nil, nil, "missing"},
		{"latitude absent", nil, ptr(74.6), "missing"},
		{"longitude absent", ptr(18.6), nil, "missing"},
		// 31 rows upstream carry the same number twice -- a copy-paste defect.
		// The resulting point is not in India and would present as a mandi.
		{"equal, the upstream copy-paste", ptr(13.385784), ptr(13.385784), "suspect"},
		{"outside India", ptr(51.5), ptr(-0.12), "outOfBounds"},
		// 0,0 is a real coordinate in the Gulf of Guinea, so it is out of
		// bounds rather than treated as absent.
		{"null island", ptr(0), ptr(0), "suspect"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := coordinateQuality(tc.lat, tc.lon); got != tc.want {
				t.Errorf("coordinateQuality = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMarketsMapsRowsAndTrimsNames(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Real rows, including the two defects: a null coordinate pair, and a
		// district name with the trailing space the upstream ships.
		_, _ = w.Write([]byte(`[
			{"market_id":1282,"state_name":"Maharashtra","market_name":"Jamkhed APMC",
			 "district_name":"Ahmednagar","market_latitude":"18.609873934158966",
			 "market_longitude":"74.69368182799322","agm_market_center_code":1430},
			{"market_id":4714,"state_name":"Andaman and Nicobar","market_name":"Markets update test APMC",
			 "district_name":"North and Middle Andaman ","market_latitude":null,
			 "market_longitude":null,"agm_market_center_code":20157}
		]`))
	}))
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

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	markets, err := client.Markets(ctx, mapper, base, "t-123")
	if err != nil {
		t.Fatalf("Markets: %v", err)
	}
	if len(markets) != 2 {
		t.Fatalf("got %d markets, want 2", len(markets))
	}

	// The upstream sends coordinates as STRINGS; they must arrive as numbers.
	if markets[0].Latitude == nil || *markets[0].Latitude != 18.609873934158966 {
		t.Errorf("markets[0].Latitude = %v, want the parsed number", markets[0].Latitude)
	}
	// Absent, not zero: a market at 0,0 and a market with no coordinate are
	// different facts.
	if markets[1].Latitude != nil {
		t.Errorf("markets[1].Latitude = %v, want nil", *markets[1].Latitude)
	}
	if markets[1].DistrictName != "North and Middle Andaman" {
		t.Errorf("districtName = %q, want it trimmed", markets[1].DistrictName)
	}
}

func TestMarketsDropsANonNumericCoordinateInsteadOfFailingTheWholeMap(t *testing.T) {
	// A truthy but non-numeric string such as "N/A" must not reach $number():
	// that throws D3030 and, before this guard, failed the entire $map --
	// costing the whole all-India market collection for one bad cell.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"market_id":1282,"state_name":"Maharashtra","market_name":"Jamkhed APMC",
			 "district_name":"Ahmednagar","market_latitude":"18.609873934158966",
			 "market_longitude":"74.69368182799322","agm_market_center_code":1430},
			{"market_id":9001,"state_name":"Maharashtra","market_name":"Bad Coordinate APMC",
			 "district_name":"Ahmednagar","market_latitude":"N/A",
			 "market_longitude":"N/A","agm_market_center_code":9999}
		]`))
	}))
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

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	markets, err := client.Markets(ctx, mapper, base, "t-123")
	if err != nil {
		t.Fatalf("Markets: %v, want the whole map to survive one bad coordinate", err)
	}
	if len(markets) != 2 {
		t.Fatalf("got %d markets, want 2 -- one bad cell must not drop the whole response", len(markets))
	}
	if markets[0].Latitude == nil || *markets[0].Latitude != 18.609873934158966 {
		t.Errorf("markets[0].Latitude = %v, want the parsed number unaffected", markets[0].Latitude)
	}
	if markets[1].Latitude != nil || markets[1].Longitude != nil {
		t.Errorf("markets[1] = %+v, want a non-numeric coordinate dropped like null", markets[1])
	}
}

func TestStateMarketsCarriesEveryCodeASelectNeeds(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"mkt_name":"Jamkhed APMC","market_id":1282,"state_code":"MH",
			 "state_name":"Maharashtra","district_id":338,"district_name":"Ahmednagar",
			 "cmdt_details":[{"cmdt_id":4,"cmdt_name":"Maize"},{"cmdt_id":5,"cmdt_name":"Jowar(Sorghum)"}]}
		]`))
	}))
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

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	rows, err := client.StateMarkets(ctx, mapper, base, "t-123", "MH", "01-07-2026", "01-12-2026")
	if err != nil {
		t.Fatalf("StateMarkets: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	// These four values ARE the contract: they map 1:1 onto the price call's
	// marketcode, districtcode, statecode and commoditycode.
	row := rows[0]
	if row.MarketID != 1282 || row.DistrictID != 338 || row.StateCode != "MH" {
		t.Errorf("codes = market %d district %d state %q, want 1282/338/MH",
			row.MarketID, row.DistrictID, row.StateCode)
	}
	if len(row.Commodities) != 2 || row.Commodities[0].Code != 4 {
		t.Errorf("commodities = %+v, want Maize code 4 first", row.Commodities)
	}

	for _, want := range []string{"statecode=MH", "option=6", "from_date=01-07-2026", "to_date=01-12-2026"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestStateMarketsSurvivesAMarketWithOneCommodity(t *testing.T) {
	// The nested collapse: cmdt_details with one entry is the inner sequence
	// JSONata flattens, and it is a different wrap from the outer one.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"mkt_name":"One APMC","market_id":7,"state_code":"MH","state_name":"Maharashtra",
			 "district_id":338,"district_name":"Ahmednagar",
			 "cmdt_details":[{"cmdt_id":4,"cmdt_name":"Maize"}]}
		]`))
	}))
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

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	rows, err := client.StateMarkets(ctx, mapper, base, "t-123", "MH", "01-07-2026", "01-12-2026")
	if err != nil {
		t.Fatalf("StateMarkets: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Commodities) != 1 || rows[0].Commodities[0].Name != "Maize" {
		t.Fatalf("got %+v, want one market with one commodity", rows)
	}
}

func TestStateMarketsRefusesAStateNameInPlaceOfACode(t *testing.T) {
	// The mapping's required: guard, and the reason call() runs Verify. Sending
	// "Maharashtra" where MH belongs earns a 200 with an empty body from this
	// upstream, which reads as "this state has no markets" -- a wrong answer
	// that looks like a right one.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the guard should have refused before any request was made")
		_, _ = w.Write([]byte(`[]`))
	}))
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

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	_, err = client.StateMarkets(ctx, mapper, base, "t-123", "Maharashtra", "01-07-2026", "01-12-2026")
	if err == nil {
		t.Fatal("want a refusal for a state name")
	}
	// The mapping's own message, so the fix is stated rather than inferred.
	if !strings.Contains(err.Error(), "short alphabetic") {
		t.Errorf("error should carry the mapping's message, got %v", err)
	}

	// And the same guard on the date format.
	if _, err := client.StateMarkets(ctx, mapper, base, "t-123", "MH", "2026-07-01", "2026-12-01"); err == nil {
		t.Fatal("want a refusal for ISO dates where dd-MM-yyyy belongs")
	}
}

func TestJoinAttachesCoordinatesByMarketID(t *testing.T) {
	rows := []StateMarket{
		{MarketID: 1282, MarketName: "Jamkhed APMC", StateCode: "MH", StateName: "Maharashtra",
			DistrictID: 338, DistrictName: "Ahmednagar",
			Commodities: []Commodity{{Code: 4, Name: "Maize"}}},
		{MarketID: 999, MarketName: "Absent APMC", StateCode: "MH", StateName: "Maharashtra",
			DistrictID: 338, DistrictName: "Ahmednagar",
			Commodities: []Commodity{{Code: 4, Name: "Maize"}}},
	}
	markets := []Market{
		{MarketID: 1282, MarketName: "Jamkhed APMC", Latitude: ptr(18.609873934158966), Longitude: ptr(74.69368182799322)},
	}

	got := join(rows, markets)
	if len(got) != 2 {
		t.Fatalf("got %d markets, want 2 -- a market absent from master data is kept, not dropped", len(got))
	}
	if got[0].CoordinateQuality != coordinateOK || got[0].Latitude == nil {
		t.Errorf("got[0] = %+v, want ok with coordinates", got[0])
	}
	// A mapping row with no master counterpart keeps every code it has; only
	// the geometry is unknown. Dropping it would lose a real, selectable market.
	if got[1].CoordinateQuality != coordinateMissing || got[1].MarketID != 999 {
		t.Errorf("got[1] = %+v, want the market kept and marked missing", got[1])
	}
	if got[1].Latitude != nil {
		t.Error("a market with no master row must carry no coordinates")
	}
}

func TestJoinDropsCoordinatesItRefuses(t *testing.T) {
	rows := []StateMarket{{MarketID: 1988, MarketName: "Nagalapuram APMC", StateCode: "AP", DistrictID: 12}}
	markets := []Market{{MarketID: 1988, Latitude: ptr(13.385784), Longitude: ptr(13.385784)}}

	got := join(rows, markets)
	if got[0].CoordinateQuality != coordinateSuspect {
		t.Fatalf("quality = %q, want suspect", got[0].CoordinateQuality)
	}
	// A verdict other than ok means the coordinates are not published. Carrying
	// them anyway invites a consumer to use them.
	if got[0].Latitude != nil || got[0].Longitude != nil {
		t.Error("a suspect coordinate must not be carried on the output")
	}
}

func TestCollectionSerializesToTheAgreedContract(t *testing.T) {
	c := Collection{
		GeneratedAt: "2026-09-10T09:14:22Z",
		Window:      Window{From: "01-07-2026", To: "01-12-2026"},
		Markets: []CollectedMarket{{
			MarketID: 1282, MarketName: "Jamkhed APMC", StateCode: "MH", StateName: "Maharashtra",
			DistrictID: 338, DistrictName: "Ahmednagar",
			Latitude: ptr(18.5), Longitude: ptr(74.5), CoordinateQuality: "ok",
			Commodities: []Commodity{{Code: 4, Name: "Maize"}},
		}},
		StateErrors: []StateError{},
		EmptyStates: []string{},
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"generatedAt":"2026-09-10T09:14:22Z","window":{"from":"01-07-2026","to":"01-12-2026"},` +
		`"markets":[{"marketId":1282,"marketName":"Jamkhed APMC","stateCode":"MH","stateName":"Maharashtra",` +
		`"districtId":338,"districtName":"Ahmednagar","latitude":18.5,"longitude":74.5,` +
		`"coordinateQuality":"ok","commodities":[{"code":4,"name":"Maize"}]}],"stateErrors":[],"emptyStates":[]}`
	if string(out) != want {
		t.Errorf("contract drifted:\n got %s\nwant %s", out, want)
	}
}

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
