package catalogcrawler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
	"github.com/google/uuid"
)

// Crawling a peer is not the same shape as crawling a provider.
//
// A provider publishes a catalog INDEX, and the engine fetches it, notices what
// changed and stores it. A peer network publishes no such index: it is reached
// the way any consumer reaches it, by POSTing a Beckn discover and keeping the
// answer. That is why this is not another crawlmanager.Source -- a Source only
// names URLs to GET, and there is nowhere in it to put a signed POST body.
//
// What is reused: the scheduler, the sink, and the signer. What is not: the
// index fetch and its change detection, because there is no index.

// peerTarget is one network to crawl: who it is, and the discovery endpoint it
// published in its descriptor.
//
// A discovery URL, not a base URL. The peer declares a complete endpoint and we
// use it verbatim; appending a path to it would be this adapter deciding the
// peer's URL shape, which is the peer's to declare.
type peerTarget struct {
	NetworkID    string
	DiscoveryURL string

	// ProjectionTtl is how long this peer permits us to keep what it returns.
	// The peer's declaration, read off its registry record -- not a local
	// setting, because it is not ours to decide.
	//
	// Zero means a record written before the field existed, not "forever": the
	// crawl falls back to the configured interval and the projection still
	// expires, it is just not paced by the peer.
	ProjectionTtl time.Duration
}

// catalogPusher is the push half of the sink, narrowed to what a peer crawl
// needs so a test does not have to stand up an HTTP client.
type catalogPusher interface {
	Push(ctx context.Context, endpoint string, body []byte) (sink.BatchOutcome, error)
}

// requestSigner is the Signer plugin, narrowed the same way.
type requestSigner interface {
	Sign(ctx context.Context, body []byte, privateKeyBase64 string, createdAt, expiresAt int64) (string, error)
}

// peerCrawl holds what crawling peers needs. Built once at start-up and reused
// for every pass, like the rest of the crawler's dependencies.
//
// It owns no database. The crawler's own store tracks index state for OUR
// providers; a peer has no index, so there is nothing of that kind to track.
// What a peer returns goes where every other catalog goes -- to
// discovery-service, through the sink.
type peerCrawl struct {
	signer          requestSigner
	localNetwork    string // our networkId: declared to the peer, and signed over
	subscriberID    string // who we sign as
	privateKey      string // the operational key, never the governance key
	window          time.Duration
	domain          string // the Beckn domain a peer routes on
	protocolVersion string

	// schemaContext is what we are willing to mirror, by schema.
	//
	// It is also what makes a jsonpath intent usable at all: a filter that
	// narrows nothing is refused, and schemaContext is one of the three things
	// that may narrow it first. Empty is valid, but then the intent has to
	// narrow on its own.
	schemaContext []string
	intent        map[string]any // configured: what breadth of catalog to mirror
	maxPages      int
	client        *http.Client
	push          catalogPusher
	pushEndpoint  string
	log           *slog.Logger

	// projections records what we are holding from each peer, so it can be
	// withdrawn later. Nil disables every purge path: a deployment whose store
	// is unavailable keeps crawling rather than silently losing the ability to
	// expire, and says so once at start-up.
	projections projectionStore
}

// discoverBody builds the Beckn discover sent to a peer.
//
// The intent is CONFIGURATION, not a constant: a deployment decides what breadth
// of catalog it wants to mirror -- a jsonpath filter, a spatial bound, or
// nothing at all for everything the peer will give us.
func (p *peerCrawl) discoverBody() ([]byte, error) {
	intent := p.intent
	if intent == nil {
		intent = map[string]any{}
	}
	return json.Marshal(map[string]any{
		// Our OWN network id, not the peer's. It tells the peer who is asking,
		// and its signature validation checks this against the keyId -- the two
		// disagreeing is what checkIdentity already refuses.
		"context": map[string]any{
			"action": "discover",
			// The peer routes on domain and version, so a discover missing
			// either is refused before it reaches its discovery service -- with
			// "no routing rules found for domain", which names the field and
			// not the caller.
			"domain":        p.domain,
			"version":       p.protocolVersion,
			"networkId":     p.localNetwork,
			"bapId":         p.subscriberID,
			"messageId":     uuid.NewString(),
			"schemaContext": p.schemaContext,
			// One transaction per PAGE request, matching messageId. A crawl is
			// not a conversation with the peer -- each page stands alone -- and
			// a shared transactionId would claim a continuity that does not
			// exist. Required, and refused with CTX_MISSING_FIELD if absent.
			"transactionId": uuid.NewString(),
			"timestamp":     time.Now().UTC().Format(time.RFC3339),
		},
		// No page. The Beckn discover schema has no paging member -- sending one
		// is refused with "property \"page\" is unsupported at $.message" -- so a
		// crawl asks ONCE and takes whatever the peer chooses to return.
		//
		// That is a real limitation, not a simplification: a peer with more
		// catalogs than it returns in one response is partially mirrored, and
		// nothing here can tell that it was. Paging across networks needs a
		// protocol answer, not a client-side one.
		"message": map[string]any{
			"intent": intent,
		},
	})
}

