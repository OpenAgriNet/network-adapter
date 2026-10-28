package catalogcrawler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type sent struct {
	entry   catalog.CatalogEntry
	content json.RawMessage
}

type stubSink struct {
	mu    sync.Mutex
	items []sent
	err   error
}

func (q *stubSink) Send(_ context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return crawlmanager.SinkOutcome{}, q.err
	}
	q.items = append(q.items, sent{entry: entry, content: content})
	return crawlmanager.SinkOutcome{Accepted: true}, nil
}

func (q *stubSink) ids() []string {
	out := make([]string, 0, len(q.items))
	for _, it := range q.items {
		out = append(out, it.entry.CatalogID)
	}
	return out
}

type stubAsker struct {
	byNetwork map[string][]json.RawMessage
	err       error
}

func (a stubAsker) discover(_ context.Context, network model.AdmittedNetwork, _ string,
	each func([]json.RawMessage) error) error {
	if a.err != nil {
		return a.err
	}
	page := a.byNetwork[network.ID]
	if len(page) == 0 {
		return nil
	}
	return each(page)
}

// blockingAsker holds a crawl inside discover until release is closed, and
// counts how many crawls got that far.
type blockingAsker struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (a *blockingAsker) discover(_ context.Context, _ model.AdmittedNetwork, _ string,
	_ func([]json.RawMessage) error) error {
	if a.calls.Add(1) == 1 {
		close(a.started)
	}
	<-a.release
	return nil
}

func catalogJSON(id string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","descriptor":{"name":"` + id + `"}}`)
}

func newCrawl(asker networkAsker, out catalogSink, networks ...model.AdmittedNetwork) *networkCrawl {
	return &networkCrawl{
		asker:        asker,
		sink:         out,
		registry:     stubNetworkRegistry{networks: networks},
		ourNetworkID: ourNetwork,
		log:          quietLog(),
	}
}

func TestNetworkCrawlPublishesWhatEachNetworkReturned(t *testing.T) {
	t.Parallel()

	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		"a.oan.local": {catalogJSON("cat-1"), catalogJSON("cat-2")},
		"b.oan.local": {catalogJSON("cat-3")},
	}}, out,
		network("a.oan.local", asSource),
		network("b.oan.local", asBoth),
	)

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run() returned an unexpected error: %v", err)
	}
	if len(out.items) != 3 {
		t.Fatalf("expected 3 queued, got %d", len(out.items))
	}
}

func TestNetworkCrawlPublishesWhatDiscoveryNeeds(t *testing.T) {
	t.Parallel()

	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		"a.oan.local": {catalogJSON("cat-1")},
	}}, out, network("a.oan.local", asSource))

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run() returned an unexpected error: %v", err)
	}

	got := out.items[0].entry
	if got.CatalogID != "cat-1" {
		t.Errorf("CatalogID = %q, want cat-1", got.CatalogID)
	}
	if got.CatalogType != catalogTypeRegular {
		t.Errorf("CatalogType = %q, want %s", got.CatalogType, catalogTypeRegular)
	}
	if len(got.NetworkIDs) != 1 || got.NetworkIDs[0] != ourNetwork {
		t.Errorf("NetworkIDs = %v, want only us -- nobody else may take our copy", got.NetworkIDs)
	}
	if len(got.SchemaTypes) != 0 {
		t.Errorf("SchemaTypes = %v, want none -- discovery reads @context off each resource", got.SchemaTypes)
	}
	if validityOf(t, out.items[0].content)["endDate"] == nil {
		t.Error("published with no endDate, so nothing would ever take the copy out again")
	}
}

func TestNetworkCrawlSkipsACatalogWithNoID(t *testing.T) {
	t.Parallel()

	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		"a.oan.local": {json.RawMessage(`{"descriptor":{"name":"nameless"}}`), catalogJSON("cat-1")},
	}}, out, network("a.oan.local", asSource))

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run() returned an unexpected error: %v", err)
	}
	if len(out.items) != 1 || out.items[0].entry.CatalogID != "cat-1" {
		t.Fatalf("expected only cat-1 queued, got %+v", out.items)
	}
}

func TestNetworkCrawlCarriesOnPastAFailingNetwork(t *testing.T) {
	t.Parallel()

	out := &stubSink{}
	crawl := newCrawl(failingAsker{fails: "a.oan.local", catalogs: []json.RawMessage{catalogJSON("cat-1")}}, out,
		network("a.oan.local", asSource),
		network("b.oan.local", asSource),
	)

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run() returned an unexpected error: %v", err)
	}
	if len(out.items) != 1 || out.items[0].entry.CatalogID != "cat-1" {
		t.Fatalf("expected b's catalog published, got %v", out.ids())
	}
}

