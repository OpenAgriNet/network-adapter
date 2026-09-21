package agmarket

import "testing"

func f64(v float64) *float64 { return &v }

func TestCoordinateQuality(t *testing.T) {
	cases := []struct {
		name string
		lat  *float64
		lon  *float64
		want string
	}{
		{"both nil", nil, nil, "missing"},
		{"lat nil only", nil, f64(20), "missing"},
		{"lon nil only", f64(20), nil, "missing"},
		{
			// 31 rows upstream carry the same number in both fields -- a
			// copy-paste defect, not a market on the 45-degree line. This
			// also catches 0,0 ("null island"), a real place, which is why
			// the equal-value check must run before the bounding-box check.
			"equal non-nil values, including null island", f64(0), f64(0), "suspect",
		},
		{"equal non-nil values, nonzero", f64(20), f64(20), "suspect"},
		{"out of bounds", f64(50), f64(50.0001), "outOfBounds"},
		{"out of bounds lat too low", f64(2), f64(80), "outOfBounds"},
		{"out of bounds lon too high", f64(20), f64(120), "outOfBounds"},
		{"normal in-range distinct pair", f64(19.076), f64(72.8777), "ok"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coordinateQuality(tc.lat, tc.lon)
			if got != tc.want {
				t.Errorf("coordinateQuality(%v, %v) = %q, want %q", tc.lat, tc.lon, got, tc.want)
			}
		})
	}
}

func TestJoinMarkets(t *testing.T) {
	rows := []StateMarket{
		{MarketID: 1, MarketName: "No Master Match", StateCode: "MH", StateName: "Maharashtra", DistrictID: 10, DistrictName: "Pune"},
		{MarketID: 2, MarketName: "Has Master Match", StateCode: "MH", StateName: "Maharashtra", DistrictID: 11, DistrictName: "Nashik"},
	}
	markets := []Market{
		{MarketID: 2, MarketName: "Has Master Match", Latitude: f64(19.076), Longitude: f64(72.8777)},
	}

	out := joinMarkets(rows, markets)
	if len(out) != 2 {
		t.Fatalf("joinMarkets returned %d markets, want 2 (unmatched rows must be kept)", len(out))
	}

	var noMatch, hasMatch *CollectedMarket
	for i := range out {
		switch out[i].MarketID {
		case 1:
			noMatch = &out[i]
		case 2:
			hasMatch = &out[i]
		}
	}

	if noMatch == nil {
		t.Fatal("row with no matching market was dropped, want it kept")
	}
	if noMatch.CoordinateQuality != "missing" {
		t.Errorf("unmatched row CoordinateQuality = %q, want %q", noMatch.CoordinateQuality, "missing")
	}
	if noMatch.Latitude != nil || noMatch.Longitude != nil {
		t.Errorf("unmatched row should have nil coordinates, got lat=%v lon=%v", noMatch.Latitude, noMatch.Longitude)
	}

	if hasMatch == nil {
		t.Fatal("row with matching market missing from output")
	}
	if hasMatch.CoordinateQuality != "ok" {
		t.Errorf("matched row CoordinateQuality = %q, want %q", hasMatch.CoordinateQuality, "ok")
	}
	if hasMatch.Latitude == nil || *hasMatch.Latitude != 19.076 {
		t.Errorf("matched row Latitude = %v, want 19.076", hasMatch.Latitude)
	}
	if hasMatch.Longitude == nil || *hasMatch.Longitude != 72.8777 {
		t.Errorf("matched row Longitude = %v, want 72.8777", hasMatch.Longitude)
	}
}

func TestDeriveQuality(t *testing.T) {
	in := []CollectedMarket{
		{MarketID: 1, CoordinateQuality: "suspect", Latitude: f64(0), Longitude: f64(0)},
		{MarketID: 2, CoordinateQuality: "ok", Latitude: f64(19.076), Longitude: f64(72.8777)},
		{MarketID: 3, CoordinateQuality: "outOfBounds", Latitude: f64(50), Longitude: f64(50)},
		{MarketID: 4, CoordinateQuality: "missing"},
	}

	out := deriveQuality(in)
	if len(out) != len(in) {
		t.Fatalf("deriveQuality changed length: got %d, want %d", len(out), len(in))
	}

	for _, m := range out {
		if m.CoordinateQuality == "ok" {
			if m.Latitude == nil || m.Longitude == nil {
				t.Errorf("market %d: ok market lost its coordinates", m.MarketID)
			}
			continue
		}
		if m.Latitude != nil || m.Longitude != nil {
			t.Errorf("market %d: quality %q should have nil coordinates, got lat=%v lon=%v",
				m.MarketID, m.CoordinateQuality, m.Latitude, m.Longitude)
		}
	}
}

