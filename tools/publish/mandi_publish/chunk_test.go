package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marketsIn builds n markets for one state, each with a coordinate and one
// commodity, so a test can say only how many it wants.
func marketsIn(state string, n int) []CollectedMarket {
	out := make([]CollectedMarket, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, CollectedMarket{
			MarketID:          1000 + i,
			MarketName:        fmt.Sprintf("Market %d", 1000+i),
			StateCode:         state,
			StateName:         "Tamil Nadu",
			DistrictID:        527,
			DistrictName:      "Ariyalur",
			Latitude:          floatPtr(11.0 + float64(i)/1000),
			Longitude:         floatPtr(78.0 + float64(i)/1000),
			CoordinateQuality: coordinateOK,
			Commodities:       []Commodity{{Code: 4, Name: "Maize"}},
		})
	}
	return out
}

// buildInto runs the builder over one collection and returns what it wrote.
func buildInto(t *testing.T, dir string, markets []CollectedMarket) ([]BuiltState, SkipSummary) {
	t.Helper()

	mappingBase, stop, err := serveMappings()
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

	collection := Collection{
		GeneratedAt: "2026-09-11T04:05:00Z",
		Window:      Window{From: "01-07-2026", To: "01-12-2026"},
		Markets:     markets,
	}
	cfg := buildConfig{catalogOut: dir, participantID: "agmarknet-mock", networkID: "oan-dev"}

	built, summary, err := buildFromCollection(ctx, collection, cfg, mapper, mappingBase)
	if err != nil {
		t.Fatalf("buildFromCollection: %v", err)
	}
	return built, summary
}

