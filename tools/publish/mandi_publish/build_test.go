package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func floatPtr(v float64) *float64 {
	return &v
}

func sampleCollection() Collection {
	return Collection{
		GeneratedAt: "2026-09-10T11:09:18Z",
		Window: Window{
			From: "01-07-2026",
			To:   "01-12-2026",
		},
		Markets: []CollectedMarket{
			{
				MarketID:          1282,
				MarketName:        "Jamkhed APMC",
				StateCode:         "MH",
				StateName:         "Maharashtra",
				DistrictID:        338,
				DistrictName:      "Ahmednagar",
				Latitude:          floatPtr(18.609873934158966),
				Longitude:         floatPtr(74.69368182799322),
				CoordinateQuality: coordinateOK,
				Commodities: []Commodity{
					{Code: 4, Name: "Maize"},
					{Code: 2, Name: "Paddy(Common)"},
				},
			},
		},
	}
}

func TestBuildOneMarketResourceExactMatch(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := buildConfig{
		catalogOut:       tmpDir,
		participantID:    "agmarknet-mock",
		networkID:        "oan-dev",
		withoutGeometry:  withoutGeometryPublish,
		fixedTxnID:       "txn-123",
		fixedMsgID:       "msg-123",
		fixedGeneratedAt: "2026-09-10T11:09:18Z",
	}

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

	built, summary, err := buildFromCollection(ctx, sampleCollection(), cfg, mapper, mappingBase)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("expected 1 built state, got %d", len(built))
	}
	if summary.ZeroCommodities != 0 || summary.GeometryLess != 0 {
		t.Errorf("unexpected summary: %+v", summary)
	}

	outFile := filepath.Join(tmpDir, "mandi-MH.json")
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}

	// Verify Envelope
	contextObj := root["context"].(map[string]any)
	if contextObj["action"] != "catalog/publish" {
		t.Errorf("action = %v, want catalog/publish", contextObj["action"])
	}
	if contextObj["version"] != "2.0.0" {
		t.Errorf("version = %v, want 2.0.0", contextObj["version"])
	}
	if contextObj["transactionId"] != "txn-123" {
		t.Errorf("transactionId = %v, want txn-123", contextObj["transactionId"])
	}
	if contextObj["messageId"] != "msg-123" {
		t.Errorf("messageId = %v, want msg-123", contextObj["messageId"])
	}

	msgObj := root["message"].(map[string]any)
	catalogs := msgObj["catalogs"].([]any)
	if len(catalogs) != 1 {
		t.Fatalf("len(catalogs) = %d, want 1", len(catalogs))
	}
	catalog := catalogs[0].(map[string]any)
	if catalog["id"] != "agmarknet-mock/mandi-MH" {
		t.Errorf("catalog.id = %v, want agmarknet-mock/mandi-MH", catalog["id"])
	}
	if catalog["isActive"] != true {
		t.Errorf("isActive = %v, want true", catalog["isActive"])
	}

	desc := catalog["descriptor"].(map[string]any)
	if desc["code"] != "AGMARKNET-MH" {
		t.Errorf("desc.code = %v, want AGMARKNET-MH", desc["code"])
	}
	if desc["name"] != "Agmarknet mandi markets — Maharashtra" {
		t.Errorf("desc.name = %v, want Agmarknet mandi markets — Maharashtra", desc["name"])
	}

	prov := catalog["provider"].(map[string]any)
	if prov["id"] != "agmarknet-mock" {
		t.Errorf("provider.id = %v, want agmarknet-mock", prov["id"])
	}

	// Catalog validity: startDate / endDate
	validity := catalog["validity"].(map[string]any)
	if validity["startDate"] != "2026-07-01T00:00:00Z" {
		t.Errorf("startDate = %v, want 2026-07-01T00:00:00Z", validity["startDate"])
	}
	if validity["endDate"] != "2026-12-01T23:59:59Z" {
		t.Errorf("endDate = %v, want 2026-12-01T23:59:59Z", validity["endDate"])
	}

	// Verify Resource
	resources := catalog["resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("len(resources) = %d, want 1", len(resources))
	}
	res := resources[0].(map[string]any)
	if res["id"] != "res:agmarknet:market:1282" {
		t.Errorf("res.id = %v, want res:agmarknet:market:1282", res["id"])
	}
	resDesc := res["descriptor"].(map[string]any)
	if resDesc["code"] != "1282" {
		t.Errorf("resDesc.code = %v, want 1282", resDesc["code"])
	}
	if resDesc["name"] != "Jamkhed APMC" {
		t.Errorf("resDesc.name = %v, want Jamkhed APMC", resDesc["name"])
	}
	if resDesc["shortDesc"] != "Mandi prices at Jamkhed APMC, Ahmednagar, Maharashtra" {
		t.Errorf("resDesc.shortDesc = %v", resDesc["shortDesc"])
	}

	ra := res["resourceAttributes"].(map[string]any)
	if ra["@context"] != "https://raw.githubusercontent.com/OpenAgriNet/network-specs/schema-packs-v0.1/schema/MandiPrice/v0.1/context.jsonld" {
		t.Errorf("@context = %v", ra["@context"])
	}
	if ra["@type"] != "openagrinet:MandiPrice" {
		t.Errorf("@type = %v", ra["@type"])
	}
	if ra["informationMode"] != "OnDemand" {
		t.Errorf("informationMode = %v", ra["informationMode"])
	}

	market := ra["market"].(map[string]any)
	if market["marketCode"] != "1282" {
		t.Errorf("marketCode = %v, want 1282", market["marketCode"])
	}
	if market["marketName"] != "Jamkhed APMC" {
		t.Errorf("marketName = %v, want Jamkhed APMC", market["marketName"])
	}
	if market["district"] != "338" {
		t.Errorf("district = %v, want 338", market["district"])
	}
	if market["state"] != "MH" {
		t.Errorf("state = %v, want MH", market["state"])
	}
	if _, exists := market["location"]; exists {
		t.Errorf("market.location exists, want absent (coverageAreas is the only geometry)")
	}

	// Supported commodities: string codes, sorted ascending: 2 then 4
	commList := ra["supportedCommodities"].([]any)
	if len(commList) != 2 {
		t.Fatalf("len(supportedCommodities) = %d, want 2", len(commList))
	}
	c0 := commList[0].(map[string]any)
	c1 := commList[1].(map[string]any)
	if c0["code"] != "2" || c0["name"] != "Paddy(Common)" {
		t.Errorf("commodity 0 = %+v", c0)
	}
	if c1["code"] != "4" || c1["name"] != "Maize" {
		t.Errorf("commodity 1 = %+v", c1)
	}

	// Resource validity: startsAt / endsAt
	resVal := ra["validity"].(map[string]any)
	if resVal["startsAt"] != "2026-07-01T00:00:00Z" {
		t.Errorf("startsAt = %v", resVal["startsAt"])
	}
	if resVal["endsAt"] != "2026-12-01T23:59:59Z" {
		t.Errorf("endsAt = %v", resVal["endsAt"])
	}

	// Coverage areas
	covAreas := ra["coverageAreas"].([]any)
	if len(covAreas) != 2 {
		t.Fatalf("len(coverageAreas) = %d, want 2", len(covAreas))
	}
	covPoint := covAreas[0].(map[string]any)
	if covPoint["type"] != "Point" {
		t.Errorf("covPoint.type = %v", covPoint["type"])
	}
	covCoords := covPoint["coordinates"].([]any)
	if len(covCoords) != 2 || covCoords[0].(float64) != 74.69368182799322 || covCoords[1].(float64) != 18.609873934158966 {
		t.Errorf("covPoint.coordinates = %v, want [74.69368182799322, 18.609873934158966]", covCoords)
	}
	covAdmin := covAreas[1].(map[string]any)
	if covAdmin["codeScheme"] != "AGMARKNET-DISTRICT" || covAdmin["areaCode"] != "338" ||
		covAdmin["areaLevel"] != "District" || covAdmin["areaName"] != "Ahmednagar" {
		t.Errorf("covAdmin = %+v", covAdmin)
	}

	// Publish Directives
	directives := msgObj["publishDirectives"].([]any)
	if len(directives) != 1 {
		t.Fatalf("len(directives) = %d, want 1", len(directives))
	}
	dir := directives[0].(map[string]any)
	if dir["catalogId"] != "agmarknet-mock/mandi-MH" {
		t.Errorf("dir.catalogId = %v", dir["catalogId"])
	}
	if dir["catalogType"] != "REGULAR" {
		t.Errorf("dir.catalogType = %v", dir["catalogType"])
	}
	if dir["updateMode"] != "MERGE" {
		t.Errorf("dir.updateMode = %v", dir["updateMode"])
	}
	visibleTo := dir["visibleTo"].([]any)
	if len(visibleTo) != 1 || visibleTo[0] != "oan-dev" {
		t.Errorf("visibleTo = %v", visibleTo)
	}
}

