package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// SkipSummary tracks markets and states skipped during building.
type SkipSummary struct {
	ZeroCommodities int
	GeometryLess    int
	EmptyStates     int
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
				summary.ZeroCommodities++
				continue
			}
			if m.CoordinateQuality != coordinateOK {
				summary.GeometryLess++
				if cfg.withoutGeometry == withoutGeometrySkip {
					continue
				}
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

		txnID := cfg.fixedTxnID
		if txnID == "" {
			txnID = uuid.New().String()
		}
		msgID := cfg.fixedMsgID
		if msgID == "" {
			msgID = uuid.New().String()
		}
		genAt := cfg.fixedGeneratedAt
		if genAt == "" {
			if col.GeneratedAt != "" {
				genAt = col.GeneratedAt
			} else {
				genAt = time.Now().UTC().Format(time.RFC3339)
			}
		}

		input := map[string]any{
			"response": publishableMarkets,
			"_local": map[string]any{
				"participantId":          cfg.participantID,
				"networkId":              cfg.networkID,
				"stateCode":              stateCode,
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
			return nil, summary, fmt.Errorf("transform state %s: %w", stateCode, err)
		}

		// Pretty print JSON
		var indented bytes.Buffer
		if err := json.Indent(&indented, mappedBytes, "", "  "); err != nil {
			return nil, summary, fmt.Errorf("indent JSON for state %s: %w", stateCode, err)
		}

		outFileName := fmt.Sprintf("mandi-%s.json", stateCode)
		outFilePath := filepath.Join(cfg.catalogOut, outFileName)
		if err := os.WriteFile(outFilePath, indented.Bytes(), 0o644); err != nil {
			return nil, summary, fmt.Errorf("write catalog file %s: %w", outFilePath, err)
		}

		catalogID := fmt.Sprintf("%s/mandi-%s", cfg.participantID, stateCode)
		builtStates = append(builtStates, BuiltState{
			StateCode: stateCode,
			CatalogID: catalogID,
			Path:      outFilePath,
			Markets:   len(publishableMarkets),
		})
	}

	return builtStates, summary, nil
}