// resourceIDs reads the ids a written catalog carries.
func resourceIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var envelope struct {
		Message struct {
			Catalogs []struct {
				ID        string `json:"id"`
				Resources []struct {
					ID                 string `json:"id"`
					ResourceAttributes struct {
						CoverageAreas []struct {
							Type string `json:"type"`
						} `json:"coverageAreas"`
					} `json:"resourceAttributes"`
				} `json:"resources"`
			} `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	var ids []string
	for _, resource := range envelope.Message.Catalogs[0].Resources {
		ids = append(ids, resource.ID)
	}
	return ids
}

func TestStateWithinTheGeometryBudgetStaysOneUnsuffixedCatalog(t *testing.T) {
	// 256 markets with coordinates is 256 geometries -- exactly the limit, so
	// it must still be one catalog. Splitting at the boundary would suffix a
	// state that did not need it.
	dir := t.TempDir()
	built, _ := buildInto(t, dir, marketsIn("TN", catalogGeometryBudget))

	if len(built) != 1 {
		t.Fatalf("built %d catalogs, want 1 at exactly the budget", len(built))
	}
	if built[0].CatalogID != "agmarknet-mock/mandi-TN" {
		t.Errorf("catalog id = %q, want no chunk suffix", built[0].CatalogID)
	}
	if filepath.Base(built[0].Path) != "mandi-TN.json" {
		t.Errorf("file = %q, want mandi-TN.json", filepath.Base(built[0].Path))
	}
}

func TestStateOverTheGeometryBudgetSplits(t *testing.T) {
	dir := t.TempDir()
	built, _ := buildInto(t, dir, marketsIn("TN", catalogGeometryBudget+1))

	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want 2 one market past the budget", len(built))
	}
	wantCounts := []int{catalogGeometryBudget, 1}
	for i, state := range built {
		wantID := fmt.Sprintf("agmarknet-mock/mandi-TN-%d", i+1)
		if state.CatalogID != wantID {
			t.Errorf("catalog %d id = %q, want %q", i, state.CatalogID, wantID)
		}
		if state.Markets != wantCounts[i] {
			t.Errorf("catalog %d carries %d markets, want %d", i, state.Markets, wantCounts[i])
		}
		if state.StateCode != "TN" {
			t.Errorf("catalog %d state = %q, want TN", i, state.StateCode)
		}
	}
}

func TestMarketsWithoutCoordinatesDoNotSpendBudget(t *testing.T) {
	// A market with no usable coordinate publishes no geometry, so it costs
	// nothing against the limit. Counting markets rather than geometries would
	// split a state that fits comfortably -- Tamil Nadu has 138 markets and
	// only 61 coordinates.
	markets := marketsIn("TN", catalogGeometryBudget+50)
	for i := catalogGeometryBudget; i < len(markets); i++ {
		markets[i].CoordinateQuality = coordinateMissing
		markets[i].Latitude, markets[i].Longitude = nil, nil
	}

	dir := t.TempDir()
	built, summary := buildInto(t, dir, markets)

	if len(built) != 1 {
		t.Fatalf("built %d catalogs, want 1: only %d markets carry geometry",
			len(built), catalogGeometryBudget)
	}
	if built[0].Markets != catalogGeometryBudget+50 {
		t.Errorf("catalog carries %d markets, want all %d",
			built[0].Markets, catalogGeometryBudget+50)
	}
	if summary.GeometryLess != 50 {
		t.Errorf("GeometryLess = %d, want 50", summary.GeometryLess)
	}
}

func TestChunksPartitionTheMarketsExactlyOnce(t *testing.T) {
	// A market lost at a chunk boundary is invisible and nothing downstream
	// would notice, so the partition is asserted directly.
	dir := t.TempDir()
	built, _ := buildInto(t, dir, marketsIn("TN", 400))

	seen := map[string]int{}
	for _, state := range built {
		for _, id := range resourceIDs(t, state.Path) {
			seen[id]++
		}
	}
	if len(seen) != 400 {
		t.Errorf("%d distinct resources across chunks, want 400", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times across chunks", id, count)
		}
	}
}

func TestEveryChunkStaysUnderTheGeometryCap(t *testing.T) {
	const discoveryGeometryCap = 256

	dir := t.TempDir()
	built, _ := buildInto(t, dir, marketsIn("TN", 400))

	for _, state := range built {
		data, err := os.ReadFile(state.Path)
		if err != nil {
			t.Fatalf("read %s: %v", state.Path, err)
		}
		// Every Point in the payload counts against the cap, wherever it sits.
		geometries := strings.Count(string(data), `"type": "Point"`) +
			strings.Count(string(data), `"type":"Point"`)
		if geometries > discoveryGeometryCap {
			t.Errorf("%s carries %d geometries, over the %d cap",
				state.CatalogID, geometries, discoveryGeometryCap)
		}
		if geometries == 0 {
			t.Errorf("%s carries no geometries at all", state.CatalogID)
		}
	}
}

func TestExcludedMarketsAreReportedWithTheirReason(t *testing.T) {
	// A market the upstream reports no commodities for cannot be published:
	// supportedCommodities has minItems 1, and one such resource fails the
	// whole catalog. Excluding it silently would hide a market from the
	// network with no record of why.
	markets := marketsIn("TN", 2)
	markets[0].Commodities = nil
	markets[0].MarketName = "Barren APMC"

	dir := t.TempDir()
	built, summary := buildInto(t, dir, markets)

	if summary.ZeroCommodities != 1 {
		t.Errorf("ZeroCommodities = %d, want 1", summary.ZeroCommodities)
	}
	if len(summary.Excluded) != 1 {
		t.Fatalf("Excluded = %+v, want one entry", summary.Excluded)
	}
	excluded := summary.Excluded[0]
	if excluded.MarketID != markets[0].MarketID || excluded.MarketName != "Barren APMC" {
		t.Errorf("excluded market = %+v, want the barren one named", excluded)
	}
	if excluded.StateCode != "TN" {
		t.Errorf("excluded state = %q, want TN", excluded.StateCode)
	}
	if !strings.Contains(excluded.Reason, "commodit") {
		t.Errorf("reason = %q, want it to say the commodity list was empty", excluded.Reason)
	}

	// And it is genuinely absent from what gets published.
	for _, id := range resourceIDs(t, built[0].Path) {
		if strings.HasSuffix(id, fmt.Sprint(markets[0].MarketID)) {
			t.Errorf("the barren market was published as %s", id)
		}
	}
}

func TestGeometryLessMarketsAreReportedWithTheirVerdict(t *testing.T) {
	// These ARE published -- they trade, and a district filter still finds
	// them -- but no proximity search can, so the reason has to be visible.
	markets := marketsIn("TN", 4)
	markets[0].CoordinateQuality, markets[0].Latitude, markets[0].Longitude = coordinateMissing, nil, nil
	markets[1].CoordinateQuality = coordinateSuspect
	markets[2].CoordinateQuality = coordinateOutOfBounds

	dir := t.TempDir()
	built, summary := buildInto(t, dir, markets)

	if summary.GeometryLess != 3 {
		t.Errorf("GeometryLess = %d, want 3", summary.GeometryLess)
	}
	if len(summary.GeometryLessMarkets) != 3 {
		t.Fatalf("GeometryLessMarkets = %+v, want 3 entries", summary.GeometryLessMarkets)
	}

	// Each carries its OWN verdict, not a shared label: "missing" and
	// "outOfBounds" are different upstream defects and are fixed differently.
	byID := map[int]string{}
	for _, market := range summary.GeometryLessMarkets {
		byID[market.MarketID] = market.Reason
	}
	for i, want := range map[int]string{0: coordinateMissing, 1: coordinateSuspect, 2: coordinateOutOfBounds} {
		got := byID[markets[i].MarketID]
		if !strings.Contains(got, want) {
			t.Errorf("market %d reason = %q, want it to name %q", markets[i].MarketID, got, want)
		}
	}

	// All four are still in the catalog.
	if got := len(resourceIDs(t, built[0].Path)); got != 4 {
		t.Errorf("published %d resources, want all 4 including the geometry-less", got)
	}
}

func TestResourceCarriesExactlyOneGeometry(t *testing.T) {
	// Every geometry counts against the discovery service's 256-per-catalog
	// limit. The coverageAreas copy is the one S_DWITHIN reads; a second copy
	// in market.location bought the consumer nothing it could not read from
	// coverageAreas, and halved how many markets fit in a catalog.
	dir := t.TempDir()
	built, _ := buildInto(t, dir, marketsIn("TN", 1))

	data, err := os.ReadFile(built[0].Path)
	if err != nil {
		t.Fatalf("read %s: %v", built[0].Path, err)
	}
	if got := strings.Count(string(data), `"type": "Point"`); got != 1 {
		t.Errorf("resource carries %d geometries, want exactly 1", got)
	}
	if strings.Contains(string(data), `"location"`) {
		t.Error("market.location is still published")
	}

	// The coordinate must still reach the consumer, in the place the spatial
	// filter reads.
	var envelope struct {
		Message struct {
			Catalogs []struct {
				Resources []struct {
					ResourceAttributes struct {
						CoverageAreas []struct {
							Type        string    `json:"type"`
							Coordinates []float64 `json:"coordinates"`
						} `json:"coverageAreas"`
					} `json:"resourceAttributes"`
				} `json:"resources"`
			} `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	areas := envelope.Message.Catalogs[0].Resources[0].ResourceAttributes.CoverageAreas
	if len(areas) != 2 || areas[0].Type != "Point" || len(areas[0].Coordinates) != 2 {
		t.Fatalf("coverageAreas = %+v, want a Point then the district reference", areas)
	}
	// GeoJSON order: longitude first. Swapped, a Tamil Nadu market lands in
	// the Arabian Sea and no farmer ever finds it.
	if areas[0].Coordinates[0] < 68 || areas[0].Coordinates[0] > 98 {
		t.Errorf("coordinates = %v, want [longitude, latitude]", areas[0].Coordinates)
	}
}
