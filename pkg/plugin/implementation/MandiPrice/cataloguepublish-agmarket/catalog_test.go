package agmarket

// catalog_test.go proves the collection-to-catalogue half of the pipeline:
// grouping by state, the two exclusion rules, deterministic ordering, the
// geometry-budget split, and the render through the REAL embedded
// mappings/catalog.yaml. The render is asserted by unmarshalling the result
// and reading the catalog and its resources out of it -- a non-empty byte
// slice proves nothing, and the whole reason for going through the real
// mapping here is that a mapping that silently drops every resource still
// returns a perfectly valid document.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// renderedCatalog is the slice of the catalog/publish payload these tests
// read. Deliberately partial: this file asserts what buildCatalogs decides
// (which markets, in which catalogue, in which order), not the full shape of
// the mapping's output, which mappings_test.go owns.
type renderedCatalog struct {
	Context struct {
		TransactionID string `json:"transactionId"`
		MessageID     string `json:"messageId"`
		Timestamp     string `json:"timestamp"`
	} `json:"context"`
	Message struct {
		Catalogs []struct {
			ID         string `json:"id"`
			Descriptor struct {
				Name string `json:"name"`
			} `json:"descriptor"`
			Resources []struct {
				ID         string `json:"id"`
				Descriptor struct {
					Name string `json:"name"`
				} `json:"descriptor"`
			} `json:"resources"`
		} `json:"catalogs"`
	} `json:"message"`
}

// testCollectedMarket builds one market that survives every filter, so a test
// can state only the fact it is about.
func testCollectedMarket(id int, name, stateCode, stateName string) CollectedMarket {
	lat, lon := 19.0+float64(id)/1000, 73.0+float64(id)/1000
	return CollectedMarket{
		MarketID:          id,
		MarketName:        name,
		StateCode:         stateCode,
		StateName:         stateName,
		DistrictID:        1000 + id,
		DistrictName:      fmt.Sprintf("District %d", id),
		Latitude:          &lat,
		Longitude:         &lon,
		CoordinateQuality: coordinateOK,
		Commodities:       []Commodity{{Code: 23, Name: "Onion"}},
	}
}

// testCatalogConfig fixes every value a run would otherwise generate, so the
// assertions below are about the building and not about the clock.
func testCatalogConfig() catalogBuildConfig {
	return catalogBuildConfig{
		ParticipantID:   "agmarknet",
		NetworkID:       "oan-dev",
		WindowFrom:      "01-09-2026",
		WindowTo:        "01-09-2026",
		GeneratedAt:     "2026-09-01T00:00:00Z",
		TransactionID:   "txn-fixed",
		MessageID:       "msg-fixed",
		WithoutGeometry: withoutGeometryPublish,
	}
}

func decodeCatalog(t *testing.T, content []byte) renderedCatalog {
	t.Helper()
	var out renderedCatalog
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatalf("unmarshal rendered catalog: %v\nbody: %s", err, content)
	}
	return out
}

func TestBuildCatalogs_SingleMarketRendersOneCatalog(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	markets := []CollectedMarket{testCollectedMarket(1, "Pune", "MH", "Maharashtra")}

	built, summary, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, testCatalogConfig())
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d catalogs, want 1", len(built))
	}
	if built[0].StateCode != "MH" || built[0].Slug != "MH" {
		t.Errorf("stateCode/slug = %q/%q, want MH/MH", built[0].StateCode, built[0].Slug)
	}
	if built[0].CatalogID != "catalog:mandi-price:MH" {
		t.Errorf("catalogID = %q, want catalog:mandi-price:MH", built[0].CatalogID)
	}
	if summary.ZeroCommodities != 0 || summary.GeometryLess != 0 || len(summary.Excluded) != 0 {
		t.Errorf("summary = %+v, want nothing skipped", summary)
	}

	doc := decodeCatalog(t, built[0].Content)
	if len(doc.Message.Catalogs) != 1 {
		t.Fatalf("catalogs = %d, want 1", len(doc.Message.Catalogs))
	}
	if doc.Message.Catalogs[0].ID != "catalog:mandi-price:MH" {
		t.Errorf("rendered catalog id = %q, want catalog:mandi-price:MH", doc.Message.Catalogs[0].ID)
	}
	if len(doc.Message.Catalogs[0].Resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(doc.Message.Catalogs[0].Resources))
	}
	if got := doc.Message.Catalogs[0].Resources[0].ID; got != "resource:mandi-price:market:1" {
		t.Errorf("resource id = %q, want resource:mandi-price:market:1", got)
	}
	// The _local block reached the mapping: these are the fixed values, not
	// anything the mapping could have invented.
	if doc.Context.TransactionID != "txn-fixed" || doc.Context.MessageID != "msg-fixed" {
		t.Errorf("context ids = %q/%q, want txn-fixed/msg-fixed", doc.Context.TransactionID, doc.Context.MessageID)
	}
	if doc.Context.Timestamp != "2026-09-01T00:00:00Z" {
		t.Errorf("timestamp = %q, want the configured generatedAt", doc.Context.Timestamp)
	}
}

