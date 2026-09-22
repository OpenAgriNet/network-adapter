package agmarket

// live_test.go drives each pipeline step against the REAL services, one step
// at a time, so a person can watch what each stage actually produces before
// the orchestrator exists to chain them.
//
// Every test here is skipped unless MANDI_LIVE=1, so `go test ./...` stays
// hermetic. The publishing step needs a second opt-in of its own
// (MANDI_LIVE_PUBLISH=1) because it is the only one with a side effect
// somebody else can see: it puts catalogues onto the network.
//
// These live in the package rather than a cmd/ because everything they
// exercise is unexported. That is also the reason this file exists at all --
// see dev_docs/plan-crawler-mandi/live-testing-guide.md.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/pipeline"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/sunbirdRegistry"
)

// liveOrSkip gates a test on real credentials being present, and reports which
// variable is missing rather than skipping silently.
func liveOrSkip(t *testing.T) map[string]string {
	t.Helper()

	if os.Getenv("MANDI_LIVE") != "1" {
		t.Skip("live test: set MANDI_LIVE=1 to run against the real services")
	}

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	resolved, err := pipeline.ResolveInputs(spec, os.LookupEnv)
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}

	for _, name := range []string{"baseUrl", "tokenUser", "tokenSecret"} {
		if resolved[name] == "" {
			t.Fatalf("live test: input %q is empty; source the repo's .env first", name)
		}
	}

	// Print what the run resolved to, redacted -- this is the first thing
	// worth seeing, because a wrong baseUrl or date explains most later
	// surprises on its own.
	t.Logf("resolved inputs: %v", pipeline.RedactedInputs(spec.Inputs, resolved))
	return resolved
}

// liveClient is a client pointed at the real upstream, already holding a token.
func liveClient(t *testing.T, resolved map[string]string) (*pipeline.Client, string) {
	t.Helper()
	client := pipeline.NewClient(resolved["baseUrl"])

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	token, err := client.Token(ctx, resolved["tokenUser"], resolved["tokenSecret"])
	if err != nil {
		t.Fatalf("token exchange failed: %v", err)
	}
	return client, token
}

// TestLive_1_Token proves the credentials work and nothing else.
func TestLive_1_Token(t *testing.T) {
	resolved := liveOrSkip(t)
	_, token := liveClient(t, resolved)

	// Length only. The token is a credential and this output ends up in
	// terminals and tickets.
	t.Logf("token exchange OK (%d characters)", len(token))
}

// TestLive_2_States fetches the state list -- the loop driver for everything
// after it.
func TestLive_2_States(t *testing.T) {
	resolved := liveOrSkip(t)
	client, token := liveClient(t, resolved)
	mapper, mappingBase := testMapper(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := client.Get(ctx, mapper, mappingBase+"/master-states.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": token})
	if err != nil {
		t.Fatalf("states: %v", err)
	}

	var states []struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &states); err != nil {
		t.Fatalf("states did not decode: %v", err)
	}
	if len(states) == 0 {
		t.Fatal("upstream returned zero states; walking nothing would read as 'India has no markets'")
	}

	t.Logf("%d states", len(states))
	for i, state := range states {
		if i == 5 {
			t.Logf("  ... and %d more", len(states)-5)
			break
		}
		t.Logf("  %s = %s", state.Code, state.Name)
	}
}

// TestLive_3_MasterMarkets fetches every market in India with its coordinates.
// This is the dataset the coordinate verdicts are computed from, so the
// counts it prints are the ones that explain later exclusions.
func TestLive_3_MasterMarkets(t *testing.T) {
	resolved := liveOrSkip(t)
	client, token := liveClient(t, resolved)
	mapper, mappingBase := testMapper(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	out, err := client.Get(ctx, mapper, mappingBase+"/master-markets.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": token})
	if err != nil {
		t.Fatalf("masterMarkets: %v", err)
	}

	var markets []Market
	if err := json.Unmarshal(out, &markets); err != nil {
		t.Fatalf("markets did not decode: %v", err)
	}

	var verdicts = map[string]int{}
	for _, market := range markets {
		verdicts[coordinateQuality(market.Latitude, market.Longitude)]++
	}
	t.Logf("%d markets; coordinate verdicts: %v", len(markets), verdicts)
	t.Log("  'missing' is the upstream leaving lat/lon null; 'suspect' is the same number in both fields")
}

