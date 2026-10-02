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
}

// catalogPusher is the push half of the sink, narrowed to what a peer crawl
// needs so a test does not have to stand up an HTTP client.
type catalogPusher interface {
	Push(ctx context.Context, endpoint string, body []byte, sourceNetwork string) (sink.BatchOutcome, error)
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
	intent          map[string]any // configured: what breadth of catalog to mirror
	maxPages        int
	client          *http.Client
	push            catalogPusher
	pushEndpoint    string
	log             *slog.Logger
}

// discoverBody builds the Beckn discover sent to a peer.
//
// The intent is CONFIGURATION, not a constant: a deployment decides what breadth
// of catalog it wants to mirror -- a jsonpath filter, a spatial bound, or
// nothing at all for everything the peer will give us.
func (p *peerCrawl) discoverBody(page int) ([]byte, error) {
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
			"domain":    p.domain,
			"version":   p.protocolVersion,
			"networkId": p.localNetwork,
			"bapId":     p.subscriberID,
			"messageId": uuid.NewString(),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"message": map[string]any{
			"intent": intent,
			"page":   map[string]any{"number": page},
		},
	})
}

// fetchPage sends one signed discover and returns what the peer answered.
func (p *peerCrawl) fetchPage(ctx context.Context, target peerTarget, page int) ([]json.RawMessage, error) {
	body, err := p.discoverBody(page)
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

// crawlPeer drains one peer, page by page, and stores what it returns.
func (p *peerCrawl) crawlPeer(ctx context.Context, target peerTarget) error {
	var all []json.RawMessage
	for page := 1; page <= p.maxPages; page++ {
		got, err := p.fetchPage(ctx, target, page)
		if err != nil {
			return err
		}
		if len(got) == 0 {
			break
		}
		all = append(all, got...)
	}
	return p.pushAll(ctx, target, all)
}

// pushAll sends one peer's catalogs on, through the same sink our own crawled
// catalogs go through.
//
// UpdateModeFull, one catalog at a time: a peer that has withdrawn resources
// must not keep them alive in our cache by omitting them. VisibleTo is OUR
// network -- we cached this for our own consumers, and the origin filter is what
// stops it going further.
func (p *peerCrawl) pushAll(ctx context.Context, target peerTarget, catalogs []json.RawMessage) error {
	for _, document := range catalogs {
		meta := sink.PushMeta{
			ParticipantID: p.subscriberID,
			MessageID:     uuid.NewString(),
			TransactionID: uuid.NewString(),
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			UpdateMode:    sink.UpdateModeFull,
			CatalogType:   "regular",
			VisibleTo:     []string{p.localNetwork},
		}
		body, err := sink.BuildPushBody(meta, document)
		if err != nil {
			return fmt.Errorf("build push body for %s: %w", target.NetworkID, err)
		}
		// The origin, on every push. This is the whole reason the column exists:
		// it is what marks the row as another network's.
		if _, err := p.push.Push(ctx, p.pushEndpoint, body, target.NetworkID); err != nil {
			return fmt.Errorf("push %s: %w", target.NetworkID, err)
		}
	}
	return nil
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
}

// trimmedURL is the peer's endpoint as published, with a stray trailing slash
// removed so two records that differ only by it do not look like two peers.
func trimmedURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
