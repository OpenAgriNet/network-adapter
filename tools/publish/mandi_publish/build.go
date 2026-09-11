package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/google/uuid"
)

// Geometry handling options.
const (
	withoutGeometryPublish = "publish"
	withoutGeometrySkip    = "skip"
)

// buildConfig configures catalog building.
type buildConfig struct {
	catalogOut      string
	participantID   string
	networkID       string
	withoutGeometry string
	states          []string

	// For deterministic testing
	fixedTxnID       string
	fixedMsgID       string
	fixedGeneratedAt string
}

// catalogGeometryBudget caps how many geometries go into one catalog.
//
// The discovery service refuses to index past 256 geometries per catalog, and
// the limit is CUMULATIVE across publishes to the same catalogId -- measured
// 2026-09-11: batches of 200, 200 and 144 geometries into one catalog answered
// ACCEPTED, then PARTIAL with 144 errors, then PARTIAL with 288, each error
// count being cumulative minus 256. So a catalog is capped by the total
// geometry it will ever hold, and merging batches does not evade it.
//
// Only markets with a usable coordinate publish a geometry, so a state of
// mostly coordinate-less markets fits in one catalog however many markets it
// has.
const catalogGeometryBudget = 256

// geometryCost is what one market spends against the budget: one Point when
// its coordinate survived the collector's verdict, nothing otherwise.
func geometryCost(market CollectedMarket) int {
	if market.CoordinateQuality == coordinateOK && market.Latitude != nil && market.Longitude != nil {
		return 1
	}
	return 0
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

// SkipSummary tracks markets and states skipped during building.
type SkipSummary struct {
	ZeroCommodities int
	GeometryLess    int
	EmptyStates     int

	// Excluded are markets kept OUT of the catalog entirely.
	Excluded []ExcludedMarket

	// GeometryLessMarkets are IN the catalog but carry no Point, so no
	// proximity search can find them. Each keeps its own verdict: missing,
	// suspect and outOfBounds are different upstream defects.
	GeometryLessMarkets []ExcludedMarket
}

// BuiltState records the outcome of building one state's catalog.
type BuiltState struct {
	StateCode string
	CatalogID string
	Path      string
	Markets   int
}

// defaultBuildConfig applies defaults and environment variable overrides.
func defaultBuildConfig(cfg buildConfig) buildConfig {
	if cfg.catalogOut == "" {
		cfg.catalogOut = "catalog"
	}
	if cfg.participantID == "" {
		if env := os.Getenv("MANDI_PARTICIPANT_ID"); env != "" {
			cfg.participantID = env
		} else {
			cfg.participantID = "agmarknet"
		}
	}
	if cfg.networkID == "" {
		if env := os.Getenv("APP_NETWORK_ID"); env != "" {
			cfg.networkID = env
		} else {
			cfg.networkID = "oan-dev"
		}
	}
	if cfg.withoutGeometry == "" {
		cfg.withoutGeometry = withoutGeometryPublish
	}
	return cfg
}

// chunkMarkets splits markets into runs that each stay within the geometry
// budget, preserving order so the partition is deterministic and a market
// lands in exactly one chunk.
//
// A market costing nothing never forces a split; a run is cut only when adding
// the next market would exceed the budget.
func chunkMarkets(markets []CollectedMarket, budget int) [][]CollectedMarket {
	var chunks [][]CollectedMarket
	var current []CollectedMarket
	spent := 0

	for _, market := range markets {
		cost := geometryCost(market)
		if len(current) > 0 && spent+cost > budget {
			chunks = append(chunks, current)
			current, spent = nil, 0
		}
		current = append(current, market)
		spent += cost
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

// buildCollection builds from a collection already in memory.
//
// This is what the one-stage run uses: collect hands its result straight here,
// so a publish no longer has to go through a file on disk. build() is the same
// thing with a file read in front of it.
func buildCollection(ctx context.Context, collection Collection, cfg buildConfig) ([]BuiltState, SkipSummary, error) {
	mappingBase, stop, err := serveMappings()
	if err != nil {
		return nil, SkipSummary{}, err
	}
	defer stop()

	mapper, closer, err := newMapper(ctx)
	if err != nil {
		return nil, SkipSummary{}, err
	}
	defer func() { _ = closer() }()

	return buildFromCollection(ctx, collection, cfg, mapper, mappingBase)
}

// buildFromCollection performs the in-memory grouping, filtering, and JSONata transformation.
func buildFromCollection(ctx context.Context, col Collection, cfg buildConfig, m mapperRunner, mappingBase string) ([]BuiltState, SkipSummary, error) {
	cfg = defaultBuildConfig(cfg)
	if cfg.withoutGeometry != withoutGeometryPublish && cfg.withoutGeometry != withoutGeometrySkip {
		return nil, SkipSummary{}, fmt.Errorf("invalid without-geometry option %q: must be publish or skip", cfg.withoutGeometry)
	}

	// Filter states if specified
	stateFilter := make(map[string]bool)
	for _, s := range cfg.states {
		stateFilter[s] = true
	}

	// Group markets by state code
	marketsByState := make(map[string][]CollectedMarket)
	stateNames := make(map[string]string)
	for _, m := range col.Markets {
		if len(stateFilter) > 0 && !stateFilter[m.StateCode] {
			continue
		}
		marketsByState[m.StateCode] = append(marketsByState[m.StateCode], m)
		if stateNames[m.StateCode] == "" && m.StateName != "" {
			stateNames[m.StateCode] = m.StateName
		}
	}

	// Also count empty states from collection that were in stateFilter
	var summary SkipSummary
	for _, es := range col.EmptyStates {
		if len(stateFilter) == 0 || stateFilter[es] {
			summary.EmptyStates++
		}
	}

	// Collect state codes in deterministic order
	var stateCodes []string
	for code := range marketsByState {
		stateCodes = append(stateCodes, code)
	}
	sort.Strings(stateCodes)

	if err := os.MkdirAll(cfg.catalogOut, 0o755); err != nil {
		return nil, summary, fmt.Errorf("create catalog output directory: %w", err)
	}

	var builtStates []BuiltState

	for _, stateCode := range stateCodes {
		rawMarkets := marketsByState[stateCode]
		stateName := stateNames[stateCode]

		var publishableMarkets []CollectedMarket
		for _, m := range rawMarkets {
			if len(m.Commodities) == 0 {
				// supportedCommodities has minItems 1, so this resource would
				// fail the whole catalog at publish time. Recorded by name,
				// because a market silently missing from the network is
				// indistinguishable from one that never existed.
				summary.ZeroCommodities++
				summary.Excluded = append(summary.Excluded, ExcludedMarket{
					MarketID:   m.MarketID,
					MarketName: strings.TrimSpace(m.MarketName),
					StateCode:  m.StateCode,
					Reason:     "upstream reported no commodities trading in this window",
				})
				continue
			}
			if m.CoordinateQuality != coordinateOK {
				summary.GeometryLess++
				record := ExcludedMarket{
					MarketID:   m.MarketID,
					MarketName: strings.TrimSpace(m.MarketName),
					StateCode:  m.StateCode,
					Reason:     fmt.Sprintf("coordinate %s", m.CoordinateQuality),
				}
				if cfg.withoutGeometry == withoutGeometrySkip {
					summary.Excluded = append(summary.Excluded, record)
					continue
				}
				summary.GeometryLessMarkets = append(summary.GeometryLessMarkets, record)
			}
			publishableMarkets = append(publishableMarkets, m)
		}

		if len(publishableMarkets) == 0 {
			summary.EmptyStates++
			continue
		}

		// Sort markets deterministically by marketId
		sort.Slice(publishableMarkets, func(i, j int) bool {
			return publishableMarkets[i].MarketID < publishableMarkets[j].MarketID
		})

		genAt := cfg.fixedGeneratedAt
		if genAt == "" {
			if col.GeneratedAt != "" {
				genAt = col.GeneratedAt
			} else {
				genAt = time.Now().UTC().Format(time.RFC3339)
			}
		}

		// One catalog per chunk. A state small enough to fit keeps its plain
		// name; only a split state gets numbered, so the common case reads the
		// way it always did.
		chunks := chunkMarkets(publishableMarkets, catalogGeometryBudget)
		for index, chunk := range chunks {
			slug := stateCode
			if len(chunks) > 1 {
				slug = fmt.Sprintf("%s-%d", stateCode, index+1)
			}

			txnID := cfg.fixedTxnID
			if txnID == "" {
				txnID = uuid.New().String()
			}
			msgID := cfg.fixedMsgID
			if msgID == "" {
				msgID = uuid.New().String()
			}

			input := map[string]any{
				"response": chunk,
				"_local": map[string]any{
					"participantId":          cfg.participantID,
					"networkId":              cfg.networkID,
					"catalogSlug":            slug,
					"stateName":              stateName,
					"windowFrom":             col.Window.From,
					"windowTo":               col.Window.To,
					"generatedAt":            genAt,
					"transactionId":          txnID,
					"messageId":              msgID,
					"publishWithoutGeometry": cfg.withoutGeometry,
				},
			}

			mappedBytes, err := m.Transform(ctx, mappingBase+"/catalog.yaml", definition.DirectionResponse, input)
			if err != nil {
				return nil, summary, fmt.Errorf("transform state %s: %w", slug, err)
			}

			// Pretty print JSON
			var indented bytes.Buffer
			if err := json.Indent(&indented, mappedBytes, "", "  "); err != nil {
				return nil, summary, fmt.Errorf("indent JSON for state %s: %w", slug, err)
			}

			outFileName := fmt.Sprintf("mandi-%s.json", slug)
			outFilePath := filepath.Join(cfg.catalogOut, outFileName)
			if err := os.WriteFile(outFilePath, indented.Bytes(), 0o644); err != nil {
				return nil, summary, fmt.Errorf("write catalog file %s: %w", outFilePath, err)
			}

			builtStates = append(builtStates, BuiltState{
				StateCode: stateCode,
				CatalogID: fmt.Sprintf("%s/mandi-%s", cfg.participantID, slug),
				Path:      outFilePath,
				Markets:   len(chunk),
			})
		}
	}

	return builtStates, summary, nil
}