// TestLive_4_StateRows fetches one state's market/commodity rows. Set
// MANDI_LIVE_STATE to choose the state (default MH), because a full run is 36
// sequential calls and this step is the one worth inspecting closely.
func TestLive_4_StateRows(t *testing.T) {
	resolved := liveOrSkip(t)
	client, token := liveClient(t, resolved)
	mapper, mappingBase := testMapper(t)

	state := os.Getenv("MANDI_LIVE_STATE")
	if state == "" {
		state = "MH"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, err := client.Get(ctx, mapper, mappingBase+"/market-commodity.yaml",
		"/v1/fetch-agmarknet-market-commodity-mapping", map[string]any{
			"token":     token,
			"stateCode": state,
			"fromDate":  resolved["fromDate"],
			"toDate":    resolved["toDate"],
		})
	if errors.Is(err, pipeline.ErrNoUpstreamData) {
		// Not a failure. The upstream is saying this state traded nothing in
		// this window -- a Sunday or a holiday does this for every state at
		// once. Treating it as an error is the exact mistake that once turned
		// 27 of 36 states into reported outages.
		t.Skipf("%s traded nothing over %s..%s (upstream: no data). "+
			"Set MANDI_FROM_DATE/MANDI_TO_DATE to a trading day to see rows.",
			state, resolved["fromDate"], resolved["toDate"])
	}
	if err != nil {
		t.Fatalf("stateRows(%s) over %s..%s: %v", state, resolved["fromDate"], resolved["toDate"], err)
	}

	var rows []StateMarket
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("rows did not decode: %v", err)
	}

	t.Logf("%s: %d market rows over %s..%s", state, len(rows), resolved["fromDate"], resolved["toDate"])
	for i, row := range rows {
		if i == 3 {
			t.Logf("  ... and %d more", len(rows)-3)
			break
		}
		t.Logf("  market %d %q, district %d, %d commodities",
			row.MarketID, row.MarketName, row.DistrictID, len(row.Commodities))
	}
}

// TestLive_5_CollectOneState runs the whole collection half for one state:
// fetch, join to the master coordinates, judge them, dedupe. This is the last
// step before anything is built, and its output is what a catalogue is made
// of.
func TestLive_5_CollectOneState(t *testing.T) {
	resolved := liveOrSkip(t)
	client, token := liveClient(t, resolved)
	mapper, mappingBase := testMapper(t)

	state := os.Getenv("MANDI_LIVE_STATE")
	if state == "" {
		state = "MH"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	markets := liveMasterMarkets(ctx, t, client, mapper, mappingBase, token)
	rows := liveStateRows(ctx, t, client, mapper, mappingBase, token, state, resolved)

	collected := dedupeByMarketID(deriveQuality(joinMarkets(rows, markets)))

	verdicts := map[string]int{}
	for _, market := range collected {
		verdicts[market.CoordinateQuality]++
	}
	t.Logf("%s: %d rows in, %d markets out, verdicts %v", state, len(rows), len(collected), verdicts)
	t.Log("  a row with no master match is KEPT -- it trades today, only its location is unknown")
}

// TestLive_6_BuildCatalogues builds real catalogue documents for one state and
// writes them where a person can read them. Nothing is published.
func TestLive_6_BuildCatalogues(t *testing.T) {
	resolved := liveOrSkip(t)
	client, token := liveClient(t, resolved)
	mapper, mappingBase := testMapper(t)

	state := os.Getenv("MANDI_LIVE_STATE")
	if state == "" {
		state = "MH"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	markets := liveMasterMarkets(ctx, t, client, mapper, mappingBase, token)
	rows := liveStateRows(ctx, t, client, mapper, mappingBase, token, state, resolved)
	collected := dedupeByMarketID(deriveQuality(joinMarkets(rows, markets)))

	built, summary, err := buildCatalogs(ctx, mapper, mappingBase+"/catalog.yaml", collected, catalogBuildConfig{
		ParticipantID:   resolved["participantId"],
		NetworkID:       resolved["networkId"],
		WindowFrom:      resolved["fromDate"],
		WindowTo:        resolved["toDate"],
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		TransactionID:   "live-test-transaction",
		MessageID:       "live-test-message",
		WithoutGeometry: resolved["withoutGeometry"],
	})
	if err != nil {
		t.Fatalf("buildCatalogs: %v", err)
	}

	outDir := os.Getenv("MANDI_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	if err := pipeline.WriteCatalogues(asCatalogues(built), outDir, "mandi-price"); err != nil {
		t.Fatalf("writeCatalogs: %v", err)
	}

	t.Logf("built %d catalogue(s) into %s", len(built), outDir)
	for _, catalog := range built {
		path := filepath.Join(outDir, "mandi-"+catalog.Slug+".json")
		info, _ := os.Stat(path)
		var size int64
		if info != nil {
			size = info.Size()
		}
		t.Logf("  %s -> %s (%d bytes)", catalog.CatalogID, path, size)
	}
	t.Logf("skipped: %d zero-commodity, %d geometry-less, %d states with nothing publishable",
		summary.ZeroCommodities, summary.GeometryLess, summary.EmptyStates)
	for _, excluded := range summary.Excluded {
		t.Logf("  excluded %d %q: %s", excluded.MarketID, excluded.MarketName, excluded.Reason)
	}
}

// TestLive_7_Publish PUTS CATALOGUES ONTO THE NETWORK. It needs its own
// opt-in for that reason: every other live test only reads.
func TestLive_7_Publish(t *testing.T) {
	resolved := liveOrSkip(t)
	if os.Getenv("MANDI_LIVE_PUBLISH") != "1" {
		t.Skip("this test publishes to the real network: set MANDI_LIVE_PUBLISH=1 to allow it")
	}

	dir := os.Getenv("MANDI_LIVE_OUT")
	if dir == "" {
		t.Fatal("set MANDI_LIVE_OUT to a directory built by TestLive_6_BuildCatalogues, " +
			"so this publishes catalogues you have already looked at")
	}

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := pipeline.PublishCatalogues(ctx, spec.Publish, resolved, dir, "mandi-price", 0)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}

	for _, outcome := range result.Outcomes {
		t.Logf("%s: %s -> %s %s", outcome.StateCode, outcome.CatalogID, outcome.Status, outcome.Reason)
	}
	if result.HasFailures() {
		t.Fatal("at least one catalogue did not reach the network intact " +
			"(PARTIAL counts: it means the catalogue indexed with resources missing)")
	}
}

func liveMasterMarkets(ctx context.Context, t *testing.T, client *pipeline.Client,
	mapper pipeline.Mapper, mappingBase, token string) []Market {
	t.Helper()
	out, err := client.Get(ctx, mapper, mappingBase+"/master-markets.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": token})
	if err != nil {
		t.Fatalf("masterMarkets: %v", err)
	}
	var markets []Market
	if err := json.Unmarshal(out, &markets); err != nil {
		t.Fatalf("markets did not decode: %v", err)
	}
	return markets
}

