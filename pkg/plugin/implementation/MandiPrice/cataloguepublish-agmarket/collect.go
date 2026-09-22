package agmarket

// collect.go is this pipeline's domain half: everything between "the frame
// says it is due" and "here are catalogues to publish".
//
// The frame (internal/pipeline) owns the registry gate, the run log, the cron
// schedule, input resolution, the upstream client and the publish step --
// none of which know what a mandi is. What lives here is the part that does:
// which Agmarknet endpoints to call, that markets are grouped by state, and
// that a market with no usable coordinate cannot be found by a proximity
// search.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/pipeline"
	"github.com/google/uuid"
)

// Upstream paths and the mapping that shapes each call. Together with the
// mappings directory these are this pipeline's four upstream interactions.
const (
	masterDataPath   = "/v1/fetch-agmarknet-master-data"
	stateRowsPath    = "/v1/fetch-agmarknet-market-commodity-mapping"
	statesMapping    = "/master-states.yaml"
	marketsMapping   = "/master-markets.yaml"
	stateRowsMapping = "/market-commodity.yaml"
	catalogMapping   = "/catalog.yaml"
)

// Capability is the code the registry knows this pipeline by.
const Capability = "openagrinet:MandiPrice"

// RegistryPipelinePath is the repo-relative path the registry's publish action
// must name for this pipeline to run. The gate compares against it rather than
// deriving it, so a registry pointing elsewhere is refused instead of being
// served by whatever this binary happens to embed.
const RegistryPipelinePath = "pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml"

// Collector implements pipeline.Collector for Agmarknet mandi prices.
type Collector struct{}

func (Collector) Capability() string { return Capability }

func (Collector) Pipeline() pipeline.Files {
	return pipeline.Files{FS: Files, Path: PipelinePath, RegistryPath: RegistryPipelinePath}
}

// Counter names this collector reports. Named constants because an operator
// reads them in a log line and a test asserts on them, so a typo in either
// would otherwise go unnoticed.
const (
	CounterStates      = "states"
	CounterEmptyStates = "emptyStates"
	CounterMarkets     = "markets"
)

// Collect walks every state the upstream lists, joins each state's trading
// rows to the master market list for coordinates, and builds one catalogue per
// state.
//
// A state the upstream reports no data for is EMPTY, not broken -- a Sunday
// does this to all of them at once -- so it is counted and skipped. Anything
// else is a real failure and is counted in Errors, which reaches the publish
// step's refuseWhen rule: publishing then would replace a whole state's
// catalogue with a partial one, or with nothing.
func (Collector) Collect(ctx context.Context, env pipeline.RunEnv) (pipeline.CollectResult, error) {
	var result pipeline.CollectResult

	states, err := fetchStates(ctx, env.Client, env.Mapper, env.MappingBase, env.Token)
	if err != nil {
		return result, err
	}

	markets, err := fetchMasterMarkets(ctx, env.Client, env.Mapper, env.MappingBase, env.Token)
	if err != nil {
		return result, err
	}

	var collected []CollectedMarket
	emptyStates := 0
	for _, state := range states {
		rows, err := fetchStateRows(ctx, env.Client, env.Mapper, env.MappingBase, env.Token, state.Code, env.Inputs)
		switch {
		case errors.Is(err, pipeline.ErrNoUpstreamData):
			emptyStates++
			env.Log.DebugContext(ctx, "mandi: state traded nothing in this window", "state", state.Code)
			continue
		case err != nil:
			result.Errors++
			env.Log.ErrorContext(ctx, "mandi: state collection failed", "state", state.Code, "error", err)
			continue
		}
		collected = append(collected, joinMarkets(rows, markets)...)
	}
	collected = dedupeByMarketID(deriveQuality(collected))

	built, summary, err := buildCatalogs(ctx, env.Mapper, env.MappingBase+catalogMapping, collected, catalogBuildConfig{
		ParticipantID:   env.Inputs["participantId"],
		NetworkID:       env.Inputs["networkId"],
		WindowFrom:      env.Inputs["fromDate"],
		WindowTo:        env.Inputs["toDate"],
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		TransactionID:   uuid.NewString(),
		MessageID:       uuid.NewString(),
		WithoutGeometry: env.Inputs["withoutGeometry"],
	})
	if err != nil {
		return result, fmt.Errorf("building catalogues: %w", err)
	}

	for _, catalog := range built {
		result.Catalogues = append(result.Catalogues, pipeline.Catalogue{
			Slug:      catalog.Slug,
			CatalogID: catalog.CatalogID,
			Content:   catalog.Content,
		})
	}

	// summary.EmptyStates counts states that returned markets but had none
	// worth publishing -- a different thing from a state the upstream had no
	// data for, and reported separately so one number does not stand for two
	// situations that call for opposite responses.
	result.Counters = map[string]int{
		CounterStates:            len(states),
		CounterEmptyStates:       emptyStates,
		CounterMarkets:           len(collected),
		"zeroCommodityMarkets":   summary.ZeroCommodities,
		"geometryLessMarkets":    summary.GeometryLess,
		"statesWithNothingToSay": summary.EmptyStates,
	}
	return result, nil
}

// stateRef is the two fields the state list contributes: the code every later
// call is keyed on, and the name a person reads.
type stateRef struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func fetchStates(ctx context.Context, client *pipeline.Client, mapper pipeline.Mapper, mappingBase, token string) ([]stateRef, error) {
	out, err := client.Get(ctx, mapper, mappingBase+statesMapping, masterDataPath, map[string]any{"token": token})
	if err != nil {
		return nil, fmt.Errorf("fetching the state list: %w", err)
	}
	var states []stateRef
	if err := json.Unmarshal(out, &states); err != nil {
		return nil, fmt.Errorf("the state list did not decode: %w", err)
	}
	if len(states) == 0 {
		// Walking an empty list would publish nothing and report success,
		// which reads as "India has no markets today".
		return nil, fmt.Errorf("the upstream returned no states")
	}
	return states, nil
}

func fetchMasterMarkets(ctx context.Context, client *pipeline.Client, mapper pipeline.Mapper, mappingBase, token string) ([]Market, error) {
	out, err := client.Get(ctx, mapper, mappingBase+marketsMapping, masterDataPath, map[string]any{"token": token})
	if err != nil {
		return nil, fmt.Errorf("fetching the master market list: %w", err)
	}
	var markets []Market
	if err := json.Unmarshal(out, &markets); err != nil {
		return nil, fmt.Errorf("the master market list did not decode: %w", err)
	}
	return markets, nil
}

func fetchStateRows(ctx context.Context, client *pipeline.Client, mapper pipeline.Mapper,
	mappingBase, token, stateCode string, resolved map[string]string) ([]StateMarket, error) {
	out, err := client.Get(ctx, mapper, mappingBase+stateRowsMapping, stateRowsPath, map[string]any{
		"token":     token,
		"stateCode": stateCode,
		"fromDate":  resolved["fromDate"],
		"toDate":    resolved["toDate"],
	})
	if err != nil {
		// Wrapped so errors.Is still sees pipeline.ErrNoUpstreamData -- the caller
		// distinguishes "this state traded nothing" from "this state failed",
		// and collapsing the two is what once turned 27 of 36 quiet states
		// into reported outages.
		return nil, fmt.Errorf("state %s: %w", stateCode, err)
	}
	var rows []StateMarket
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("state %s rows did not decode: %w", stateCode, err)
	}
	return rows, nil
}
