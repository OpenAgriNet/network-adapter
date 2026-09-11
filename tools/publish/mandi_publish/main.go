// Command mandi_publish collects, builds, and publishes Agmarknet Vistaar
// market catalogs.
//
// ONE RUN COLLECTS AND WRITES THE CATALOGS. There is no intermediate
// collection document.
//
// Collect and build:
//
//	MANDI_TOKEN_USER=... MANDI_TOKEN_SECRET=... \
//	  mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --catalog-out catalog
//
// Collect, build and publish in one go:
//
//	MANDI_PUBLISH_URL=http://localhost:9200 \
//	  mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --publish
//
// Publish catalogs already on disk, collecting nothing:
//
//	mandi_publish --publish --catalog-in catalog --states MH --dry-run
//
// Each state becomes one catalog, because the discovery service caps a catalog
// at 256 geometries -- measured, see the design spec.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// config is one run's inputs, gathered so collect can be tested without flags.
type config struct {
	baseURL  string
	user     string
	secret   string
	states   []string // empty means every state master data reports
	fromDate string
	toDate   string
}

func main() {
	loadEnvFile(".env")

	// Collect
	baseURL := flag.String("base-url", "http://34.0.4.235:8080", "Agmarknet Vistaar base URL")
	states := flag.String("states", "", "comma-separated state codes; empty means every state")
	fromDate := flag.String("from", "", "window start, dd-MM-yyyy (default: today)")
	toDate := flag.String("to", "", "window end, dd-MM-yyyy (default: today)")

	// Build. The catalogs ARE the output: one run collects and writes them.
	catalogOut := flag.String("catalog-out", "catalog", "directory to write per-state catalogs into")
	participantID := flag.String("participant-id", "", "participant ID (default: $MANDI_PARTICIPANT_ID, else agmarknet)")
	networkID := flag.String("network-id", "", "network ID (default: $APP_NETWORK_ID, else oan-dev)")
	withoutGeometry := flag.String("without-geometry", "publish", "what to do with geometry-less markets: publish or skip")

	// Publish
	doPublish := flag.Bool("publish", false, "publish the catalogs to the provider adapter")
	publishURL := flag.String("publish-url", "", "provider adapter base URL (default: $MANDI_PUBLISH_URL)")
	catalogIn := flag.String("catalog-in", "", "publish these already-built catalogs instead of collecting")
	dryRun := flag.Bool("dry-run", false, "print what would be posted without sending HTTP requests")
	retireOldFlag := flag.Bool("retire-old", false, "additionally publish the isActive:false tombstone for old catalog")

	flag.Parse()

	ctx := context.Background()

	pubCfg := publishConfig{
		publishURL: firstNonEmpty(*publishURL, os.Getenv("MANDI_PUBLISH_URL")),
		states:     splitStates(*states),
		dryRun:     *dryRun,
		retireOld:  *retireOldFlag,
	}

	// --catalog-in publishes what is already on disk and collects nothing. It
	// is the path for re-posting exactly what was reviewed.
	if *catalogIn != "" {
		if !*doPublish && !*retireOldFlag {
			fail("--catalog-in only makes sense with --publish or --retire-old")
		}
		pubCfg.catalogIn = *catalogIn
		publishAndExit(ctx, pubCfg)
		return
	}

	today := time.Now().Format("02-01-2006")
	if *fromDate == "" {
		*fromDate = today
	}
	if *toDate == "" {
		*toDate = today
	}

	built, summary, collection, err := run(ctx, runConfig{
		collect: config{
			baseURL: *baseURL,
			// CREDENTIALS FROM THE ENVIRONMENT, NEVER A FLAG. A flag value
			// lands in shell history and is visible in ps output to every
			// user on the host.
			user:     os.Getenv("MANDI_TOKEN_USER"),
			secret:   os.Getenv("MANDI_TOKEN_SECRET"),
			states:   splitStates(*states),
			fromDate: *fromDate,
			toDate:   *toDate,
		},
		build: buildConfig{
			catalogOut:      *catalogOut,
			participantID:   *participantID,
			networkID:       *networkID,
			withoutGeometry: *withoutGeometry,
			states:          splitStates(*states),
		},
	})
	if err != nil {
		fail("%v", err)
	}

	printCollectionSummary(collection)
	printBuildSummary(built, summary, buildConfig{catalogOut: *catalogOut, withoutGeometry: *withoutGeometry})

	if *doPublish || *retireOldFlag {
		pubCfg.catalogIn = *catalogOut
		// A state that failed to collect is not a state with no markets, so a
		// partial collection must not be published as though it were whole.
		if len(collection.StateErrors) > 0 {
			fail("%d states failed to collect; not publishing a partial set", len(collection.StateErrors))
		}
		publishAndExit(ctx, pubCfg)
		return
	}

	// Non-zero on a partial collection: a caller scripting this must be able
	// to tell a complete run from one missing a state.
	if len(collection.StateErrors) > 0 {
		os.Exit(1)
	}
}

