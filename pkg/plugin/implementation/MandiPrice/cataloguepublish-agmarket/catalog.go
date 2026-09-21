package agmarket

// catalog.go turns a collection of markets into catalog/publish documents,
// one per state and, where a state carries more geometry than Discovery will
// index, one per chunk of that state.
//
// Ported from tools/publish/mandi_publish/build.go, whose buildFromCollection
// this is. The comments here are that tool's, kept because they record
// measured facts about the live network and the upstream data, not opinions:
// the geometry cap was observed in a real publish, and naming an excluded
// market instead of counting it is a lesson from a report nobody could act on.
//
// What this file does NOT do is interpret the pipeline YAML's `catalog:`
// block: the exclude/annotate/chunk rules there are JSONata strings, and
// evaluating them needs the expression engine the pipeline runner will have.
// The behaviour below is the same behaviour those rules describe, written in
// Go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// Geometry handling options: what to do with a market whose coordinate did
// not survive the collector's verdict.
const (
	withoutGeometryPublish = "publish"
	withoutGeometrySkip    = "skip"
)

// BuiltCatalog is one rendered catalog/publish document and the identity it
// was rendered under, held in memory so a caller can publish it directly or
// write it out for a human to read first.
type BuiltCatalog struct {
	StateCode string
	Slug      string
	CatalogID string
	Content   []byte
}

// ExcludedMarket is one market and why it did not get a full resource.
//
// Named individually rather than counted: "95 markets have missing
// coordinates" tells nobody WHICH, and the whole point of reporting them is
// that somebody can go and look one up.
type ExcludedMarket struct {
	MarketID   int
	MarketName string
	StateCode  string
	Reason     string
}

// SkipSummary tracks markets skipped or degraded during building.
type SkipSummary struct {
	ZeroCommodities int
	GeometryLess    int

	// Excluded are markets kept OUT of the catalog entirely.
	Excluded []ExcludedMarket

	// GeometryLessMarkets are IN the catalog but carry no Point, so no
	// proximity search can find them. Each keeps its own verdict: missing,
	// suspect and outOfBounds are different upstream defects.
	GeometryLessMarkets []ExcludedMarket
}

// catalogBuildConfig is everything the building needs that the markets
// themselves do not carry.
//
// GeneratedAt, TransactionID and MessageID are passed in rather than minted
// here so a caller decides whether a run is reproducible; a test fixes them
// and gets byte-identical output twice.
type catalogBuildConfig struct {
	ParticipantID   string
	NetworkID       string
	WindowFrom      string
	WindowTo        string
	GeneratedAt     string
	TransactionID   string
	MessageID       string
	WithoutGeometry string

	// Budget overrides the geometry cap per catalog. Zero or less means
	// CatalogGeometryBudget, which is the number the live service enforces.
	Budget int
}