func TestBuildMarketWithOneCommodityWrapsArray(t *testing.T) {
	tmpDir := t.TempDir()

	col := Collection{
		GeneratedAt: "2026-09-10T11:09:18Z",
		Window:      Window{From: "01-07-2026", To: "01-12-2026"},
		Markets: []CollectedMarket{
			{
				MarketID:          1282,
				MarketName:        "Jamkhed APMC",
				StateCode:         "MH",
				StateName:         "Maharashtra",
				DistrictID:        338,
				DistrictName:      "Ahmednagar",
				Latitude:          floatPtr(18.609873934158966),
				Longitude:         floatPtr(74.69368182799322),
				CoordinateQuality: coordinateOK,
				Commodities: []Commodity{
					{Code: 4, Name: "Maize"},
				},
			},
		},
	}

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

	_, _, err = buildFromCollection(ctx, col, buildConfig{catalogOut: tmpDir}, mapper, mappingBase)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "mandi-MH.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	catalogs := root["message"].(map[string]any)["catalogs"].([]any)
	resources := catalogs[0].(map[string]any)["resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("resources length = %d, want 1", len(resources))
	}
	ra := resources[0].(map[string]any)["resourceAttributes"].(map[string]any)
	commList := ra["supportedCommodities"].([]any)
	if len(commList) != 1 {
		t.Fatalf("supportedCommodities length = %d, want 1", len(commList))
	}
	c := commList[0].(map[string]any)
	if c["code"] != "4" || c["name"] != "Maize" {
		t.Errorf("commodity = %+v", c)
	}
}