func TestBuildCatalogs_GroupsByState(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	markets := []CollectedMarket{
		testCollectedMarket(1, "Pune", "MH", "Maharashtra"),
		testCollectedMarket(2, "Mysuru", "KA", "Karnataka"),
	}

	built, _, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, testCatalogConfig())
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want 2", len(built))
	}
	// States come out in a fixed order so two runs read the same way.
	if built[0].StateCode != "KA" || built[1].StateCode != "MH" {
		t.Errorf("state order = %q,%q, want KA,MH", built[0].StateCode, built[1].StateCode)
	}
}

func TestBuildCatalogs_ExcludesZeroCommodityMarketByName(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	empty := testCollectedMarket(7, "  Barren  ", "MH", "Maharashtra")
	empty.Commodities = nil

	markets := []CollectedMarket{
		testCollectedMarket(1, "Pune", "MH", "Maharashtra"),
		empty,
	}

	built, summary, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, testCatalogConfig())
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if summary.ZeroCommodities != 1 {
		t.Errorf("zeroCommodities = %d, want 1", summary.ZeroCommodities)
	}
	if len(summary.Excluded) != 1 {
		t.Fatalf("excluded = %+v, want exactly one entry", summary.Excluded)
	}
	got := summary.Excluded[0]
	if got.MarketID != 7 || got.MarketName != "Barren" || got.StateCode != "MH" {
		t.Errorf("excluded entry = %+v, want market 7 Barren in MH (name trimmed)", got)
	}
	if got.Reason != "upstream reported no commodities trading in this window" {
		t.Errorf("reason = %q, want the zero-commodity reason", got.Reason)
	}

	doc := decodeCatalog(t, built[0].Content)
	if len(doc.Message.Catalogs[0].Resources) != 1 {
		t.Fatalf("resources = %d, want only the surviving market", len(doc.Message.Catalogs[0].Resources))
	}
}

func TestBuildCatalogs_StateWithNoPublishableMarketsProducesNoCatalog(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	empty := testCollectedMarket(7, "Barren", "MH", "Maharashtra")
	empty.Commodities = nil

	built, summary, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", []CollectedMarket{empty}, testCatalogConfig())
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 0 {
		t.Fatalf("built %d catalogs, want none", len(built))
	}
	if summary.ZeroCommodities != 1 {
		t.Errorf("zeroCommodities = %d, want 1", summary.ZeroCommodities)
	}
}