// buildCatalogs groups markets by state, applies the two exclusion rules,
// orders and chunks what is left, and renders each chunk through the catalog
// mapping.
//
// A state left with nothing publishable produces no catalog rather than an
// empty one: publishing a catalog with zero resources would retire the
// state's markets from the network on the next MERGE.
func buildCatalogs(ctx context.Context, m mapperRunner, mappingRef string, markets []CollectedMarket, cfg catalogBuildConfig) ([]BuiltCatalog, SkipSummary, error) {
	var summary SkipSummary

	if cfg.WithoutGeometry == "" {
		cfg.WithoutGeometry = withoutGeometryPublish
	}
	if cfg.WithoutGeometry != withoutGeometryPublish && cfg.WithoutGeometry != withoutGeometrySkip {
		return nil, summary, fmt.Errorf("invalid without-geometry option %q: must be publish or skip", cfg.WithoutGeometry)
	}

	budget := cfg.Budget
	if budget <= 0 {
		budget = CatalogGeometryBudget
	}

	// Group by state code, remembering each state's name for the descriptor.
	marketsByState := make(map[string][]CollectedMarket)
	stateNames := make(map[string]string)
	for _, market := range markets {
		marketsByState[market.StateCode] = append(marketsByState[market.StateCode], market)
		if stateNames[market.StateCode] == "" && market.StateName != "" {
			stateNames[market.StateCode] = market.StateName
		}
	}

	stateCodes := make([]string, 0, len(marketsByState))
	for code := range marketsByState {
		stateCodes = append(stateCodes, code)
	}
	sort.Strings(stateCodes)

	var built []BuiltCatalog

	for _, stateCode := range stateCodes {
		stateName := stateNames[stateCode]

		var publishable []CollectedMarket
		for _, market := range marketsByState[stateCode] {
			if len(market.Commodities) == 0 {
				// supportedCommodities has minItems 1, so this resource would
				// fail the whole catalog at publish time. Recorded by name,
				// because a market silently missing from the network is
				// indistinguishable from one that never existed.
				summary.ZeroCommodities++
				summary.Excluded = append(summary.Excluded, ExcludedMarket{
					MarketID:   market.MarketID,
					MarketName: strings.TrimSpace(market.MarketName),
					StateCode:  market.StateCode,
					Reason:     "upstream reported no commodities trading in this window",
				})
				continue
			}
			if market.CoordinateQuality != coordinateOK {
				summary.GeometryLess++
				record := ExcludedMarket{
					MarketID:   market.MarketID,
					MarketName: strings.TrimSpace(market.MarketName),
					StateCode:  market.StateCode,
					Reason:     fmt.Sprintf("coordinate %s", market.CoordinateQuality),
				}
				if cfg.WithoutGeometry == withoutGeometrySkip {
					summary.Excluded = append(summary.Excluded, record)
					continue
				}
				summary.GeometryLessMarkets = append(summary.GeometryLessMarkets, record)
			}
			publishable = append(publishable, market)
		}

		if len(publishable) == 0 {
			continue
		}

		// Sorted before chunking, so a market lands in the same chunk on
		// every run and two runs of the same collection read the same way.
		sort.Slice(publishable, func(i, j int) bool {
			return publishable[i].MarketID < publishable[j].MarketID
		})

		// One catalog per chunk. A state small enough to fit keeps its plain
		// name; only the second and later chunks are numbered, so the common
		// case reads the way it always did and an unsplit state's catalogId
		// never changes just because another state grew.
		chunks := chunkMarkets(publishable, budget)
		for index, chunk := range chunks {
			slug := stateCode
			if index > 0 {
				slug = fmt.Sprintf("%s-%d", stateCode, index+1)
			}

			input := map[string]any{
				"response": chunk,
				"_local": map[string]any{
					"participantId":          cfg.ParticipantID,
					"networkId":              cfg.NetworkID,
					"catalogSlug":            slug,
					"stateName":              stateName,
					"windowFrom":             cfg.WindowFrom,
					"windowTo":               cfg.WindowTo,
					"generatedAt":            cfg.GeneratedAt,
					"transactionId":          cfg.TransactionID,
					"messageId":              cfg.MessageID,
					"publishWithoutGeometry": cfg.WithoutGeometry,
				},
			}

			content, err := m.Transform(ctx, mappingRef, definition.DirectionResponse, input)
			if err != nil {
				return nil, summary, fmt.Errorf("transform state %s: %w", slug, err)
			}

			built = append(built, BuiltCatalog{
				StateCode: stateCode,
				Slug:      slug,
				CatalogID: fmt.Sprintf("catalog:mandi-price:%s", slug),
				Content:   content,
			})
		}
	}

	return built, summary, nil
}

// writeCatalogs writes each built catalog to <dir>/<filenamePrefix>-<slug>.json.
//
// Indented, because these files exist to be read: somebody reviews what is
// about to go onto the network, and a single-line document of several hundred
// markets cannot be reviewed. The publish step reads the same directory back,
// which is why the naming is fixed rather than a caller's choice.
func writeCatalogs(built []BuiltCatalog, dir, filenamePrefix string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create catalog output directory: %w", err)
	}

	for _, catalog := range built {
		var indented bytes.Buffer
		if err := json.Indent(&indented, catalog.Content, "", "  "); err != nil {
			return fmt.Errorf("indent JSON for state %s: %w", catalog.Slug, err)
		}

		path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", filenamePrefix, catalog.Slug))
		if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write catalog file %s: %w", path, err)
		}
	}
	return nil
}