type failingAsker struct {
	fails    string
	catalogs []json.RawMessage
}

func (a failingAsker) discover(_ context.Context, network model.AdmittedNetwork, _ string,
	each func([]json.RawMessage) error) error {
	if network.ID == a.fails {
		return errors.New("unreachable")
	}
	return each(a.catalogs)
}

func TestNetworkCrawlReportsARegistryFailure(t *testing.T) {
	t.Parallel()

	crawl := &networkCrawl{
		asker:        stubAsker{},
		sink:         &stubSink{},
		registry:     stubNetworkRegistry{err: errors.New("registry unreachable")},
		ourNetworkID: ourNetwork,
		log:          quietLog(),
	}

	if err := crawl.run(context.Background()); err == nil {
		t.Error("expected the registry failure to be reported, got none")
	}
}

type registryWithoutNetworks struct{ definition.RegistryLookup }

func TestNewNetworkCrawlNeedsARegistryThatListsNetworks(t *testing.T) {
	t.Parallel()

	_, err := newNetworkCrawlFrom(context.Background(), federationConfig{
		networkID: ourNetwork, signingKey: "c2VlZA==", keyID: "key-1",
	}, registryWithoutNetworks{}, &stubSink{}, quietLog())
	if err == nil {
		t.Error("expected a registry that cannot list networks to be refused, got none")
	}
}

type registryWithNetworks struct {
	definition.RegistryLookup
	stubNetworkRegistry
}

func TestNewNetworkCrawlBuildsFromConfig(t *testing.T) {
	t.Parallel()

	crawl, err := newNetworkCrawlFrom(context.Background(), federationConfig{
		networkID: ourNetwork, signingKey: "c2VlZA==", keyID: "key-1",
		intent: `{"filters":{}}`, refresh: time.Minute,
	}, registryWithNetworks{}, &stubSink{}, quietLog())
	if err != nil {
		t.Fatalf("newNetworkCrawlFrom() returned an unexpected error: %v", err)
	}
	if crawl.ourNetworkID != ourNetwork || crawl.intent != `{"filters":{}}` {
		t.Errorf("config was not carried through: %+v", crawl)
	}
	if crawl.schedule == nil {
		t.Error("a crawl riding the catalog ticker must decide when it is due")
	}
}