func liveStateRows(ctx context.Context, t *testing.T, client *pipeline.Client,
	mapper pipeline.Mapper, mappingBase, token, state string, resolved map[string]string) []StateMarket {
	t.Helper()
	out, err := client.Get(ctx, mapper, mappingBase+"/market-commodity.yaml",
		"/v1/fetch-agmarknet-market-commodity-mapping", map[string]any{
			"token":     token,
			"stateCode": state,
			"fromDate":  resolved["fromDate"],
			"toDate":    resolved["toDate"],
		})
	if errors.Is(err, pipeline.ErrNoUpstreamData) {
		t.Skipf("%s traded nothing over %s..%s (upstream: no data). "+
			"Set MANDI_FROM_DATE/MANDI_TO_DATE to a trading day.",
			state, resolved["fromDate"], resolved["toDate"])
	}
	if err != nil {
		t.Fatalf("stateRows(%s): %v", state, err)
	}
	var rows []StateMarket
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("rows did not decode: %v", err)
	}
	return rows
}

// TestLive_0_RegistryLookup exercises the lookup the design's registry gate
// depends on, against the real Sunbird RC, using the same plugin the adapter
// would use -- not a curl approximation of it.
//
// Nothing in this package calls the registry yet (mandi-pipeline-document.md
// §4.2-4.3 describes the gate; it is unbuilt). This test exists to prove the
// path works and to show exactly what the registry does and does not hold
// today, because that determines whether the gate could pass at all.
func TestLive_0_RegistryLookup(t *testing.T) {
	resolved := liveOrSkip(t)
	record := liveProviderRecord(t, resolved)

	t.Logf("resolved: participant=%s capability=%s baseUrl=%s",
		record.ParticipantID, record.CapabilityCode, record.BaseURL)
	for action, plan := range record.Actions {
		t.Logf("  action %q -> %s %s (mappings: %s)", action, plan.Method, plan.Path, plan.Mappings)
	}
	if _, ok := record.Actions["publish"]; !ok {
		t.Logf("  NO \"publish\" action: the crawler would find no pipeline to run for this capability")
	}
}