func TestBuildMarketWithZeroCommoditiesSkipped(t *testing.T) {
	tmpDir := t.TempDir()

	col := Collection{
		GeneratedAt: "2026-09-10T11:09:18Z",
		Window:      Window{From: "01-07-2026", To: "01-12-2026"},
		Markets: []CollectedMarket{
			{
				MarketID:          999,
				MarketName:        "Empty APMC",
				StateCode:         "MH",
				StateName:         "Maharashtra",
				DistrictID:        338,
				DistrictName:      "Ahmednagar",
				Latitude:          floatPtr(18.6),
				Longitude:         floatPtr(74.6),
				CoordinateQuality: coordinateOK,
				Commodities:       []Commodity{}, // zero commodities
			},
		},
	}

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

	built, summary, err := buildFromCollection(ctx, col, buildConfig{catalogOut: tmpDir}, mapper, mappingBase)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(built) != 0 {
		t.Fatalf("built states = %d, want 0", len(built))
	}
	if summary.ZeroCommodities != 1 {
		t.Errorf("summary.ZeroCommodities = %d, want 1", summary.ZeroCommodities)
	}
	if summary.EmptyStates != 1 {
		t.Errorf("summary.EmptyStates = %d, want 1", summary.EmptyStates)
	}
}

func TestBuildGeometryLessMarket(t *testing.T) {
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

	col := Collection{
		GeneratedAt: "2026-09-10T11:09:18Z",
		Window:      Window{From: "01-07-2026", To: "01-12-2026"},
		Markets: []CollectedMarket{
			{
				MarketID:          888,
				MarketName:        "NoGeo APMC",
				StateCode:         "MH",
				StateName:         "Maharashtra",
				DistrictID:        338,
				DistrictName:      "Ahmednagar",
				Latitude:          nil,
				Longitude:         nil,
				CoordinateQuality: coordinateMissing,
				Commodities: []Commodity{
					{Code: 1, Name: "Wheat"},
				},
			},
		},
	}

	// 1. When withoutGeometry == "publish" (default)
	tmpDirPub := t.TempDir()
	built, summary, err := buildFromCollection(ctx, col, buildConfig{catalogOut: tmpDirPub, withoutGeometry: withoutGeometryPublish}, mapper, mappingBase)
	if err != nil {
		t.Fatalf("build publish: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("len(built) = %d, want 1", len(built))
	}
	if summary.GeometryLess != 1 {
		t.Errorf("summary.GeometryLess = %d, want 1", summary.GeometryLess)
	}

	data, err := os.ReadFile(filepath.Join(tmpDirPub, "mandi-MH.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	res := root["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)["resources"].([]any)[0].(map[string]any)
	ra := res["resourceAttributes"].(map[string]any)
	market := ra["market"].(map[string]any)
	if _, exists := market["location"]; exists {
		t.Errorf("market.location exists, want absent")
	}
	covAreas := ra["coverageAreas"].([]any)
	if len(covAreas) != 1 {
		t.Fatalf("len(coverageAreas) = %d, want 1 (admin area only)", len(covAreas))
	}
	admin := covAreas[0].(map[string]any)
	if admin["codeScheme"] != "AGMARKNET-DISTRICT" {
		t.Errorf("codeScheme = %v", admin["codeScheme"])
	}

	// 2. When withoutGeometry == "skip"
	tmpDirSkip := t.TempDir()
	builtSkip, summarySkip, err := buildFromCollection(ctx, col, buildConfig{catalogOut: tmpDirSkip, withoutGeometry: withoutGeometrySkip}, mapper, mappingBase)
	if err != nil {
		t.Fatalf("build skip: %v", err)
	}
	if len(builtSkip) != 0 {
		t.Fatalf("len(builtSkip) = %d, want 0", len(builtSkip))
	}
	if summarySkip.GeometryLess != 1 {
		t.Errorf("summarySkip.GeometryLess = %d, want 1", summarySkip.GeometryLess)
	}
	if summarySkip.EmptyStates != 1 {
		t.Errorf("summarySkip.EmptyStates = %d, want 1", summarySkip.EmptyStates)
	}
}

func TestBuildCommodityDedupeAndSort(t *testing.T) {
	tmpDir := t.TempDir()

	col := Collection{
		GeneratedAt: "2026-09-10T11:09:18Z",
		Window:      Window{From: "01-07-2026", To: "01-12-2026"},
		Markets: []CollectedMarket{
			{
				MarketID:          100,
				MarketName:        "Test APMC",
				StateCode:         "MH",
				StateName:         "Maharashtra",
				DistrictID:        338,
				DistrictName:      "Ahmednagar",
				Latitude:          floatPtr(18.6),
				Longitude:         floatPtr(74.6),
				CoordinateQuality: coordinateOK,
				Commodities: []Commodity{
					{Code: 160, Name: "Pomegranate"},
					{Code: 23, Name: "Onion"},
					{Code: 160, Name: "Pomegranate"}, // duplicate
					{Code: 4, Name: "Maize"},
				},
			},
		},
	}

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

	_, _, err = buildFromCollection(ctx, col, buildConfig{catalogOut: tmpDir}, mapper, mappingBase)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "mandi-MH.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	ra := root["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)["resources"].([]any)[0].(map[string]any)["resourceAttributes"].(map[string]any)
	commList := ra["supportedCommodities"].([]any)
	if len(commList) != 3 {
		t.Fatalf("len(supportedCommodities) = %d, want 3", len(commList))
	}
	wantCodes := []string{"4", "23", "160"}
	for i, want := range wantCodes {
		got := commList[i].(map[string]any)["code"]
		if got != want {
			t.Errorf("commList[%d].code = %v, want %v", i, got, want)
		}
	}
}

func TestBuildDeterministicOutput(t *testing.T) {
	col := sampleCollection()

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

	cfg1 := buildConfig{
		catalogOut:       t.TempDir(),
		participantID:    "agmarknet",
		networkID:        "oan-dev",
		withoutGeometry:  withoutGeometryPublish,
		fixedTxnID:       "fixed-txn",
		fixedMsgID:       "fixed-msg",
		fixedGeneratedAt: "2026-09-10T12:00:00Z",
	}
	cfg2 := buildConfig{
		catalogOut:       t.TempDir(),
		participantID:    "agmarknet",
		networkID:        "oan-dev",
		withoutGeometry:  withoutGeometryPublish,
		fixedTxnID:       "fixed-txn",
		fixedMsgID:       "fixed-msg",
		fixedGeneratedAt: "2026-09-10T12:00:00Z",
	}

	_, _, err = buildFromCollection(ctx, col, cfg1, mapper, mappingBase)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	_, _, err = buildFromCollection(ctx, col, cfg2, mapper, mappingBase)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}

	bytes1, err := os.ReadFile(filepath.Join(cfg1.catalogOut, "mandi-MH.json"))
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	bytes2, err := os.ReadFile(filepath.Join(cfg2.catalogOut, "mandi-MH.json"))
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}

	if !reflect.DeepEqual(bytes1, bytes2) {
		t.Fatalf("runs produced different output")
	}
}