const (
	ourNetwork   = "bharatvistar.oan.local"
	theirNetwork = "mahavistara.oan.local"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type stubNetworkRegistry struct {
	networks []model.AdmittedNetwork
	err      error
}

func (s stubNetworkRegistry) AdmittedNetworks(context.Context) ([]model.AdmittedNetwork, error) {
	return s.networks, s.err
}

// The directions a fixture is admitted for. A pack list is required on a real
// record, so every direction here carries one.
const (
	asSource    = "source"
	asRequester = "requester"
	asBoth      = "both"
)

func network(id, agreed string) model.AdmittedNetwork {
	n := model.AdmittedNetwork{
		ID:            id,
		DiscoveryURL:  "https://" + id + "/network",
		ProjectionTTL: time.Hour,
	}
	packs := &model.FederationDirection{SchemaPacks: []string{weatherPack}}
	if agreed == asSource || agreed == asBoth {
		n.Source = packs
	}
	if agreed == asRequester || agreed == asBoth {
		n.Requester = packs
	}
	return n
}

func TestCrawlableNetworksKeepsOnlyWhatWeMayReadFrom(t *testing.T) {
	t.Parallel()

	registry := stubNetworkRegistry{networks: []model.AdmittedNetwork{
		network("source.oan.local", asSource),
		network("both.oan.local", asBoth),
		network("requester.oan.local", asRequester),
	}}

	networks, err := crawlableNetworks(context.Background(), registry, ourNetwork, quietLog())
	if err != nil {
		t.Fatalf("crawlableNetworks() returned an unexpected error: %v", err)
	}

	got := map[string]bool{}
	for _, p := range networks {
		got[p.ID] = true
	}
	if !got["source.oan.local"] || !got["both.oan.local"] {
		t.Errorf("expected source and both to be crawled, got %v", got)
	}
	if got["requester.oan.local"] {
		t.Error("a requester reads from us; we never read from it")
	}
}

func TestCrawlableNetworksDropsOurselves(t *testing.T) {
	t.Parallel()

	registry := stubNetworkRegistry{networks: []model.AdmittedNetwork{
		network(ourNetwork, asBoth),
		network("network.oan.local", asSource),
	}}

	networks, err := crawlableNetworks(context.Background(), registry, ourNetwork, quietLog())
	if err != nil {
		t.Fatalf("crawlableNetworks() returned an unexpected error: %v", err)
	}
	if len(networks) != 1 || networks[0].ID != "network.oan.local" {
		t.Fatalf("expected only the network, got %+v", networks)
	}
}

func TestCrawlableNetworksRefusesWithoutOurOwnID(t *testing.T) {
	t.Parallel()

	registry := stubNetworkRegistry{networks: []model.AdmittedNetwork{network("network.oan.local", asSource)}}

	if _, err := crawlableNetworks(context.Background(), registry, "", quietLog()); err == nil {
		t.Error("expected a refusal when we do not know our own id, got none")
	}
}

func TestCrawlableNetworksReportsARegistryFailure(t *testing.T) {
	t.Parallel()

	registry := stubNetworkRegistry{err: errors.New("registry unreachable")}

	if _, err := crawlableNetworks(context.Background(), registry, ourNetwork, quietLog()); err == nil {
		t.Error("expected the registry failure to be reported, got none")
	}
}

// Not parallel: otel.SetMeterProvider is global.
func TestRecordReportsDurationAndFailures(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		outcome      string
		expectErrors bool
	}{
		{name: "ok is a success", outcome: outcomeOK},
		{name: "unreachable counts as a failure", outcome: outcomeUnreachable, expectErrors: true},
		{name: "an unlisted outcome counts as a failure", outcome: "something_new", expectErrors: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := otel.GetMeterProvider()
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			otel.SetMeterProvider(mp)
			t.Cleanup(func() {
				otel.SetMeterProvider(previous)
				_ = mp.Shutdown(ctx)
			})

			recordCrawl(ctx, time.Now(), operationNetworkCrawl, tc.outcome)

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(ctx, &rm); err != nil {
				t.Fatalf("collect: %v", err)
			}
			if !recordedDuration(rm, operationNetworkCrawl) {
				t.Errorf("expected a duration for %q", operationNetworkCrawl)
			}
			if got := countedErrors(rm); got != tc.expectErrors {
				t.Errorf("counted an error = %v, want %v for outcome %q", got, tc.expectErrors, tc.outcome)
			}
		})
	}
}

func recordedDuration(rm metricdata.ResourceMetrics, operation string) bool {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, point := range hist.DataPoints {
				if v, found := point.Attributes.Value(telemetry.AttrOperation); found && v.AsString() == operation {
					return true
				}
			}
		}
	}
	return false
}

func countedErrors(rm metricdata.ResourceMetrics) bool {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
				return true
			}
		}
	}
	return false
}

func licensed(id string, ttl time.Duration) model.AdmittedNetwork {
	p := network(id, asSource)
	p.ProjectionTTL = ttl
	return p
}

func warnings(t *testing.T, refresh time.Duration, networks ...model.AdmittedNetwork) string {
	t.Helper()
	var out bytes.Buffer
	warnIfRefreshOutpacesTtl(networks, refresh,
		slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return out.String()
}

func TestARefreshWellInsideTheLicenceIsNotWarnedAbout(t *testing.T) {
	if got := warnings(t, time.Hour, licensed("n2.oan.local", 24*time.Hour)); got != "" {
		t.Errorf("warned about an ample refresh: %s", got)
	}
}

func TestARefreshEqualToTheLicenceIsWarnedAbout(t *testing.T) {
	got := warnings(t, 30*time.Second, licensed("n2.oan.local", 30*time.Second))
	if !strings.Contains(got, "n2.oan.local") {
		t.Errorf("want the network named, got: %s", got)
	}
	if !strings.Contains(got, "30s") {
		t.Errorf("want both durations reported, got: %s", got)
	}
}

func TestARefreshSlowerThanTheLicenceIsWarnedAbout(t *testing.T) {
	if got := warnings(t, time.Hour, licensed("n2.oan.local", time.Minute)); got == "" {
		t.Error("a refresh slower than the licence did not warn")
	}
}

func TestANetworkThatSetNoLicenceIsNotWarnedAbout(t *testing.T) {
	if got := warnings(t, time.Hour, licensed("n2.oan.local", 0)); got != "" {
		t.Errorf("warned about a network with no projectionTtl: %s", got)
	}
}

func TestEachNetworkIsJudgedOnItsOwnLicence(t *testing.T) {
	got := warnings(t, time.Hour,
		licensed("roomy.oan.local", 24*time.Hour),
		licensed("tight.oan.local", time.Minute))
	if strings.Contains(got, "roomy.oan.local") {
		t.Errorf("warned about the roomy network: %s", got)
	}
	if !strings.Contains(got, "tight.oan.local") {
		t.Errorf("did not warn about the tight network: %s", got)
	}
}

type sinkRefusingOne struct {
	stubSink
	refuse string
}

func (q *sinkRefusingOne) Send(ctx context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error) {
	if entry.CatalogID == q.refuse {
		return crawlmanager.SinkOutcome{Reason: "rejected"}, nil
	}
	return q.stubSink.Send(ctx, entry, content)
}

func TestACatalogDiscoveryRefusesDoesNotStopTheRest(t *testing.T) {
	out := &sinkRefusingOne{refuse: "catalog-a"}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		theirNetwork: {catalogJSON("catalog-a"), catalogJSON("catalog-b")},
	}}, out, network(theirNetwork, asSource))

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.items) != 1 || out.items[0].entry.CatalogID != "catalog-b" {
		t.Errorf("published %v, want catalog-b to survive catalog-a being refused", out.ids())
	}
}