func TestBuildCatalogs_GeometryLessMarket_SkipExcludes(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	blind := testCollectedMarket(9, "Nowhere", "MH", "Maharashtra")
	blind.CoordinateQuality = coordinateMissing
	blind.Latitude, blind.Longitude = nil, nil

	cfg := testCatalogConfig()
	cfg.WithoutGeometry = withoutGeometrySkip

	markets := []CollectedMarket{testCollectedMarket(1, "Pune", "MH", "Maharashtra"), blind}

	built, summary, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, cfg)
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if summary.GeometryLess != 1 {
		t.Errorf("geometryLess = %d, want 1", summary.GeometryLess)
	}
	if len(summary.Excluded) != 1 || summary.Excluded[0].MarketID != 9 {
		t.Fatalf("excluded = %+v, want market 9", summary.Excluded)
	}
	if summary.Excluded[0].Reason != "coordinate missing" {
		t.Errorf("reason = %q, want \"coordinate missing\"", summary.Excluded[0].Reason)
	}
	if len(summary.GeometryLessMarkets) != 0 {
		t.Errorf("geometryLessMarkets = %+v, want empty when skipping", summary.GeometryLessMarkets)
	}

	doc := decodeCatalog(t, built[0].Content)
	if len(doc.Message.Catalogs[0].Resources) != 1 {
		t.Fatalf("resources = %d, want only the located market", len(doc.Message.Catalogs[0].Resources))
	}
}

func TestBuildCatalogs_GeometryLessMarket_PublishKeepsAndRecords(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	blind := testCollectedMarket(9, "Nowhere", "MH", "Maharashtra")
	blind.CoordinateQuality = coordinateSuspect
	blind.Latitude, blind.Longitude = nil, nil

	markets := []CollectedMarket{testCollectedMarket(1, "Pune", "MH", "Maharashtra"), blind}

	built, summary, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, testCatalogConfig())
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if summary.GeometryLess != 1 {
		t.Errorf("geometryLess = %d, want 1", summary.GeometryLess)
	}
	if len(summary.Excluded) != 0 {
		t.Errorf("excluded = %+v, want nothing excluded when publishing", summary.Excluded)
	}
	if len(summary.GeometryLessMarkets) != 1 || summary.GeometryLessMarkets[0].MarketID != 9 {
		t.Fatalf("geometryLessMarkets = %+v, want market 9 recorded", summary.GeometryLessMarkets)
	}
	if summary.GeometryLessMarkets[0].Reason != "coordinate suspect" {
		t.Errorf("reason = %q, want the market's own verdict", summary.GeometryLessMarkets[0].Reason)
	}

	doc := decodeCatalog(t, built[0].Content)
	if len(doc.Message.Catalogs[0].Resources) != 2 {
		t.Fatalf("resources = %d, want both markets published", len(doc.Message.Catalogs[0].Resources))
	}
}

func TestBuildCatalogs_OrdersMarketsByMarketID(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	markets := []CollectedMarket{
		testCollectedMarket(30, "Third", "MH", "Maharashtra"),
		testCollectedMarket(10, "First", "MH", "Maharashtra"),
		testCollectedMarket(20, "Second", "MH", "Maharashtra"),
	}

	built, _, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, testCatalogConfig())
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}

	doc := decodeCatalog(t, built[0].Content)
	want := []string{
		"resource:mandi-price:market:10",
		"resource:mandi-price:market:20",
		"resource:mandi-price:market:30",
	}
	if len(doc.Message.Catalogs[0].Resources) != len(want) {
		t.Fatalf("resources = %d, want %d", len(doc.Message.Catalogs[0].Resources), len(want))
	}
	for i, id := range want {
		if got := doc.Message.Catalogs[0].Resources[i].ID; got != id {
			t.Errorf("resource[%d] = %q, want %q", i, got, id)
		}
	}
}

// TestBuildCatalogs_OrderDecidesChunkMembership proves the sort happens in Go
// and not merely inside the mapping: only a sort BEFORE chunking can decide
// which catalogue a market lands in, and the mapping cannot move a market
// across two separate documents.
func TestBuildCatalogs_OrderDecidesChunkMembership(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	cfg := testCatalogConfig()
	cfg.Budget = 1

	markets := []CollectedMarket{
		testCollectedMarket(20, "Second", "MH", "Maharashtra"),
		testCollectedMarket(5, "First", "MH", "Maharashtra"),
	}

	built, _, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, cfg)
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want 2", len(built))
	}

	first := decodeCatalog(t, built[0].Content)
	if got := first.Message.Catalogs[0].Resources[0].ID; got != "resource:mandi-price:market:5" {
		t.Errorf("first chunk holds %q, want the lowest marketId", got)
	}
	second := decodeCatalog(t, built[1].Content)
	if got := second.Message.Catalogs[0].Resources[0].ID; got != "resource:mandi-price:market:20" {
		t.Errorf("second chunk holds %q, want the higher marketId", got)
	}
}