func TestBuildEndToEndAgainstRealMHFile(t *testing.T) {
	realMH := "../../../mandi_MH.json"
	if _, err := os.Stat(realMH); err != nil {
		t.Skipf("mandi_MH.json not present at %s: %v", realMH, err)
	}

	fixture, readErr := os.ReadFile(realMH)
	if readErr != nil {
		t.Fatalf("read %s: %v", realMH, readErr)
	}
	var collection Collection
	if err := json.Unmarshal(fixture, &collection); err != nil {
		t.Fatalf("unmarshal %s: %v", realMH, err)
	}

	tmpDir := t.TempDir()
	cfg := buildConfig{
		catalogOut:      tmpDir,
		participantID:   "agmarknet-mock",
		networkID:       "oan-dev",
		withoutGeometry: withoutGeometryPublish,
	}

	ctx := context.Background()
	built, summary, err := buildCollection(ctx, collection, cfg)
	if err != nil {
		t.Fatalf("build real MH: %v", err)
	}

	// 272 of the 273 markets carry a coordinate, so Maharashtra spends 272
	// geometries and is the one state that cannot fit in a single catalog.
	if len(built) != 2 {
		t.Fatalf("built states = %+v, want 2 chunks for 272 geometries", built)
	}
	total := 0
	for i, state := range built {
		if state.StateCode != "MH" {
			t.Errorf("chunk %d state = %q, want MH", i, state.StateCode)
		}
		wantID := fmt.Sprintf("agmarknet-mock/mandi-MH-%d", i+1)
		if state.CatalogID != wantID {
			t.Errorf("chunk %d id = %q, want %q", i, state.CatalogID, wantID)
		}
		total += state.Markets
	}
	if total != 273 {
		t.Errorf("chunks carry %d markets in total, want 273", total)
	}

	// 1 of the 273 has missing coordinates, and it is named, not just counted.
	if summary.GeometryLess != 1 {
		t.Errorf("summary.GeometryLess = %d, want 1", summary.GeometryLess)
	}
	if len(summary.GeometryLessMarkets) != 1 {
		t.Fatalf("GeometryLessMarkets = %+v, want one named market", summary.GeometryLessMarkets)
	}
	if !strings.Contains(summary.GeometryLessMarkets[0].Reason, coordinateMissing) {
		t.Errorf("reason = %q, want it to name the verdict", summary.GeometryLessMarkets[0].Reason)
	}

	// Every market appears exactly once across the chunks.
	resourcesByID := map[string]map[string]any{}
	for _, state := range built {
		data, err := os.ReadFile(state.Path)
		if err != nil {
			t.Fatalf("read %s: %v", state.Path, err)
		}
		var root map[string]any
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatalf("unmarshal %s: %v", state.Path, err)
		}
		catalogs := root["message"].(map[string]any)["catalogs"].([]any)
		for _, r := range catalogs[0].(map[string]any)["resources"].([]any) {
			rm := r.(map[string]any)
			id := rm["id"].(string)
			if _, duplicate := resourcesByID[id]; duplicate {
				t.Errorf("%s appears in more than one chunk", id)
			}
			resourcesByID[id] = rm
		}
	}
	if len(resourcesByID) != 273 {
		t.Fatalf("%d distinct resources across chunks, want 273", len(resourcesByID))
	}

	foundRes, ok := resourcesByID["res:agmarknet:market:1282"]
	if !ok {
		t.Fatalf("market 1282 not found in any chunk")
	}

	ra := foundRes["resourceAttributes"].(map[string]any)
	market := ra["market"].(map[string]any)
	if market["marketName"] != "Jamkhed APMC" {
		t.Errorf("marketName = %v, want Jamkhed APMC", market["marketName"])
	}
}