func TestANetworkThatPermitsNoCopyIsNotCrawled(t *testing.T) {
	forbids := network(theirNetwork, asSource)
	forbids.ProjectionTTL = 0
	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		theirNetwork: {catalogJSON("catalog-a")},
	}}, out, forbids)

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.items) != 0 {
		t.Errorf("published %v from a network that permits no copy to be kept", out.ids())
	}
}

func TestANetworkWeAgreedNoSchemaPacksWithIsNotCrawled(t *testing.T) {
	nothing := network(theirNetwork, asSource)
	nothing.Source = &model.FederationDirection{}
	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		theirNetwork: {catalogJSON("catalog-a")},
	}}, out, nothing)

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.items) != 0 {
		t.Errorf("published %v from a network whose agreement covers nothing", out.ids())
	}
}

func TestACatalogThatIsNotJSONIsSkipped(t *testing.T) {
	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		theirNetwork: {json.RawMessage(`{"id":`), catalogJSON("catalog-b")},
	}}, out, network(theirNetwork, asSource))

	if err := crawl.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(out.items) != 1 || out.items[0].entry.CatalogID != "catalog-b" {
		t.Errorf("published %v, want only the catalog we could read", out.ids())
	}
}

func TestNewNetworkCrawlRefusesAConfigItCannotSignWith(t *testing.T) {
	_, err := newNetworkCrawlFrom(context.Background(), federationConfig{
		networkID: ourNetwork, keyID: "key-1",
	}, registryWithNetworks{}, &stubSink{}, quietLog())
	if err == nil {
		t.Error("a crawl with no signing key was built, and every call it makes would be refused")
	}
}

func TestATickBeforeItIsDueDoesNotCrawl(t *testing.T) {
	out := &stubSink{}
	crawl := newCrawl(stubAsker{byNetwork: map[string][]json.RawMessage{
		theirNetwork: {catalogJSON("catalog-a")},
	}}, out, network(theirNetwork, asSource))
	crawl.schedule = newDueEvery(time.Hour, time.Now)
	crawl.schedule.due()

	crawl.tick(context.Background())

	if len(out.items) != 0 {
		t.Errorf("crawled on a tick that was not due, queueing %v", out.items)
	}
}

func TestATickReportsAFailedPassWithoutPanicking(t *testing.T) {
	crawl := &networkCrawl{
		asker:        stubAsker{},
		sink:         &stubSink{},
		registry:     stubNetworkRegistry{err: errors.New("registry unreachable")},
		ourNetworkID: ourNetwork,
		log:          quietLog(),
	}

	crawl.tick(context.Background())
}

func TestASecondTickStandsDownWhileACrawlIsRunning(t *testing.T) {
	asker := &blockingAsker{started: make(chan struct{}), release: make(chan struct{})}
	crawl := newCrawl(asker, &stubSink{}, network(theirNetwork, asSource))

	done := make(chan struct{})
	go func() {
		defer close(done)
		crawl.tick(context.Background())
	}()
	<-asker.started

	second := make(chan struct{})
	go func() {
		defer close(second)
		crawl.tick(context.Background())
	}()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Error("a second tick did not stand down while a crawl was running")
	}

	close(asker.release)
	<-done
	<-second

	if got := asker.calls.Load(); got != 1 {
		t.Errorf("asked %d times, want 1 -- a second tick ran on top of the first", got)
	}
}
