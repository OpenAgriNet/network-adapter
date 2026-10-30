package catalogcrawler

// networkcrawl.go — one pass over the networks we read from: pick them from
// the registry, ask each, and publish what came back to our own discovery.
//
// Catalog type: docs/adr/0001-crawled-catalogs-are-published-as-regular.md

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
	"go.opentelemetry.io/otel/metric"
)

const (
	// What a crawled catalog is published as. MASTER is for a catalog that
	// defines others, which a network never shares.
	catalogTypeRegular = "REGULAR"

	pluginID   = "catalogcrawler"
	pluginType = "crawler"

	operationNetworkAsk   = "network_ask"   // one discover to one network
	operationNetworkCrawl = "network_crawl" // a pass over every network
	operationCrawlPublish = "crawl_publish" // publishing what a network returned

	outcomeOK          = "ok"
	outcomeUnreachable = "unreachable"  // the network could not be reached
	outcomeRefused     = "refused"      // the network answered, but not with catalogs
	outcomeUnreadable  = "unreadable"   // the network answered something we cannot parse
	outcomeNotSent     = "not_sent"     // we never managed to ask
	outcomeRegistryErr = "registry_err" // we could not find out who to crawl
	outcomePublishErr  = "publish_err"
)

type networkAsker interface {
	discover(ctx context.Context, network model.AdmittedNetwork, intent string,
		each func(page []json.RawMessage) error) error
}

// catalogSink is where a crawled catalog goes. Published as it is read; no
// queue, because a pass that misses one simply fetches it again.
type catalogSink interface {
	Send(ctx context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error)
}

type networkCrawl struct {
	asker        networkAsker
	sink         catalogSink
	registry     networkRegistry
	ourNetworkID string
	intent       string
	schedule     *dueEvery
	log          *slog.Logger

	// inFlight keeps one pass at a time. A pass can outlast the tick that
	// starts it, and it is what makes the schedule visible to the next tick.
	inFlight sync.Mutex
	running  bool
}

// run asks every network we read from and publishes what came back. A network that
// fails is logged and skipped: one unreachable network must not stop the rest.
func (c *networkCrawl) run(ctx context.Context) error {
	start := time.Now()
	networks, err := crawlableNetworks(ctx, c.registry, c.ourNetworkID, c.log)
	if err != nil {
		recordCrawl(ctx, start, operationNetworkCrawl, outcomeRegistryErr)
		return err
	}

	warnIfRefreshOutpacesTtl(networks, c.schedule.every(), c.log)

	for _, network := range networks {
		fetchedAt := time.Now()
		if network.ProjectionTTL <= 0 {
			c.log.ErrorContext(ctx, "catalogcrawler: a network permits no copy to be kept, not crawled",
				"networkId", network.ID)
			continue
		}
		if len(network.SourceContexts()) == 0 {
			c.log.ErrorContext(ctx, "catalogcrawler: a network's source agreement names no schema pack, not crawled",
				"networkId", network.ID)
			continue
		}
		err := c.asker.discover(ctx, network, c.intent, func(page []json.RawMessage) error {
			c.publishPage(ctx, network, fetchedAt, page)
			return nil
		})
		if err != nil {
			c.log.ErrorContext(ctx, "catalogcrawler: asking a network failed",
				"networkId", network.ID, "error", err)
		}
	}
	recordCrawl(ctx, start, operationNetworkCrawl, outcomeOK)
	return nil
}

// publishPage sends one page on to our own discovery, each copy stamped with
// when it stops being served.
func (c *networkCrawl) publishPage(ctx context.Context, network model.AdmittedNetwork,
	fetchedAt time.Time, catalogs []json.RawMessage) {
	expires := fetchedAt.Add(network.ProjectionTTL)
	published := 0

	for _, document := range catalogs {
		id := catalogIDOf(document)
		if id == "" {
			c.log.WarnContext(ctx, "catalogcrawler: catalog has no id, not published",
				"networkId", network.ID)
			continue
		}
		stamped, err := withExpiry(document, expires)
		if err != nil {
			c.log.ErrorContext(ctx, "catalogcrawler: a crawled catalog could not be stamped",
				"networkId", network.ID, "catalogId", id, "error", err)
			continue
		}
		if c.publishOne(ctx, network, id, stamped) {
			published++
		}
	}
	c.log.InfoContext(ctx, "catalogcrawler: network crawled",
		"networkId", network.ID, "published", published, "returned", len(catalogs),
		"servedUntil", expires)
}

// publishOne publishes one copy and reports whether discovery took it. Ours alone:
// nobody else may take our copy of another network's catalog out of us.
func (c *networkCrawl) publishOne(ctx context.Context, network model.AdmittedNetwork,
	id string, document json.RawMessage) bool {
	at := time.Now()
	outcome, err := c.sink.Send(ctx, catalog.CatalogEntry{
		CatalogID:   id,
		CatalogType: catalogTypeRegular,
		NetworkIDs:  []string{c.ourNetworkID},
	}, document)

	switch {
	case err != nil:
		c.log.ErrorContext(ctx, "catalogcrawler: publishing a crawled catalog failed",
			"networkId", network.ID, "catalogId", id, "error", err)
	case !outcome.Accepted:
		c.log.ErrorContext(ctx, "catalogcrawler: discovery refused a crawled catalog",
			"networkId", network.ID, "catalogId", id, "reason", outcome.Reason)
	default:
		recordCrawl(ctx, at, operationCrawlPublish, outcomeOK)
		return true
	}
	recordCrawl(ctx, at, operationCrawlPublish, outcomePublishErr)
	return false
}