func TestDedupeByMarketID(t *testing.T) {
	in := []CollectedMarket{
		{MarketID: 1, MarketName: "first-one"},
		{MarketID: 2, MarketName: "first-two"},
		{MarketID: 1, MarketName: "duplicate-one"},
		{MarketID: 3, MarketName: "first-three"},
		{MarketID: 2, MarketName: "duplicate-two"},
	}

	out := dedupeByMarketID(in)
	if len(out) != 3 {
		t.Fatalf("dedupeByMarketID returned %d markets, want 3", len(out))
	}

	wantOrder := []struct {
		id   int
		name string
	}{
		{1, "first-one"},
		{2, "first-two"},
		{3, "first-three"},
	}
	for i, want := range wantOrder {
		if out[i].MarketID != want.id || out[i].MarketName != want.name {
			t.Errorf("out[%d] = %+v, want MarketID=%d MarketName=%q (first occurrence must win, order preserved)",
				i, out[i], want.id, want.name)
		}
	}
}

func TestGeometryCost(t *testing.T) {
	cases := []struct {
		name   string
		market CollectedMarket
		want   int
	}{
		{"ok with coordinates", CollectedMarket{CoordinateQuality: "ok", Latitude: f64(19), Longitude: f64(72)}, 1},
		{"ok but nil latitude", CollectedMarket{CoordinateQuality: "ok", Latitude: nil, Longitude: f64(72)}, 0},
		{"ok but nil longitude", CollectedMarket{CoordinateQuality: "ok", Latitude: f64(19), Longitude: nil}, 0},
		{"missing", CollectedMarket{CoordinateQuality: "missing"}, 0},
		{"suspect", CollectedMarket{CoordinateQuality: "suspect", Latitude: f64(0), Longitude: f64(0)}, 0},
		{"outOfBounds", CollectedMarket{CoordinateQuality: "outOfBounds", Latitude: f64(50), Longitude: f64(50)}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := geometryCost(tc.market)
			if got != tc.want {
				t.Errorf("geometryCost(%+v) = %d, want %d", tc.market, got, tc.want)
			}
		})
	}
}

func TestChunkMarkets(t *testing.T) {
	t.Run("splits to stay within budget", func(t *testing.T) {
		var markets []CollectedMarket
		for i := 0; i < 5; i++ {
			markets = append(markets, CollectedMarket{
				MarketID: i, CoordinateQuality: "ok", Latitude: f64(19), Longitude: f64(72),
			})
		}
		chunks := chunkMarkets(markets, 2)
		if len(chunks) != 3 {
			t.Fatalf("got %d chunks, want 3 (budget 2, 5 markets each costing 1)", len(chunks))
		}
		total := 0
		for _, c := range chunks {
			cost := 0
			for _, m := range c {
				cost += geometryCost(m)
			}
			if cost > 2 {
				t.Errorf("chunk exceeded budget: cost %d > 2", cost)
			}
			total += len(c)
		}
		if total != 5 {
			t.Errorf("chunks together hold %d markets, want 5", total)
		}
	})

	t.Run("geometry-less markets never force a split", func(t *testing.T) {
		var markets []CollectedMarket
		for i := 0; i < 1000; i++ {
			markets = append(markets, CollectedMarket{MarketID: i, CoordinateQuality: "missing"})
		}
		chunks := chunkMarkets(markets, 2)
		if len(chunks) != 1 {
			t.Fatalf("got %d chunks, want 1 (geometry-less markets must stay in one chunk regardless of budget)", len(chunks))
		}
		if len(chunks[0]) != 1000 {
			t.Errorf("chunk holds %d markets, want 1000", len(chunks[0]))
		}
	})

	t.Run("mixed geometry and geometry-less markets", func(t *testing.T) {
		markets := []CollectedMarket{
			{MarketID: 1, CoordinateQuality: "ok", Latitude: f64(19), Longitude: f64(72)},
			{MarketID: 2, CoordinateQuality: "missing"},
			{MarketID: 3, CoordinateQuality: "missing"},
			{MarketID: 4, CoordinateQuality: "ok", Latitude: f64(19), Longitude: f64(72)},
			{MarketID: 5, CoordinateQuality: "ok", Latitude: f64(19), Longitude: f64(72)},
		}
		chunks := chunkMarkets(markets, 2)
		// costs: 1,0,0,1,1 -> running spent: 1,1,1,2,(3>2 split) -> [1,2,3,4],[5]
		if len(chunks) != 2 {
			t.Fatalf("got %d chunks, want 2", len(chunks))
		}
		if len(chunks[0]) != 4 || len(chunks[1]) != 1 {
			t.Errorf("chunk sizes = %d,%d want 4,1", len(chunks[0]), len(chunks[1]))
		}
	})
}
