package main

import (
	"strings"
	"testing"
)

func TestReportNamesEveryExcludedMarketAndWhy(t *testing.T) {
	summary := SkipSummary{
		ZeroCommodities: 2,
		Excluded: []ExcludedMarket{
			{MarketID: 999, MarketName: "Empty APMC", StateCode: "TN",
				Reason: "upstream reported no commodities trading in this window"},
			{MarketID: 1001, MarketName: "Quiet APMC", StateCode: "MH",
				Reason: "upstream reported no commodities trading in this window"},
		},
	}

	report := strings.Join(buildReportLines(nil, summary, "catalog"), "\n")

	// A count alone tells nobody WHICH market vanished from the network.
	for _, want := range []string{"999", "Empty APMC", "TN", "1001", "Quiet APMC", "MH"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not mention %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "no commodities") {
		t.Errorf("report does not say why:\n%s", report)
	}
	// Excluded means NOT PUBLISHED, and the report has to say so in words, not
	// leave it to be inferred from a heading.
	if !strings.Contains(strings.ToLower(report), "not published") {
		t.Errorf("report does not say the markets were left out:\n%s", report)
	}
}

func TestReportSeparatesTheCoordinateVerdicts(t *testing.T) {
	summary := SkipSummary{
		GeometryLess: 3,
		GeometryLessMarkets: []ExcludedMarket{
			{MarketID: 1, MarketName: "A", StateCode: "TN", Reason: "coordinate missing"},
			{MarketID: 2, MarketName: "B", StateCode: "TN", Reason: "coordinate outOfBounds"},
			{MarketID: 3, MarketName: "C", StateCode: "TN", Reason: "coordinate suspect"},
		},
	}

	report := strings.Join(buildReportLines(nil, summary, "catalog"), "\n")

	// missing, suspect and outOfBounds are three different upstream defects
	// with three different remedies, so they must not collapse into one label.
	for _, verdict := range []string{coordinateMissing, coordinateOutOfBounds, coordinateSuspect} {
		if !strings.Contains(report, verdict) {
			t.Errorf("report does not distinguish %q:\n%s", verdict, report)
		}
	}
	// These ARE published; only proximity search cannot reach them.
	lower := strings.ToLower(report)
	if !strings.Contains(lower, "published") || !strings.Contains(lower, "proximity") {
		t.Errorf("report does not say these are published but unfindable by distance:\n%s", report)
	}
}

func TestReportShowsChunkingWhenAStateIsSplit(t *testing.T) {
	built := []BuiltState{
		{StateCode: "TN", CatalogID: "p/mandi-TN-1", Path: "catalog/mandi-TN-1.json", Markets: 100},
		{StateCode: "TN", CatalogID: "p/mandi-TN-2", Path: "catalog/mandi-TN-2.json", Markets: 36},
	}

	report := strings.Join(buildReportLines(built, SkipSummary{}, "catalog"), "\n")

	if !strings.Contains(report, "p/mandi-TN-1") || !strings.Contains(report, "p/mandi-TN-2") {
		t.Errorf("report does not name both chunks:\n%s", report)
	}
	if !strings.Contains(report, "136") {
		t.Errorf("report does not total the state's markets across chunks:\n%s", report)
	}
}

func TestReportIsQuietWhenNothingWasLeftOut(t *testing.T) {
	built := []BuiltState{{StateCode: "TN", CatalogID: "p/mandi-TN", Path: "catalog/mandi-TN.json", Markets: 12}}

	report := strings.Join(buildReportLines(built, SkipSummary{}, "catalog"), "\n")

	// A clean run must not print empty "0 markets excluded" headings; noise in
	// the good case is what stops people reading the bad case.
	lower := strings.ToLower(report)
	if strings.Contains(lower, "not published") || strings.Contains(lower, "proximity") {
		t.Errorf("clean run printed exclusion sections:\n%s", report)
	}
}
