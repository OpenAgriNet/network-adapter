package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/tools/publish/internal/catalogpublish"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it. The report functions write straight to
// os.Stderr rather than taking a writer, so this is the seam a test has.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}

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

func TestCountQualityCountsOnlyTheMatchingVerdict(t *testing.T) {
	markets := []CollectedMarket{
		{CoordinateQuality: coordinateMissing},
		{CoordinateQuality: coordinateOK},
		{CoordinateQuality: coordinateMissing},
		{CoordinateQuality: coordinateSuspect},
	}
	if got := countQuality(markets, coordinateMissing); got != 2 {
		t.Errorf("countQuality(missing) = %d, want 2", got)
	}
	if got := countQuality(markets, coordinateOutOfBounds); got != 0 {
		t.Errorf("countQuality(outOfBounds) = %d, want 0", got)
	}
}

func TestPrintCollectionSummaryNamesEveryFactAboutTheRun(t *testing.T) {
	collection := Collection{
		Markets: []CollectedMarket{
			{CoordinateQuality: coordinateMissing},
			{CoordinateQuality: coordinateOK},
		},
		EmptyStates: []string{"GA", "KL"},
		StateErrors: []StateError{{StateCode: "TN", Reason: "upstream timed out"}},
	}

	out := captureStderr(t, func() { printCollectionSummary(collection) })

	for _, want := range []string{"collected 2 markets", "1 markets have missing", "GA, KL", "TN FAILED: upstream timed out"} {
		if !strings.Contains(out, want) {
			t.Errorf("collection summary does not mention %q:\n%s", want, out)
		}
	}
}

func TestPrintBuildSummaryPrintsTheReportLines(t *testing.T) {
	built := []BuiltState{{StateCode: "TN", CatalogID: "catalog:mandi-price:TN", Path: "catalog/mandi-TN.json", Markets: 12}}

	out := captureStderr(t, func() { printBuildSummary(built, SkipSummary{}, buildConfig{catalogOut: "catalog"}) })

	if !strings.Contains(out, "catalog:mandi-price:TN") {
		t.Errorf("build summary does not name the catalog:\n%s", out)
	}
}

func TestPrintPublishSummaryReportsEachOutcome(t *testing.T) {
	res := catalogpublish.Result{
		Outcomes: []catalogpublish.Outcome{
			{StateCode: "MH", CatalogID: "catalog:mandi-price:MH", Status: catalogpublish.StatusPublished},
			{StateCode: "TN", CatalogID: "catalog:mandi-price:TN", Status: catalogpublish.StatusRejected, Reason: "bad schema"},
			{StateCode: "KA", CatalogID: "catalog:mandi-price:KA", Status: catalogpublish.StatusTransportError, Reason: "connection refused"},
		},
		RetiredOld: &catalogpublish.Outcome{CatalogID: "cat-agmarknet-mandi-prices", Status: catalogpublish.StatusPublished},
	}

	out := captureStderr(t, func() { printPublishSummary(res, catalogpublish.Config{}) })

	for _, want := range []string{
		"catalog:mandi-price:MH -> ACCEPTED",
		"catalog:mandi-price:TN -> REJECTED: bad schema",
		"catalog:mandi-price:KA -> ERROR: connection refused",
		"retired old catalog cat-agmarknet-mandi-prices -> ACCEPTED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("publish summary does not mention %q:\n%s", want, out)
		}
	}
}

func TestPrintPublishSummaryAnnouncesADryRun(t *testing.T) {
	res := catalogpublish.Result{
		Outcomes: []catalogpublish.Outcome{
			{StateCode: "MH", CatalogID: "catalog:mandi-price:MH", Status: catalogpublish.StatusDryRun},
		},
	}

	out := captureStderr(t, func() {
		printPublishSummary(res, catalogpublish.Config{DryRun: true, PublishURL: "http://localhost:9200/"})
	})

	if !strings.Contains(out, "dry-run: would publish to http://localhost:9200/publish") {
		t.Errorf("publish summary does not announce the dry-run target:\n%s", out)
	}
	if !strings.Contains(out, "catalog:mandi-price:MH -> would POST") {
		t.Errorf("publish summary does not report the dry-run outcome:\n%s", out)
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
