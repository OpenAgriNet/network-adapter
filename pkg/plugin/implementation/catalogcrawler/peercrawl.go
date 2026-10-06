package catalogcrawler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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

	// SchemaPacks is what this peer declared it can speak, verbatim from its
	// descriptor. Pack URLs -- each names a pack's attributes.yaml -- while a
	// discover filters on the matching context.jsonld, so they are translated
	// at crawl time rather than stored rewritten.
	//
	// Empty means the peer declared none, or the record predates the field. In
	// both cases the crawl falls back to its configured schemaContext, which is
	// what it used before a peer could say anything about this.
	SchemaPacks []string
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
	signer requestSigner
	// localNetwork is our OWN networkId. It is not sent to a peer and not
	// stamped on what we store -- a crawl names the peer in both places. It is
	// kept so the crawler can drop ITSELF from the admitted-peer list, which
	// carries role "network" for our own network-layer adapter too.
	localNetwork    string
	subscriberID    string // who we sign as
	privateKey      string // the network's signing key
	keyID           string // the id the registry filed that key under
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
	// pageSize is the `limit` query parameter. 0 asks once with no limit.
	//
	// It counts RESOURCES, not catalogs -- discovery ranks resources, pages
	// that list, then groups the page into catalogs. So one catalog's resources
	// can straddle a page boundary, which is why every page is accumulated
	// before anything is pushed.
	pageSize int
	// updateMode is how a projection is published: FULL replaces, MERGE upserts.
	updateMode string
	// batchSize is how many catalogs are held before they are published.
	//
	// Only read under MERGE. FULL cannot publish in batches at all -- every
	// push claims to be the catalog's complete set, so a second one deletes
	// what the first wrote -- so it accumulates the whole peer regardless.
	batchSize    int
	client       *http.Client
	push         catalogPusher
	pushEndpoint string
	log          *slog.Logger

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
func (p *peerCrawl) discoverBody(audience string, schemaContext []string) ([]byte, error) {
	intent := p.intent
	if intent == nil {
		intent = map[string]any{}
	}
	return json.Marshal(map[string]any{
		// The AUDIENCE this call asks for. visibleTo is an audience list and
		// context.networkId can only carry ONE value, so a crawl that wants two
		// audiences has to ask twice -- see crawlPeer.
		//
		// Whatever is asked for here is also what the answer is stamped with,
		// because that is the only thing the answer is known to be addressed
		// to. The discover response carries no visibleTo of its own.
		"context": map[string]any{
			"action": "discover",
			// The peer routes on domain and version, so a discover missing
			// either is refused before it reaches its discovery service -- with
			// "no routing rules found for domain", which names the field and
			// not the caller.
			"domain":        p.domain,
			"version":       p.protocolVersion,
			"networkId":     audience,
			"bapId":         p.subscriberID,
			"messageId":     uuid.NewString(),
			"schemaContext": schemaContext,
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

// crawlAudience asks a peer for one audience and publishes what comes back,
// returning the catalog ids it saw.
//
// Only IDS are returned. The documents are released as they are published, so
// what this holds does not grow with the size of the peer -- which is the whole
// reason it publishes here rather than handing a list back.
//
// Under MERGE it publishes in batches: an id-keyed upsert means a catalog split
// across pages reassembles correctly, so a page can be written and dropped.
//
// Under FULL it cannot. Every FULL push claims to be the catalog's complete
// current set, so publishing page 2 deletes what page 1 wrote -- a
// 1000-resource catalog fetched in two pages of 500 would end up holding 500.
// So FULL accumulates the whole peer before publishing anything, and pays the
// memory for it.
func (p *peerCrawl) crawlAudience(ctx context.Context, target peerTarget, audience string) ([]string, error) {
	batch := newPushBatch(p, audience, p.batchFor())

	if p.pageSize <= 0 {
		// One request, no limit: what this did before paging, kept so a
		// deployment that has not configured a page size is unaffected.
		got, err := p.fetchPage(ctx, target, audience, 0, 0)
		if err != nil {
			return nil, err
		}
		if err := batch.add(ctx, got); err != nil {
			return nil, err
		}
		return batch.done(ctx)
	}

	for page := 0; page < p.maxPages; page++ {
		got, err := p.fetchPage(ctx, target, audience, p.pageSize, page*p.pageSize)
		if err != nil {
			return nil, err
		}
		if err := batch.add(ctx, got); err != nil {
			return nil, err
		}
		// A page the peer could not fill is the last one. Discovery pages a
		// RESOURCE list, so a short page in CATALOGS is not a reliable end
		// marker -- but an empty one is.
		if len(got) == 0 {
			break
		}
	}
	return batch.done(ctx)
}

// batchFor is how many catalogs may be held before they are published.
//
// Zero means "hold everything", which is what FULL requires and what a
// misconfigured batch size should not silently become.
func (p *peerCrawl) batchFor() int {
	if p.updateMode == sink.UpdateModeFull {
		return 0
	}
	if p.batchSize < 1 {
		return 1
	}
	return p.batchSize
}

// pushBatch collects catalogs and publishes them once there are enough.
//
// size 0 holds everything until done, which is FULL's only correct shape.
type pushBatch struct {
	crawl    *peerCrawl
	audience string
	size     int
	held     []json.RawMessage
	ids      []string
}

func newPushBatch(crawl *peerCrawl, audience string, size int) *pushBatch {
	return &pushBatch{crawl: crawl, audience: audience, size: size}
}

func (b *pushBatch) add(ctx context.Context, catalogs []json.RawMessage) error {
	b.held = append(b.held, catalogs...)
	if b.size > 0 && len(b.held) >= b.size {
		return b.flush(ctx)
	}
	return nil
}

func (b *pushBatch) done(ctx context.Context) ([]string, error) {
	if err := b.flush(ctx); err != nil {
		return nil, err
	}
	return b.ids, nil
}

// flush publishes what is held and releases it, keeping only the ids.
func (b *pushBatch) flush(ctx context.Context) error {
	if len(b.held) == 0 {
		return nil
	}
	if err := b.crawl.pushAll(ctx, b.audience, b.held); err != nil {
		return err
	}
	fresh, unnamed := catalogIDsOf(b.held)
	if unnamed > 0 {
		b.crawl.log.WarnContext(ctx,
			"catalogcrawler: peer returned catalogs with no id; they cannot be expired later",
			"audience", b.audience, "count", unnamed)
	}
	b.ids = append(b.ids, fresh...)
	b.held = nil
	return nil
}

// fetchPage sends one signed discover and returns what the peer answered.
func (p *peerCrawl) fetchPage(ctx context.Context, target peerTarget, audience string, limit, offset int) ([]json.RawMessage, error) {
	body, err := p.discoverBody(audience, p.schemaContextsFor(target))
	if err != nil {
		return nil, fmt.Errorf("build discover for %s: %w", target.NetworkID, err)
	}

	// Paging lives on the QUERY STRING, not in the intent: the Beckn discover
	// schema has no paging member and refuses one in the body.
	endpoint := target.DiscoveryURL
	if limit > 0 {
		endpoint = withPage(endpoint, limit, offset)
	}

	// The same Signer every other outbound call uses. There is no
	// federation-specific signature: a cross-network discover is an ordinary
	// signed Beckn call whose sender happens to be a network.
	now := time.Now()
	created, expires := now.Unix(), now.Add(p.window).Unix()
	signature, err := p.signer.Sign(ctx, body, p.privateKey, created, expires)
	if err != nil {
		return nil, fmt.Errorf("sign discover for %s: %w", target.NetworkID, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", authorization(p.subscriberID, p.keyID, created, expires, signature))

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

// authorization builds the header a signed Beckn call carries.
//
// The Signer plugin returns the signature and nothing else -- deliberately, it
// does one cryptographic job -- so the envelope around it is the caller's to
// build. Every signed request in this system carries the same shape, and this
// is the one place outside the handler's sign step that has to produce it,
// because a crawl is a background pass rather than a request moving through a
// module.
//
// keyId is "<who>|<which key>|<algorithm>". Both halves matter: WHO lets a peer
// know who is asking, and WHICH KEY lets it pick the right one from among what
// we published. Sending the bare signature -- which this did until now -- left
// a peer holding something it could not attribute to anybody, and so could not
// verify even in principle.
func authorization(subscriberID, keyID string, created, expires int64, signature string) string {
	return fmt.Sprintf(
		`Signature keyId="%s|%s|ed25519",algorithm="ed25519",created="%d",expires="%d",`+
			`headers="(created) (expires) digest",signature="%s"`,
		subscriberID, keyID, created, expires, signature)
}

// crawlPeer asks one peer, stores what it returns, and withdraws what it has
// stopped returning.
//
// The withdrawal half only runs after a SUCCESSFUL fetch. A peer that could not
// be reached has told us nothing about what it still publishes, and treating
// silence as a withdrawal would empty the cache on one timeout -- which is what
// the ttl, and only the ttl, is allowed to do.
func (p *peerCrawl) crawlPeer(ctx context.Context, target peerTarget) error {
	// IDS, not documents: each audience publishes and releases what it fetched,
	// so this does not grow with the size of the peer.
	var seen []string

	// TWO calls, because context.networkId carries one value and we want two
	// audiences:
	//
	//   the peer's own id  -- what the peer owns. A catalog published with no
	//                         declared audience is filled with its own network,
	//                         so this is how its own catalogue is reached.
	//   our id             -- what the peer's publishers chose to share WITH US
	//                         by naming us in publishDirectives.visibleTo.
	//
	// Ordered deliberately. A catalog in BOTH answers is pushed twice, and the
	// second push decides what it is stamped with. Ours going last means our
	// copy ends up addressed to us -- which is what stops the peer's own crawl
	// of US matching it and re-importing its own catalogs.
	//
	// Both collapse into one call once context.networkId can carry a list.
	for _, audience := range []string{target.NetworkID, p.subscriberID} {
		ids, err := p.crawlAudience(ctx, target, audience)
		if err != nil {
			return err
		}
		seen = append(seen, ids...)
	}

	return p.reconcile(ctx, target, seen)
}

// reconcile records this pass's projections and withdraws the ones the peer
// has dropped.
//
// Ordering: the fresh ids are recorded BEFORE the dropped ones are withdrawn.
// The reverse order would, if the process died in between, leave a catalog
// published to discovery with no row naming it -- unpurgeable for ever.
func (p *peerCrawl) reconcile(ctx context.Context, target peerTarget, fresh []string) error {
	if p.projections == nil {
		return nil
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
// VisibleTo is the SOURCE network, so a crawled catalog stays labelled as whose
// it is. We hold a copy; we do not become its owner.
//
// Stamping our OWN id here would say "this is visible to us" about a fact that
// is really "this belongs to Maha" -- one field carrying two unrelated things,
// and overwriting the one it is named for. It would also make a third network
// asking us for OUR catalogs unable to tell a copy from an original.
//
// Keeping the owner is what makes "answer with your own data only" true by
// construction: a peer asks us for a specific owner, so copies of a third
// network's data cannot match. It is a Beckn field doing the work, which is why
// this needs no origin column of its own.
//
// A local search carries no network id at all, so it is unaffected: it still
// sees everything we hold, ours and crawled alike.
func (p *peerCrawl) pushAll(ctx context.Context, audience string, catalogs []json.RawMessage) error {
	for _, document := range catalogs {
		meta := sink.PushMeta{
			ParticipantID: p.subscriberID,
			MessageID:     uuid.NewString(),
			TransactionID: uuid.NewString(),
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			// discovery-service's /publish checks the body's action against the
			// route, so this must say publish and not push.
			Action: "catalog/publish",
			// Configured. FULL replaces, so a resource the peer dropped is
			// deleted here too; MERGE upserts by id and never notices one.
			UpdateMode: p.updateMode,
			// Upper case: the enum is ["MASTER","REGULAR"] and the comparison is
			// exact. A crawled catalog is never a master -- a master is a
			// deployment's own shared definition, not something mirrored.
			CatalogType: "REGULAR",
			// Whatever this call ASKED for. The discover response carries no
			// visibleTo of its own -- it is a publish-side directive and is
			// never returned -- so the audience we asked for is the only thing
			// the answer is known to be addressed to.
			VisibleTo: []string{audience},
		}
		body, err := sink.BuildPushBody(meta, document)
		if err != nil {
			return fmt.Errorf("build push body for %s: %w", audience, err)
		}
		if _, err := p.push.Push(ctx, p.pushEndpoint, body); err != nil {
			return fmt.Errorf("push %s: %w", audience, err)
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

// withPage puts limit and offset on a discover URL.
//
// Discovery reads paging off the query string rather than the intent, because
// the Beckn discover schema has no paging member and refuses one in the body.
//
// An unparseable URL is returned untouched rather than refused: the request is
// about to be made against it anyway, and failing there says what is wrong with
// the registry's baseUrl far better than a paging helper can.
func withPage(raw string, limit, offset int) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// packContextFile is what a schema pack URL's last segment becomes.
//
// A pack names its attributes.yaml -- the pack definition -- while a message is
// interpreted against the JSON-LD context beside it. The architecture document
// defines both and keeps them separate: SchemaRegistry.schemaUrl is the
// attributes.yaml, and context.schemaContext is "an array of JSON-LD context
// URIs used to interpret the message schema".
const packContextFile = "context.jsonld"

// schemaContextsFor is what a discover to this peer filters on.
//
// The peer's own declaration where it made one, and our configured list
// otherwise. A peer that declared nothing is crawled exactly as it was before
// it could say anything about this.
//
// Translated here rather than stored translated: the registry record keeps
// agreeing with the descriptor it was read from, and a change to this mapping
// does not need every admitted peer re-admitted.
func (p *peerCrawl) schemaContextsFor(target peerTarget) []string {
	if len(target.SchemaPacks) == 0 {
		return p.schemaContext
	}

	contexts := make([]string, 0, len(target.SchemaPacks))
	for _, pack := range target.SchemaPacks {
		if context := contextURLOf(pack); context != "" {
			contexts = append(contexts, context)
		}
	}
	if len(contexts) == 0 {
		return p.schemaContext
	}
	return contexts
}

// contextURLOf turns a schema pack URL into the context URL beside it.
//
// Only the final segment changes -- the capability and version in the path are
// the part that identifies the schema, and they are kept exactly as the peer
// published them. A URL that names no file is left alone and dropped by the
// caller, because guessing at a shape we do not recognise would send a peer a
// filter it cannot match and report that as "nothing found".
func contextURLOf(pack string) string {
	parsed, err := url.Parse(strings.TrimSpace(pack))
	if err != nil || parsed.Path == "" {
		return ""
	}
	cut := strings.LastIndex(parsed.Path, "/")
	if cut < 0 || cut == len(parsed.Path)-1 {
		return ""
	}
	parsed.Path = parsed.Path[:cut+1] + packContextFile
	return parsed.String()
}