// liveProviderRecord looks this pipeline's own bindingKey up in the real
// Sunbird RC, using the same plugin the adapter would use.
func liveProviderRecord(t *testing.T, resolved map[string]string) *model.ProviderRecord {
	t.Helper()

	// The declared default is a container-network name; from a shell it has
	// to be localhost. Override with SUNBIRD_REGISTRY_URL.
	registryURL := resolved["registryUrl"]
	if override := os.Getenv("SUNBIRD_REGISTRY_URL"); override != "" {
		registryURL = override
	}
	t.Logf("registry: %s", registryURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, closer, err := sunbirdRegistry.New(ctx, nil, &sunbirdRegistry.Config{
		URL:            registryURL,
		Entity:         "Participant",
		ProviderEntity: "ProviderSchema",
		Timeout:        10,
	})
	if err != nil {
		t.Fatalf("sunbirdRegistry.New: %v", err)
	}
	defer func() { _ = closer() }()

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}

	// The bindingKey the pipeline's own inputs produce. This is what the gate
	// looks up -- not a hand-typed one, or the test would prove nothing about
	// what a real run would ask for.
	bindingKey := resolved["participantId"] + "|" + spec.Metadata.Capability
	t.Logf("bindingKey from this pipeline's inputs: %s", bindingKey)

	record, err := client.ProviderRecord(ctx, bindingKey)
	if err != nil {
		t.Logf("NOT FOUND: %v", err)
		t.Logf("  the registry gate would REFUSE this run")
		override := os.Getenv("MANDI_LIVE_BINDING_KEY")
		if override == "" {
			t.Skip("set MANDI_LIVE_BINDING_KEY to a binding the registry actually holds to see a record")
		}
		t.Logf("  retrying with MANDI_LIVE_BINDING_KEY=%s", override)
		if record, err = client.ProviderRecord(ctx, override); err != nil {
			t.Fatalf("ProviderRecord(%s): %v", override, err)
		}
	}
	return record
}

// TestLive_8_Tick runs the pre-flight a scheduled tick runs, end to end and
// against the real registry: look the capability up, let the gate find the
// publish action, load the pipeline that action names, and ask the schedule
// whether now is the time.
//
// It stops there deliberately. Everything after this decision is steps 1-7,
// which the earlier tests already cover one at a time; what has never been
// exercised against the live registry is the chain that decides to run them.
func TestLive_8_Tick(t *testing.T) {
	resolved := liveOrSkip(t)
	record := liveProviderRecord(t, resolved)

	pipelinePath, err := pipeline.PipelinePathFor(record)
	if err != nil {
		t.Fatalf("registry gate refused the live record: %v", err)
	}
	t.Logf("gate: the registry sanctions pipeline %s", pipelinePath)

	spec, err := pipeline.LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	t.Logf("pipeline: %s (capability %s, provider %s)",
		spec.Metadata.Name, spec.Metadata.Capability, spec.Metadata.Provider)
	t.Logf("schedule: cron %q %s", spec.Schedule.Cron, spec.Schedule.Timezone)

	// DryRun is the real entry point, stopped before it fetches: it runs the
	// gate, the run log and the schedule exactly as a scheduled tick would.
	now := time.Now()
	decision, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Collector: Collector{},
		Record:    record,
		Now:       now,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("tick against the live record: %v", err)
	}
	t.Logf("decision at %s: due=%v (%s)", now.Format(time.RFC3339), decision.Due, decision.Reason)

	// Never-run is the state a fresh deployment is in, and this pipeline
	// fires at 00:00 -- so at any wall-clock time today the answer must be
	// "due". A false here means a tick would never fire at all.
	if !decision.Due {
		t.Errorf("a never-run pipeline is not due: %s", decision.Reason)
	}

	// Same record, with a run log saying it just ran: the tick must go quiet
	// rather than publishing a second time the same day.
	repeat, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Collector: Collector{},
		Record:    record,
		RunLog:    &fakeRunLog{last: now},
		Now:       now,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("tick after a run: %v", err)
	}
	t.Logf("decision having just run: due=%v (%s)", repeat.Due, repeat.Reason)
	if repeat.Due {
		t.Error("due again immediately after running; this would publish on every tick")
	}
}

// asCatalogues is the same conversion Collect does, so a live test writes the
// files a real run would.
func asCatalogues(built []BuiltCatalog) []pipeline.Catalogue {
	out := make([]pipeline.Catalogue, 0, len(built))
	for _, catalog := range built {
		out = append(out, pipeline.Catalogue{
			Slug:      catalog.Slug,
			CatalogID: catalog.CatalogID,
			Content:   catalog.Content,
		})
	}
	return out
}
