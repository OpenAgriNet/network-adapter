package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// printCollectionSummary reports what was collected and, above all, what was
// not: a doubtful coordinate and a state that answered nothing are both facts
// a reader has to see.
func printCollectionSummary(collection Collection) {
	fmt.Fprintf(os.Stderr, "collected %d markets\n", len(collection.Markets))
	for _, quality := range []string{coordinateMissing, coordinateSuspect, coordinateOutOfBounds} {
		if n := countQuality(collection.Markets, quality); n > 0 {
			fmt.Fprintf(os.Stderr, "  %d markets have %s coordinates\n", n, quality)
		}
	}
	if len(collection.EmptyStates) > 0 {
		// The upstream holds no mapping rows for these. It is a coverage fact,
		// not a failure -- see errNoUpstreamData.
		fmt.Fprintf(os.Stderr, "  %d states returned no data: %s\n",
			len(collection.EmptyStates), strings.Join(collection.EmptyStates, ", "))
	}
	for _, failure := range collection.StateErrors {
		fmt.Fprintf(os.Stderr, "  %s FAILED: %s\n", failure.StateCode, failure.Reason)
	}
}

// countQuality counts markets carrying one verdict, for the run summary.
func countQuality(markets []CollectedMarket, quality string) int {
	n := 0
	for _, market := range markets {
		if market.CoordinateQuality == quality {
			n++
		}
	}
	return n
}

func printBuildSummary(built []BuiltState, summary SkipSummary, cfg buildConfig) {
	for _, line := range buildReportLines(built, summary, cfg.catalogOut) {
		fmt.Fprintln(os.Stderr, line)
	}
}

// buildReportLines renders what was built and, in detail, what was left out.
//
// Every excluded market is named, not counted. "95 markets have missing
// coordinates" tells nobody which ones, and the reason for reporting them at
// all is that somebody can look one up against the upstream.
func buildReportLines(built []BuiltState, summary SkipSummary, catalogOut string) []string {
	lines := []string{fmt.Sprintf("built %d catalogs into %s", len(built), catalogOut)}

	// Per state, so a split state reads as one state in several catalogs
	// rather than as several states.
	totals := map[string]int{}
	var order []string
	for _, state := range built {
		if _, seen := totals[state.StateCode]; !seen {
			order = append(order, state.StateCode)
		}
		totals[state.StateCode] += state.Markets
	}
	for _, stateCode := range order {
		chunks := chunksOf(built, stateCode)
		if len(chunks) == 1 {
			lines = append(lines, fmt.Sprintf("  %s: %d markets -> %s",
				stateCode, totals[stateCode], chunks[0].CatalogID))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %d markets split across %d catalogs (%d-geometry limit per catalog)",
			stateCode, totals[stateCode], len(chunks), catalogGeometryBudget))
		for _, chunk := range chunks {
			lines = append(lines, fmt.Sprintf("      %s: %d markets", chunk.CatalogID, chunk.Markets))
		}
	}

	if len(summary.Excluded) > 0 {
		lines = append(lines, fmt.Sprintf("%d markets NOT PUBLISHED:", len(summary.Excluded)))
		lines = append(lines, marketLines(summary.Excluded)...)
	}

	if len(summary.GeometryLessMarkets) > 0 {
		lines = append(lines, fmt.Sprintf(
			"%d markets published WITHOUT a location, so no proximity search can find them:",
			len(summary.GeometryLessMarkets)))
		lines = append(lines, "  (a filter on market.state or market.district still returns them)")
		lines = append(lines, marketLines(summary.GeometryLessMarkets)...)
	}

	if summary.EmptyStates > 0 {
		lines = append(lines, fmt.Sprintf("%d states produced no catalog at all", summary.EmptyStates))
	}
	return lines
}

// chunksOf returns one state's catalogs in the order they were built.
func chunksOf(built []BuiltState, stateCode string) []BuiltState {
	var out []BuiltState
	for _, state := range built {
		if state.StateCode == stateCode {
			out = append(out, state)
		}
	}
	return out
}

// marketLines groups markets by reason and names each one, so the reasons are
// counted and the individual markets stay identifiable.
func marketLines(markets []ExcludedMarket) []string {
	byReason := map[string][]ExcludedMarket{}
	for _, market := range markets {
		byReason[market.Reason] = append(byReason[market.Reason], market)
	}
	reasons := make([]string, 0, len(byReason))
	for reason := range byReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)

	var lines []string
	for _, reason := range reasons {
		group := byReason[reason]
		lines = append(lines, fmt.Sprintf("  %d x %s:", len(group), reason))
		for _, market := range group {
			lines = append(lines, fmt.Sprintf("      %s %d %s",
				market.StateCode, market.MarketID, market.MarketName))
		}
	}
	return lines
}

func printPublishSummary(res PublishResult, cfg publishConfig) {
	if cfg.dryRun {
		fmt.Fprintf(os.Stderr, "dry-run: would publish to %s/publish\n", strings.TrimRight(cfg.publishURL, "/"))
	}
	for _, o := range res.Outcomes {
		switch o.Status {
		case StatusPublished:
			fmt.Fprintf(os.Stderr, "  %s: %s -> ACCEPTED\n", o.StateCode, o.CatalogID)
		case StatusDryRun:
			fmt.Fprintf(os.Stderr, "  %s: %s -> would POST\n", o.StateCode, o.CatalogID)
		case StatusRejected:
			fmt.Fprintf(os.Stderr, "  %s: %s -> REJECTED: %s\n", o.StateCode, o.CatalogID, o.Reason)
		case StatusTransportError:
			fmt.Fprintf(os.Stderr, "  %s: %s -> ERROR: %s\n", o.StateCode, o.CatalogID, o.Reason)
		}
	}
	if res.RetiredOld != nil {
		o := res.RetiredOld
		switch o.Status {
		case StatusPublished:
			fmt.Fprintf(os.Stderr, "  retired old catalog %s -> ACCEPTED\n", o.CatalogID)
		case StatusDryRun:
			fmt.Fprintf(os.Stderr, "  retired old catalog %s -> would POST\n", o.CatalogID)
		case StatusRejected:
			fmt.Fprintf(os.Stderr, "  retiring old catalog %s -> REJECTED: %s\n", o.CatalogID, o.Reason)
		case StatusTransportError:
			fmt.Fprintf(os.Stderr, "  retiring old catalog %s -> ERROR: %s\n", o.CatalogID, o.Reason)
		}
	}
}