// fetchPage sends one signed discover and returns what the peer answered.
func (p *peerCrawl) fetch(ctx context.Context, target peerTarget) ([]json.RawMessage, error) {
	body, err := p.discoverBody()
	if err != nil {
		return nil, fmt.Errorf("build discover for %s: %w", target.NetworkID, err)
	}

	// The same Signer every other outbound call uses. There is no
	// federation-specific signature: a cross-network discover is an ordinary
	// signed Beckn call whose sender happens to be a network.
	now := time.Now()
	signature, err := p.signer.Sign(ctx, body, p.privateKey, now.Unix(), now.Add(p.window).Unix())
	if err != nil {
		return nil, fmt.Errorf("sign discover for %s: %w", target.NetworkID, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.DiscoveryURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", signature)

	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("discover %s: %w", target.NetworkID, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover %s: %s", target.NetworkID, response.Status)
	}

	// Each catalog is kept as raw JSON and pushed exactly as the peer published
	// it. Decoding into a struct of our own would silently drop every field this
	// adapter does not happen to model -- and what a peer may publish is
	// governed by the schema packs it declared, not by our idea of a catalog.
	var answer struct {
		Message struct {
			Catalogs []json.RawMessage `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("decode %s: %w", target.NetworkID, err)
	}
	return answer.Message.Catalogs, nil
}

// crawlPeer asks one peer, stores what it returns, and withdraws what it has
// stopped returning.
//
// The withdrawal half only runs after a SUCCESSFUL fetch. A peer that could not
// be reached has told us nothing about what it still publishes, and treating
// silence as a withdrawal would empty the cache on one timeout -- which is what
// the ttl, and only the ttl, is allowed to do.
func (p *peerCrawl) crawlPeer(ctx context.Context, target peerTarget) error {
	catalogs, err := p.fetch(ctx, target)
	if err != nil {
		return err
	}
	if err := p.pushAll(ctx, target, catalogs); err != nil {
		return err
	}
	return p.reconcile(ctx, target, catalogs)
}

// reconcile records this pass's projections and withdraws the ones the peer
// has dropped.
//
// Ordering: the fresh ids are recorded BEFORE the dropped ones are withdrawn.
// The reverse order would, if the process died in between, leave a catalog
// published to discovery with no row naming it -- unpurgeable for ever.
func (p *peerCrawl) reconcile(ctx context.Context, target peerTarget, catalogs []json.RawMessage) error {
	if p.projections == nil {
		return nil
	}

	fresh, unnamed := catalogIDsOf(catalogs)
	if unnamed > 0 {
		p.log.WarnContext(ctx, "catalogcrawler: peer returned catalogs with no id; they cannot be expired later",
			"networkId", target.NetworkID, "count", unnamed)
	}

	held, err := p.projections.ProjectionsFor(ctx, target.NetworkID)
	if err != nil {
		return fmt.Errorf("list projections of %s: %w", target.NetworkID, err)
	}

	expiresAt := time.Now().Add(p.ttlFor(target))
	if err := p.projections.RecordProjections(ctx, target.NetworkID, fresh, expiresAt); err != nil {
		return fmt.Errorf("record projections of %s: %w", target.NetworkID, err)
	}

	p.purgeAll(ctx, target.NetworkID, missingFrom(held, fresh), "peer no longer publishes it")
	return nil
}

// ttlFor is the licence to apply to what this peer just returned.
//
// A peer whose record carries no ttl still gets a finite one. There is no state
// in which a projection has no expiry: that is the whole obligation.
func (p *peerCrawl) ttlFor(target peerTarget) time.Duration {
	if target.ProjectionTtl > 0 {
		return target.ProjectionTtl
	}
	return defaultProjectionTtl
}

// defaultProjectionTtl covers a registry record written before projectionTtl
// existed. Deliberately short: an unknown licence is not a long one.
const defaultProjectionTtl = time.Hour

// pushAll sends one peer's catalogs on, through the same sink our own crawled
// catalogs go through.
//
// UpdateModeFull, one catalog at a time: a peer that has withdrawn resources
// must not keep them alive in our cache by omitting them.
//
// VisibleTo is OUR network, and that single value is what stops a peer's
// catalogue being re-exported: we cached it for our own consumers, so a third
// network asking us matches nothing. It is a Beckn field doing the work, which
// is why this needs no origin column of its own.
func (p *peerCrawl) pushAll(ctx context.Context, target peerTarget, catalogs []json.RawMessage) error {
	for _, document := range catalogs {
		meta := sink.PushMeta{
			ParticipantID: p.subscriberID,
			MessageID:     uuid.NewString(),
			TransactionID: uuid.NewString(),
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			// discovery-service's /publish checks the body's action against the
			// route, so this must say publish and not push.
			Action:     "catalog/publish",
			UpdateMode: sink.UpdateModeFull,
			// Upper case: the enum is ["MASTER","REGULAR"] and the comparison is
			// exact. A crawled catalog is never a master -- a master is a
			// deployment's own shared definition, not something mirrored.
			CatalogType: "REGULAR",
			VisibleTo:   []string{p.localNetwork},
		}
		body, err := sink.BuildPushBody(meta, document)
		if err != nil {
			return fmt.Errorf("build push body for %s: %w", target.NetworkID, err)
		}
		if _, err := p.push.Push(ctx, p.pushEndpoint, body); err != nil {
			return fmt.Errorf("push %s: %w", target.NetworkID, err)
		}
	}
	return nil
}

// refreshEvery re-crawls every peer on an interval until ctx is cancelled.
//
// Without this a crawled catalogue is frozen at whatever the peer said once:
// a resource it has since withdrawn keeps being served by us, which is the one
// thing a cache must not do. The interval is how stale we are willing to be.
//
// The first pass runs immediately, so a restart does not leave the cache empty
// for a whole interval.
func (p *peerCrawl) refreshEvery(ctx context.Context, configured time.Duration, targets func(context.Context) ([]peerTarget, error)) {
	// A TIMER re-armed each pass, not a fixed ticker. The interval depends on
	// what the peers declared, and the peers are only known once a pass has
	// listed them -- a peer admitted later with a shorter ttl has to be able to
	// speed the loop up, or its data lapses between passes.
	run := func() time.Duration {
		found, err := targets(ctx)
		if err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: listing peers for refresh", "error", err)
			// Still a pass: expiry does not depend on the registry answering,
			// and a registry outage must not suspend every licence.
			p.crawlAll(ctx, nil, "refresh")
			return configured
		}
		p.crawlAll(ctx, found, "refresh")
		// Only on a listing that SUCCEEDED. found is authoritative here --
		// empty means every peer really was suspended, and purging everything
		// is then correct. On the error path above it means nothing of the
		// kind, which is why this is not inside crawlAll.
		if p.projections != nil {
			p.purgeUnadmitted(ctx, found)
		}
		return refreshInterval(configured, found)
	}

	next := run()

	timer := time.NewTimer(next)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(run())
		}
	}
}

// crawlAll visits every peer in one pass.
//
// A failure is logged and skipped, never returned. One network being briefly
// unreachable is routine and must not leave every other peer's cache stale --
// which is the whole of what "failures remain peer-scoped" means in practice.
// It is a function of its own, and not a closure inside CrawlPeers, so this can
// be tested without a scheduler.
func (p *peerCrawl) crawlAll(ctx context.Context, targets []peerTarget, runID string) {
	for _, target := range targets {
		if err := p.crawlPeer(ctx, target); err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: peer crawl failed",
				"runId", runID, "networkId", target.NetworkID, "error", err)
		}
	}
	if p.projections == nil {
		return
	}
	// Expiry runs AFTER the crawl and regardless of how it went. It is not
	// about any one peer -- it acts precisely on the peers that answered
	// nothing -- so skipping it when a crawl failed would let a bad pass
	// suspend every licence.
	//
	// Suspension is NOT swept here. It is driven by the admitted list, and
	// crawlAll cannot tell an empty list ("every peer was suspended", purge
	// everything) from an absent one ("the registry did not answer", purge
	// nothing). Acting on that distinction belongs where the error is still in
	// hand: refreshEvery.
	p.purgeExpired(ctx, time.Now())
}

// trimmedURL is the peer's endpoint as published, with a stray trailing slash
// removed so two records that differ only by it do not look like two peers.
func trimmedURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
