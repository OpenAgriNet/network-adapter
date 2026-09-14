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
// The discovery service caps a catalog at 256 geometries -- measured, see the
// design spec. A state within that budget becomes one catalog; a state over
// it is split into several numbered catalogs (mandi-<STATE>-1.json, -2, ...).
package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/beckn-one/beckn-onix/tools/publish/internal/catalogpublish"
)

// The catalog whose single India-wide polygon resource this work replaces.
//
// That resource matches every S_DWITHIN at any radius -- measured: a discover
// 300 km from anything still returns it -- so it must go, or every proximity
// answer carries a resource that says only "somewhere in India".
const oldCatalogID = "cat-agmarknet-mandi-prices"

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

	pubCfg := catalogpublish.Config{
		PublishURL:     firstNonEmpty(*publishURL, os.Getenv("MANDI_PUBLISH_URL")),
		States:         splitStates(*states),
		DryRun:         *dryRun,
		RetireOld:      *retireOldFlag,
		FilenamePrefix: "mandi",
		AddressHint:    "pass --publish-url or set MANDI_PUBLISH_URL",
		OldCatalogID:   oldCatalogID,
	}

	// --catalog-in publishes what is already on disk and collects nothing. It
	// is the path for re-posting exactly what was reviewed.
	if *catalogIn != "" {
		if !*doPublish && !*retireOldFlag {
			fail("--catalog-in only makes sense with --publish or --retire-old")
		}
		pubCfg.CatalogIn = *catalogIn
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
		pubCfg.CatalogIn = *catalogOut
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
func publishAndExit(ctx context.Context, cfg catalogpublish.Config) {
	res, err := catalogpublish.Publish(ctx, cfg)
	if err != nil {
		fail("publish: %v", err)
	}
	printPublishSummary(res, cfg)
	if res.HasFailures() {
		os.Exit(1)
	}
}