// publishAndExit posts the catalogs and exits non-zero if anything did not
// reach the index intact.
func publishAndExit(ctx context.Context, cfg publishConfig) {
	res, err := publish(ctx, cfg)
	if err != nil {
		fail("publish: %v", err)
	}
	printPublishSummary(res, cfg)
	if res.HasFailures() {
		os.Exit(1)
	}
}

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

// fail prints one message and exits non-zero.
func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mandi_publish: "+format+"\n", args...)
	os.Exit(1)
}

// firstNonEmpty returns the first value that is set, so a flag beats an
// environment variable and both beat nothing.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func printBuildSummary(built []BuiltState, summary SkipSummary, cfg buildConfig) {
	fmt.Fprintf(os.Stderr, "built %d state catalogs into %s\n", len(built), cfg.catalogOut)
	for _, b := range built {
		fmt.Fprintf(os.Stderr, "  %s: %d markets -> %s (%s)\n", b.StateCode, b.Markets, b.Path, b.CatalogID)
	}
	if summary.ZeroCommodities > 0 {
		fmt.Fprintf(os.Stderr, "  skipped %d markets with zero commodities\n", summary.ZeroCommodities)
	}
	if summary.GeometryLess > 0 {
		fmt.Fprintf(os.Stderr, "  %d markets without coordinates (without-geometry=%s)\n", summary.GeometryLess, cfg.withoutGeometry)
	}
	if summary.EmptyStates > 0 {
		fmt.Fprintf(os.Stderr, "  %d states emitted no catalog file\n", summary.EmptyStates)
	}
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

// splitStates reads the comma-separated flag, dropping blanks so a trailing
// comma is not a state code.
func splitStates(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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

// collect runs the whole pipeline.
//
// Sequential deliberately. Thirty-six calls against a service that publishes no
// rate limit is the polite default, and concurrency here would buy seconds while
// risking a throttle that looks like data loss.
func collect(ctx context.Context, cfg config) (Collection, error) {
	if cfg.user == "" || cfg.secret == "" {
		return Collection{}, errors.New(
			"set MANDI_TOKEN_USER and MANDI_TOKEN_SECRET in the environment")
	}

	mappingBase, stop, err := serveMappings()
	if err != nil {
		return Collection{}, err
	}
	defer stop()

	mapper, closer, err := newMapper(ctx)
	if err != nil {
		return Collection{}, err
	}
	defer func() { _ = closer() }()

	client := newClient(cfg.baseURL)

	token, err := client.Token(ctx, cfg.user, cfg.secret)
	if err != nil {
		return Collection{}, err
	}

	stateCodes := cfg.states
	if len(stateCodes) == 0 {
		states, err := client.States(ctx, mapper, mappingBase, token)
		if err != nil {
			return Collection{}, err
		}
		for _, state := range states {
			stateCodes = append(stateCodes, state.Code)
		}
	}

	if len(stateCodes) == 0 {
		return Collection{}, errors.New("state list resolved to zero states; refusing to write an empty collection")
	}

	markets, err := client.Markets(ctx, mapper, mappingBase, token)
	if err != nil {
		return Collection{}, err
	}

	collection := Collection{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      Window{From: cfg.fromDate, To: cfg.toDate},
		Markets:     []CollectedMarket{},
		StateErrors: []StateError{},
		EmptyStates: []string{},
	}

	seen := make(map[int]bool)

	for _, code := range stateCodes {
		rows, err := client.StateMarkets(ctx, mapper, mappingBase, token, code, cfg.fromDate, cfg.toDate)
		if errors.Is(err, errNoUpstreamData) {
			// The upstream holds no mapping rows for this state, which is a
			// coverage fact rather than a failed run -- it says so with an
			// HTTP 400 and "No data available.". Recording it as a stateError
			// made 27 of 36 states read as 27 outages and exited non-zero on a
			// collection as complete as the upstream allows.
			collection.EmptyStates = append(collection.EmptyStates, code)
			continue
		}
		if err != nil {
			collection.StateErrors = append(collection.StateErrors,
				StateError{StateCode: code, Reason: err.Error()})
			continue
		}
		if len(rows) == 0 {
			collection.EmptyStates = append(collection.EmptyStates, code)
		}
		for _, cm := range join(rows, markets) {
			if seen[cm.MarketID] {
				continue
			}
			seen[cm.MarketID] = true
			collection.Markets = append(collection.Markets, cm)
		}
	}
	return collection, nil
}

// loadEnvFile reads a simple KEY=VALUE file and populates missing environment variables.
func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			if os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
}