func TestBuildCatalogs_SplitsOverGeometryBudget(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	cfg := testCatalogConfig()
	cfg.Budget = 1

	markets := []CollectedMarket{
		testCollectedMarket(1, "Pune", "MH", "Maharashtra"),
		testCollectedMarket(2, "Nashik", "MH", "Maharashtra"),
	}

	built, _, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, cfg)
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogs, want 2", len(built))
	}
	if built[0].Slug != "MH" || built[1].Slug != "MH-2" {
		t.Fatalf("slugs = %q,%q, want MH,MH-2", built[0].Slug, built[1].Slug)
	}
	if built[0].CatalogID != "catalog:mandi-price:MH" || built[1].CatalogID != "catalog:mandi-price:MH-2" {
		t.Errorf("catalogIDs = %q,%q", built[0].CatalogID, built[1].CatalogID)
	}
	for i, b := range built {
		doc := decodeCatalog(t, b.Content)
		if len(doc.Message.Catalogs[0].Resources) != 1 {
			t.Errorf("catalog %d carries %d resources, want 1", i, len(doc.Message.Catalogs[0].Resources))
		}
		if doc.Message.Catalogs[0].ID != b.CatalogID {
			t.Errorf("catalog %d rendered id = %q, want %q", i, doc.Message.Catalogs[0].ID, b.CatalogID)
		}
	}
}

// A market with no geometry costs nothing, so a state of them stays in one
// catalogue however many markets it has -- the cap is on geometries, not rows.
func TestBuildCatalogs_GeometryLessMarketsDoNotSplit(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	cfg := testCatalogConfig()
	cfg.Budget = 1

	var markets []CollectedMarket
	for id := 1; id <= 5; id++ {
		m := testCollectedMarket(id, fmt.Sprintf("Market %d", id), "MH", "Maharashtra")
		m.CoordinateQuality = coordinateMissing
		m.Latitude, m.Longitude = nil, nil
		markets = append(markets, m)
	}

	built, _, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, cfg)
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d catalogs, want 1", len(built))
	}
	if built[0].Slug != "MH" {
		t.Errorf("slug = %q, want MH", built[0].Slug)
	}
}

func TestBuildCatalogs_RejectsUnknownGeometryOption(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	cfg := testCatalogConfig()
	cfg.WithoutGeometry = "maybe"

	if _, _, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", nil, cfg); err == nil {
		t.Fatal("want an error for an unknown without-geometry option")
	}
}

// TestBuildCatalogs_CountsStatesLeftWithNothingPublishable: a state whose
// every market was excluded produces no catalogue, and without a counter it
// disappears from the run entirely -- no catalogue, no error, nothing to
// report. The file's report block still asks for this number.
func TestBuildCatalogs_CountsStatesLeftWithNothingPublishable(t *testing.T) {
	mapper, mappingBase := testMapper(t)

	// Both markets are excluded: one has no commodities, the other has an
	// unusable coordinate under `skip`.
	markets := []CollectedMarket{
		{MarketID: 1, StateCode: "MH", StateName: "Maharashtra", CoordinateQuality: "ok"},
		{MarketID: 2, StateCode: "MH", StateName: "Maharashtra", CoordinateQuality: "missing",
			Commodities: []Commodity{{Code: 1, Name: "Onion"}}},
	}

	cfg := testCatalogConfig()
	cfg.WithoutGeometry = withoutGeometrySkip

	built, summary, err := buildCatalogs(context.Background(), mapper, mappingBase+"/catalog.yaml", markets, cfg)
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}
	if len(built) != 0 {
		t.Fatalf("built %d catalogues from markets that were all excluded", len(built))
	}
	if summary.EmptyStates != 1 {
		t.Errorf("EmptyStates = %d, want 1 -- a state that produced no catalogue must still be counted",
			summary.EmptyStates)
	}
}