func catalogIDOf(document json.RawMessage) string {
	var named struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(document, &named); err != nil {
		return ""
	}
	return named.ID
}

// newNetworkCrawlFrom builds the pass. A registry that cannot list networks is
// refused here, rather than crawling nobody in silence.
func newNetworkCrawlFrom(ctx context.Context, cfg federationConfig, registry definition.RegistryLookup,
	sink catalogSink, log *slog.Logger) (*networkCrawl, error) {
	networks, ok := registry.(networkRegistry)
	if !ok {
		return nil, fmt.Errorf("catalogcrawler: %q is true but the registry plugin cannot list networks", cfgFederationEnabled)
	}

	caller, err := newNetworkCaller(ctx, networkCallerConfig{
		networkID:  cfg.networkID,
		signingKey: cfg.signingKey,
		keyID:      cfg.keyID,
		timeout:    cfg.timeout,
		pageSize:   cfg.pageSize,
	}, log)
	if err != nil {
		return nil, err
	}

	return &networkCrawl{
		asker:        caller,
		sink:         sink,
		registry:     networks,
		ourNetworkID: cfg.networkID,
		intent:       cfg.intent,
		schedule:     newDueEvery(cfg.refresh, time.Now),
		log:          log,
	}, nil
}

// tick runs a pass when one is due. Errors are logged rather than returned:
// the scheduler has nowhere to report them, and the next tick tries again.
func (c *networkCrawl) tick(ctx context.Context) {
	mine, busy := c.claim()
	if !mine {
		if busy {
			c.log.InfoContext(ctx, "catalogcrawler: a network crawl is still running, skipping this tick")
		}
		return
	}
	defer c.release()

	if err := c.run(ctx); err != nil {
		c.log.ErrorContext(ctx, "catalogcrawler: network crawl failed", "error", err)
	}
}

// claim reports whether this tick may run, and whether a pass already in flight
// is what turned it away. The schedule is read here so only one tick touches it.
func (c *networkCrawl) claim() (mine, busy bool) {
	c.inFlight.Lock()
	defer c.inFlight.Unlock()
	if c.running {
		return false, true
	}
	if c.schedule != nil && !c.schedule.due() {
		return false, false
	}
	c.running = true
	return true, false
}

func (c *networkCrawl) release() {
	c.inFlight.Lock()
	c.running = false
	c.inFlight.Unlock()
}

// networkRegistry is what we need from a registry plugin here.
type networkRegistry interface {
	AdmittedNetworks(ctx context.Context) ([]model.AdmittedNetwork, error)
}

// crawlableNetworks returns the networks we agreed to read from, without our own.
// Our entry looks like any network, so only the id separates it.
func crawlableNetworks(ctx context.Context, registry networkRegistry, ourNetworkID string, log *slog.Logger) ([]model.AdmittedNetwork, error) {
	if strings.TrimSpace(ourNetworkID) == "" {
		return nil, fmt.Errorf("catalogcrawler: this deployment's own network id is required to crawl networks")
	}

	admitted, err := registry.AdmittedNetworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("catalogcrawler: listing networks: %w", err)
	}

	networks := make([]model.AdmittedNetwork, 0, len(admitted))
	for _, network := range admitted {
		switch {
		case strings.EqualFold(network.ID, ourNetworkID):
			log.InfoContext(ctx, "catalogcrawler: skipping our own entry", "networkId", network.ID)
		case !network.IsSource():
			log.InfoContext(ctx, "catalogcrawler: not a network we read from",
				"networkId", network.ID, "agreed", network.Directions())
		default:
			log.InfoContext(ctx, "catalogcrawler: network to crawl",
				"networkId", network.ID, "packs", network.SourcePacks(),
				"discoveryUrl", network.DiscoveryURL, "projectionTtl", network.ProjectionTTL)
			networks = append(networks, network)
		}
	}
	log.InfoContext(ctx, "catalogcrawler: networks resolved",
		"crawlable", len(networks), "admitted", len(admitted))
	return networks, nil
}

// warnIfRefreshOutpacesTtl reports a network whose copy expires before the
// crawl that would renew it, so its catalogs come and go rather than stay.
func warnIfRefreshOutpacesTtl(networks []model.AdmittedNetwork, refresh time.Duration, log *slog.Logger) {
	for _, network := range networks {
		if network.ProjectionTTL > 0 && refresh >= network.ProjectionTTL {
			log.Warn("catalogcrawler: a network is asked no more often than it lets us keep a copy",
				"networkId", network.ID, "refresh", refresh, "projectionTtl", network.ProjectionTTL)
		}
	}
}

// recordCrawl emits the shared duration and error counters. Anything but
// outcomeOK counts as a failure, so a new outcome cannot pass as healthy.
func recordCrawl(ctx context.Context, start time.Time, operation, outcome string) {
	m, err := telemetry.GetMetrics(ctx)
	if err != nil {
		return
	}

	attrs := metric.WithAttributes(
		telemetry.AttrPluginID.String(pluginID),
		telemetry.AttrPluginType.String(pluginType),
		telemetry.AttrOperation.String(operation),
		telemetry.AttrErrorType.String(outcome),
	)

	m.PluginExecutionDuration.Record(ctx, time.Since(start).Seconds(), attrs)
	if outcome != outcomeOK {
		m.PluginErrorsTotal.Add(ctx, 1, attrs)
	}
}
