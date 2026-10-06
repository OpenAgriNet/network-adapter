package catalogcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/store"
	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
)

// stubSigner stands in for the Signer plugin. These tests are about what we send
// a peer, not about Ed25519, which the signer's own tests cover.
type stubSigner struct{ signature string }

func (s stubSigner) Sign(context.Context, []byte, string, int64, int64) (string, error) {
	if s.signature == "" {
		return `Signature keyId="bharatvistar.oan.local|op-1|ed25519"`, nil
	}
	return s.signature, nil
}

// recordingPush captures what a crawl would have sent on, so the body can be
// asserted without a discovery-service to send it to.
type recordingPush struct {
	bodies [][]byte
}

func (r *recordingPush) Push(_ context.Context, _ string, body []byte) (sink.BatchOutcome, error) {
	r.bodies = append(r.bodies, body)
	return sink.BatchOutcome{Acked: true}, nil
}

// Who we are, as the peer sees us. The discover asks for the audience this id
// belongs to.
const testSubscriberID = "network.bharatvistar.oan.local"

// fakeStaged is the queue, in memory: the same stage-then-publish shape the
// postgres one has, with enough of the claim semantics to drive the worker.
type fakeStaged struct {
	mu      sync.Mutex
	staged  map[string]store.StagedCatalog // keyed network|catalog
	queued  []string                       // keys, in enqueue order
	claimed map[string]bool
	settled []string
	retried []string
	parked  []string
	// attempts is what a claimed row reports as its prior failure count, so a
	// test can drive the publisher to the end of its budget.
	attempts int
}

func newFakeStaged() *fakeStaged {
	return &fakeStaged{staged: map[string]store.StagedCatalog{}, claimed: map[string]bool{}}
}

func stagedKey(networkID, catalogID string) string { return networkID + "|" + catalogID }

// recordingSink captures what was published, so a test can assert on the entry
// the publisher built -- which is where visibleTo and the catalog type come
// from now that Send does the rest.
type recordingSink struct {
	mu      sync.Mutex
	entries []catalog.CatalogEntry
	bodies  [][]byte
	fail    error
	refuse  string
}

func (r *recordingSink) Send(_ context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return crawlmanager.SinkOutcome{}, r.fail
	}
	if r.refuse != "" {
		return crawlmanager.SinkOutcome{Accepted: false, Reason: r.refuse}, nil
	}
	r.entries = append(r.entries, entry)
	r.bodies = append(r.bodies, content)
	return crawlmanager.SinkOutcome{Accepted: true}, nil
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// failingPush is a discovery that will not take a push.
type failingPush struct{}

func (failingPush) Push(_ context.Context, _ string, _ []byte) (sink.BatchOutcome, error) {
	return sink.BatchOutcome{}, errors.New("discovery is unwell")
}

func (f *fakeStaged) StageAndEnqueue(_ context.Context, sc store.StagedCatalog, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := stagedKey(sc.NetworkID, sc.CatalogID)
	if _, seen := f.staged[key]; !seen {
		f.queued = append(f.queued, key)
	}
	f.staged[key] = sc // re-staging overwrites, as the real one does
	return nil
}

func (f *fakeStaged) ClaimNextStaged(_ context.Context) (*store.ClaimedStaged, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range f.queued {
		if f.claimed[key] {
			continue
		}
		f.claimed[key] = true
		return &store.ClaimedStaged{
			ID: key, ClaimID: "claim", Attempts: f.attempts, StagedCatalog: f.staged[key],
		}, nil
	}
	return nil, nil
}

func (f *fakeStaged) CompleteStaged(_ context.Context, item *store.ClaimedStaged) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.staged, item.ID)
	f.settled = append(f.settled, item.ID)
	return nil
}

func (f *fakeStaged) RescheduleStaged(_ context.Context, id, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, id)
	return nil
}

func (f *fakeStaged) ParkStaged(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = append(f.parked, id)
	return nil
}

// stagedCount is how many documents are being held.
func (f *fakeStaged) stagedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.staged)
}

func newPeerCrawl(t *testing.T, push catalogPusher) *peerCrawl {
	t.Helper()
	return &peerCrawl{
		signer:       stubSigner{},
		localNetwork: "bharatvistar.oan.local",
		subscriberID: testSubscriberID,
		privateKey:   "test-key",
		window:       time.Minute,
		intent:       map[string]any{},
		// Low enough that a peer which never returns an empty page still
		// terminates the test.
		maxPages:     5,
		client:       http.DefaultClient,
		push:         push,
		pushEndpoint: "http://discovery.invalid/publish",
		store:        newFakeStaged(),
		sink:         &recordingSink{},
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// peerWith serves a peer that answers with n catalogs.
func peerWith(t *testing.T, catalogCount int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		catalogs := []string{}
		for i := 0; i < catalogCount; i++ {
			catalogs = append(catalogs, `{"id":"c1"}`)
		}
		_, _ = io.WriteString(w,
			`{"message":{"catalogs":[`+strings.Join(catalogs, ",")+`]}}`)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// A peer that received an unsigned body could not tell us apart from anyone
// else, and would answer with its local view instead of the federated one.
func TestCrawlPeerSendsASignedDiscover(t *testing.T) {
	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(body)
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	err := newPeerCrawl(t, &recordingPush{}).
		crawlPeer(context.Background(), peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})
	if err != nil {
		t.Fatalf("crawlPeer: %v", err)
	}

	if gotAuth == "" {
		t.Error("the discover went out unsigned")
	}
	if !strings.Contains(gotBody, `"action":"discover"`) {
		t.Errorf("body = %s, want a Beckn discover", gotBody)
	}
}

// The discover names the AUDIENCE we belong to: give us what you decided to
// share with us.
//
// visibleTo is an audience list, so our own id is what a peer's publisher names
// to share a catalog with us. Asking with the PEER's id instead would return
// everything it owns -- a catalog with no declared audience is filled with its
// own network -- and leave its publishers no say in what we take.
func TestCrawlPeerAsksForTheAudienceWeBelongTo(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	newPeerCrawl(t, &recordingPush{}).
		crawlPeer(context.Background(), peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})

	envelope, _ := body["context"].(map[string]any)
	// Deliberately NOT "maha". A test asserting the peer's id would pass while
	// the crawl took everything that peer holds regardless of who its
	// publishers meant it for.
	if envelope["networkId"] != testSubscriberID {
		t.Fatalf("networkId = %v, want our own id %q", envelope["networkId"], testSubscriberID)
	}
}

// A catalog in BOTH audiences' answers is staged under ONE key, and the last
// write decides what it is addressed to.
//
// Ours is asked second on purpose, so our copy ends up addressed to US. The
// other way round it carries the other network's id -- and that network's own
// crawl of us then matches it and re-imports its own catalogs, every pass.
func TestCrawlPeerStampsTheAudienceItAskedFor(t *testing.T) {
	server, _ := peerWith(t, 1)
	crawl := newPeerCrawl(t, &recordingPush{})
	published := crawl.sink.(*recordingSink)

	err := crawl.crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha.oan.local", DiscoveryURL: server.URL})
	if err != nil {
		t.Fatalf("crawlPeer: %v", err)
	}
	crawl.publishStaged(context.Background())

	// ONE publish, not one per audience: both answers carry the same catalog
	// id, so they are the same staged row.
	if published.count() != 1 {
		t.Fatalf("published %d catalogs, want 1 -- both audiences stage the same id", published.count())
	}
	entry := published.entries[0]
	if len(entry.NetworkIDs) != 1 || entry.NetworkIDs[0] != testSubscriberID {
		t.Errorf("networkIds = %v, want [%s] -- ours is asked last so it wins",
			entry.NetworkIDs, testSubscriberID)
	}
	if entry.CatalogType != "REGULAR" {
		t.Errorf("catalogType = %q, want REGULAR -- a crawled catalog is never a master", entry.CatalogType)
	}
}

// One unreachable peer must not abandon the others. A network being down is
// routine; it is not a reason to leave the rest of the cache unrefreshed.
func TestCrawlAllContinuesPastAFailingPeer(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer up.Close()

	newPeerCrawl(t, &recordingPush{}).crawlAll(context.Background(), []peerTarget{
		{NetworkID: "down", DiscoveryURL: down.URL},
		{NetworkID: "up", DiscoveryURL: up.URL},
	}, "run-1")

	if !reached {
		t.Fatal("a failing peer stopped the pass before the healthy one")
	}
}

// A discover match carries the provider's endpoint, and the next Beckn action
// goes to exactly that endpoint. If the crawl drops it a federated match is
// useless: the Experience gets a result it cannot act on, and nothing downstream
// can reconstruct the endpoint, because our registry has no record of another
// network's provider.
func TestCrawlPeerKeepsTheProviderEndpoint(t *testing.T) {
	const published = `{"id":"c1","bppId":"pocra.mahavistara","bppUri":"https://pocra.example.org/beckn"}`

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls > 1 {
			_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"catalogs":[`+published+`]}}`)
	}))
	defer server.Close()

	crawl := newPeerCrawl(t, &recordingPush{})
	sent := crawl.sink.(*recordingSink)
	if err := crawl.crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha.oan.local", DiscoveryURL: server.URL}); err != nil {
		t.Fatalf("crawlPeer: %v", err)
	}
	crawl.publishStaged(context.Background())

	if sent.count() != 1 {
		t.Fatalf("published %d catalogs, want 1", sent.count())
	}
	body := string(sent.bodies[0])

	// Verbatim, and never rewritten to point at us: the request goes from the
	// originating adapter straight to the provider, so an adapter that pointed a
	// cached endpoint at itself would have quietly made itself a relay.
	if !strings.Contains(body, `"bppUri":"https://pocra.example.org/beckn"`) {
		t.Errorf("the provider endpoint did not survive the crawl unchanged: %s", body)
	}
	if !strings.Contains(body, `"bppId":"pocra.mahavistara"`) {
		t.Errorf("the provider id did not survive the crawl: %s", body)
	}
}

// --- who counts as a peer -------------------------------------------------

// fakePeerLookup is a registry that returns whatever the test puts in it.
type fakePeerLookup struct{ subs []model.Subscription }

func (f fakePeerLookup) Lookup(context.Context, *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

func (f fakePeerLookup) AdmittedPeers(context.Context) ([]model.Subscription, error) {
	return f.subs, nil
}

func peerSub(id string) model.Subscription {
	return model.Subscription{
		Subscriber: model.Subscriber{SubscriberID: id, URL: "https://" + id + "/federation/discovery"},
	}
}

// We must not crawl OURSELVES.
//
// Role "network" matches this deployment's own entry as well as a real peer,
// and the registry cannot tell which record is the caller's.
//
// The comparison is a plain one, and it is only plain because a network is now
// ONE participant: our entry IS our network id. It used to register as
// "network.<networkId>", so comparing the network id matched nothing and the
// crawler cheerfully crawled itself -- which is what this test was written for.
func TestPeerTargetsDropsOurselves(t *testing.T) {
	crawler := &crawlerImpl{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		peers: &peerCrawl{
			localNetwork: "bharatvistar.oan.local",
			subscriberID: "bharatvistar.oan.local",
		},
		registry: fakePeerLookup{subs: []model.Subscription{
			peerSub("bharatvistar.oan.local"), // ours
			peerSub("mahavistara.oan.local"),  // an actual peer
		}},
	}

	targets, err := crawler.peerTargets(context.Background())
	if err != nil {
		t.Fatalf("peerTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want only the peer: %+v", len(targets), targets)
	}
	if targets[0].NetworkID != "mahavistara.oan.local" {
		t.Fatalf("target = %q, want the peer", targets[0].NetworkID)
	}
}

// A peer with no endpoint is skipped: there is nowhere to send a discover.
func TestPeerTargetsSkipsAPeerWithNoUrl(t *testing.T) {
	bare := model.Subscription{Subscriber: model.Subscriber{SubscriberID: "maha.oan.local"}}
	crawler := &crawlerImpl{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		peers:    &peerCrawl{localNetwork: "bharatvistar.oan.local", subscriberID: "bharatvistar.oan.local"},
		registry: fakePeerLookup{subs: []model.Subscription{bare}},
	}

	targets, err := crawler.peerTargets(context.Background())
	if err != nil {
		t.Fatalf("peerTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("got %+v, want none", targets)
	}
}

// --- the signature a peer actually receives -------------------------------

// A crawl carries a FULL Authorization header, not a bare signature.
//
// It used to send only the base64, which left a peer holding something it could
// not attribute to anybody: no sender, no key to check it against. The envelope
// is what makes a signature verifiable at all, and this is the one place
// outside the handler's sign step that has to build it.
func TestCrawlSendsAnAttributableSignature(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	crawl := newPeerCrawl(t, &recordingPush{})
	crawl.subscriberID = "bharatvistar.oan.local"
	crawl.keyID = "1-abc-def"
	crawl.crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})

	if !strings.HasPrefix(auth, "Signature ") {
		t.Fatalf("Authorization = %q, want a Signature header", auth)
	}
	// WHO is asking, so a peer can scope its answer, and WHICH KEY, so it can
	// pick the right one from among what we published.
	if !strings.Contains(auth, `keyId="bharatvistar.oan.local|1-abc-def|ed25519"`) {
		t.Errorf("Authorization names no usable keyId: %s", auth)
	}
	for _, part := range []string{"algorithm=", "created=", "expires=", "signature="} {
		if !strings.Contains(auth, part) {
			t.Errorf("Authorization is missing %s: %s", part, auth)
		}
	}
}

// The header a crawl builds has to be the SHAPE the handler's sign step
// produces, because the same validator reads both. This pins the format so a
// change on one side shows up here rather than at a peer.
func TestTheAuthorizationShapeMatchesTheSignStep(t *testing.T) {
	got := authorization("bharatvistar.oan.local", "key-1", 1700000000, 1700000030, "c2ln")

	want := `Signature keyId="bharatvistar.oan.local|key-1|ed25519",algorithm="ed25519",` +
		`created="1700000000",expires="1700000030",` +
		`headers="(created) (expires) digest",signature="c2ln"`
	if got != want {
		t.Fatalf("header mismatch\n got: %s\nwant: %s", got, want)
	}
}

// Paging is on the QUERY STRING, because the Beckn discover schema has no
// paging member and a peer refuses one in the body.
//
// Every page is accumulated before anything is pushed. `limit` counts
// RESOURCES, so one catalog's resources can straddle a page boundary -- pushing
// page 1 with updateMode FULL and then page 2 would delete what page 1 wrote.
func TestCrawlPeerFollowsPages(t *testing.T) {
	var queries []string
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		// Two full pages, then an empty one that ends the loop.
		if page++; page <= 2 {
			_, _ = io.WriteString(w, `{"message":{"catalogs":[{"id":"c1"},{"id":"c2"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	crawl := newPeerCrawl(t, &recordingPush{})
	crawl.pageSize = 2
	crawl.maxPages = 5

	got, err := crawl.crawlAudience(context.Background(),
		peerTarget{NetworkID: "maha", DiscoveryURL: server.URL}, "maha")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d catalogs across pages, want 4", len(got))
	}
	want := []string{"limit=2", "limit=2&offset=2", "limit=2&offset=4"}
	if len(queries) != len(want) {
		t.Fatalf("made %d requests (%v), want %d", len(queries), queries, len(want))
	}
	for i, q := range queries {
		if q != want[i] {
			t.Errorf("request %d query = %q, want %q", i, q, want[i])
		}
	}
}

// An unconfigured page size asks ONCE with no limit, which is what this did
// before paging existed. A deployment that has not opted in is unaffected.
func TestCrawlPeerWithoutPageSizeAsksOnce(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"message":{"catalogs":[{"id":"c1"}]}}`)
	}))
	defer server.Close()

	crawl := newPeerCrawl(t, &recordingPush{})
	crawl.pageSize = 0

	if _, err := crawl.crawlAudience(context.Background(),
		peerTarget{NetworkID: "maha", DiscoveryURL: server.URL}, "maha"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(queries) != 1 {
		t.Fatalf("made %d requests, want 1", len(queries))
	}
	if queries[0] != "" {
		t.Errorf("query = %q, want no paging parameters", queries[0])
	}
}

// The push mode is configured, and MERGE is the default because of PAGING.
//
// FULL means "this document is the catalog's complete current set", so every
// page claims to be the whole thing: a 1000-resource catalog fetched in two
// pages of 500 is published twice and the second deletes the first 500.
func TestUpdateModeIsConfigured(t *testing.T) {
	for raw, want := range map[string]string{
		"":       sink.UpdateModeMerge,
		"MERGE":  sink.UpdateModeMerge,
		"FULL":   sink.UpdateModeFull,
		"full":   sink.UpdateModeFull,
		" full ": sink.UpdateModeFull,
		// Unrecognised is MERGE, not an error: refusing to start over a typo
		// would take the federated half down for a value whose safe reading is
		// the one it already had.
		"nonsense": sink.UpdateModeMerge,
	} {
		if got := updateModeOr(raw); got != want {
			t.Errorf("updateModeOr(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A crawl STAGES and does not publish. What it holds is one page; the worker
// publishes from the database afterwards.
//
// Asserted by the push count being zero while content is staged -- the only
// evidence that nothing is being accumulated and sent in one go.
func TestCrawlStagesAndDoesNotPublish(t *testing.T) {
	push := &recordingPush{}
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if page++; page <= 2 {
			_, _ = io.WriteString(w, `{"message":{"catalogs":[{"id":"c1"},{"id":"c2"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	crawl := newPeerCrawl(t, push)
	crawl.pageSize = 2
	staged := crawl.store.(*fakeStaged)

	ids, err := crawl.crawlAudience(context.Background(),
		peerTarget{NetworkID: "maha", DiscoveryURL: server.URL}, "maha")
	if err != nil {
		t.Fatalf("crawlAudience: %v", err)
	}
	if len(ids) != 4 {
		t.Fatalf("returned %d ids across pages, want 4", len(ids))
	}
	if len(push.bodies) != 0 {
		t.Errorf("crawl pushed %d bodies; staging must not publish", len(push.bodies))
	}
	// c1 and c2, restaged on the second page under the same keys.
	if got := staged.stagedCount(); got != 2 {
		t.Errorf("staged %d catalogs, want 2 (the same ids restaged)", got)
	}
}

// The worker publishes what was staged, stamps the audience the crawl asked
// for, and settles the queue row.
func TestPublishStagedSendsAndSettles(t *testing.T) {
	crawl := newPeerCrawl(t, &recordingPush{})
	staged := crawl.store.(*fakeStaged)
	published := crawl.sink.(*recordingSink)

	if err := staged.StageAndEnqueue(context.Background(), store.StagedCatalog{
		NetworkID: "maha.oan.local",
		CatalogID: "c1",
		Audience:  testSubscriberID,
		Document:  json.RawMessage(`{"id":"c1"}`),
	}, "http://maha.invalid/discover"); err != nil {
		t.Fatalf("StageAndEnqueue: %v", err)
	}

	crawl.publishStaged(context.Background())

	if published.count() != 1 {
		t.Fatalf("published %d catalogs, want 1", published.count())
	}
	entry := published.entries[0]
	if len(entry.NetworkIDs) != 1 || entry.NetworkIDs[0] != testSubscriberID {
		t.Errorf("networkIds = %v, want [%s]", entry.NetworkIDs, testSubscriberID)
	}
	if entry.CatalogID != "c1" {
		t.Errorf("catalogId = %q, want c1", entry.CatalogID)
	}
	if len(staged.settled) != 1 {
		t.Errorf("settled %d rows, want 1", len(staged.settled))
	}
	if staged.stagedCount() != 0 {
		t.Error("the staged document was not released after publishing")
	}
}

// A failed publish is rescheduled, not settled, and the content stays staged --
// so the retry republishes rather than re-asking the network that owns it.
func TestPublishStagedRetriesOnFailure(t *testing.T) {
	crawl := newPeerCrawl(t, &recordingPush{})
	crawl.sink = &recordingSink{fail: errors.New("discovery is unwell")}
	staged := crawl.store.(*fakeStaged)

	if err := staged.StageAndEnqueue(context.Background(), store.StagedCatalog{
		NetworkID: "maha.oan.local", CatalogID: "c1", Audience: "a",
		Document: json.RawMessage(`{"id":"c1"}`),
	}, "http://maha.invalid/discover"); err != nil {
		t.Fatalf("StageAndEnqueue: %v", err)
	}

	crawl.publishStaged(context.Background())

	if len(staged.retried) != 1 {
		t.Errorf("rescheduled %d rows, want 1", len(staged.retried))
	}
	if len(staged.settled) != 0 {
		t.Errorf("settled %d rows; a failed publish must not settle", len(staged.settled))
	}
	if staged.stagedCount() != 1 {
		t.Error("the staged document was released despite the publish failing")
	}
}

// A queue row whose content is gone is settled, not retried for ever.
func TestPublishStagedDropsAnEmptyRow(t *testing.T) {
	crawl := newPeerCrawl(t, &recordingPush{})
	staged := crawl.store.(*fakeStaged)

	if err := staged.StageAndEnqueue(context.Background(), store.StagedCatalog{
		NetworkID: "maha.oan.local", CatalogID: "c1", Audience: "a",
	}, "http://maha.invalid/discover"); err != nil {
		t.Fatalf("StageAndEnqueue: %v", err)
	}

	crawl.publishStaged(context.Background())

	if len(staged.settled) != 1 {
		t.Errorf("settled %d rows, want 1 -- an empty row must not be claimed for ever", len(staged.settled))
	}
}

// A network declares what it can speak, and the crawl asks for exactly that.
//
// Translated at crawl time, not at admission: the registry record keeps
// agreeing with the descriptor it was read from.
func TestSchemaContextComesFromTheirDeclaration(t *testing.T) {
	got := newPeerCrawl(t, &recordingPush{}).schemaContextsFor(peerTarget{SchemaPacks: []string{
		"https://raw.githubusercontent.com/OpenAgriNet/network-specs/main/schema/WeatherObservation/v0.1/attributes.yaml",
		"https://raw.githubusercontent.com/OpenAgriNet/network-specs/main/api-schemas/PMFBYGrievance/v0.1/attributes.yaml",
	}})

	want := []string{
		"https://raw.githubusercontent.com/OpenAgriNet/network-specs/main/schema/WeatherObservation/v0.1/context.jsonld",
		"https://raw.githubusercontent.com/OpenAgriNet/network-specs/main/api-schemas/PMFBYGrievance/v0.1/context.jsonld",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("context %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A network that declared nothing is asked with NO schema filter. Nothing is
// substituted -- what it does with an unnarrowed filter is its decision.
func TestNoDeclarationMeansNoSchemaFilter(t *testing.T) {
	crawl := newPeerCrawl(t, &recordingPush{})
	for name, target := range map[string]peerTarget{
		"none":       {},
		"empty":      {SchemaPacks: []string{}},
		"unusable":   {SchemaPacks: []string{"", "   "}},
		"noFilePart": {SchemaPacks: []string{"https://example.test/"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := crawl.schemaContextsFor(target); len(got) != 0 {
				t.Errorf("got %v, want no filter at all", got)
			}
		})
	}
}

// Only the final segment changes. The capability and version in the path are
// what identify the schema, and are kept exactly as the peer published them.
func TestContextURLOf(t *testing.T) {
	for pack, want := range map[string]string{
		"https://x.test/schema/Weather/v0.1/attributes.yaml": "https://x.test/schema/Weather/v0.1/context.jsonld",
		"https://x.test/a/b/c.yml":                           "https://x.test/a/b/context.jsonld",
		// Nothing to replace, so nothing is guessed.
		"https://x.test/": "",
		"":                "",
		"   ":             "",
	} {
		if got := contextURLOf(pack); got != want {
			t.Errorf("contextURLOf(%q) = %q, want %q", pack, got, want)
		}
	}
}

// A publish that keeps failing is PARKED once its attempts are spent, not
// retried for ever.
//
// A parked row is not claimable until the queue's own sweep revives it, which
// is the same treatment the local crawl's failures get -- and without it a
// catalog discovery will never accept is republished on every pass, for ever.
func TestPublishStagedParksWhenAttemptsAreSpent(t *testing.T) {
	crawl := newPeerCrawl(t, &recordingPush{})
	crawl.sink = &recordingSink{fail: errors.New("discovery is unwell")}
	crawl.maxAttempts = 3
	staged := crawl.store.(*fakeStaged)

	if err := staged.StageAndEnqueue(context.Background(), store.StagedCatalog{
		NetworkID: "maha.oan.local", CatalogID: "c1", Audience: "a",
		Document: json.RawMessage(`{"id":"c1"}`),
	}, "http://maha.invalid/discover"); err != nil {
		t.Fatalf("StageAndEnqueue: %v", err)
	}

	// Two goes behind it: this attempt is the third and last.
	staged.attempts = 2
	crawl.publishStaged(context.Background())

	if len(staged.parked) != 1 {
		t.Errorf("parked %d rows, want 1 once the budget is spent", len(staged.parked))
	}
	if len(staged.retried) != 0 {
		t.Errorf("rescheduled %d rows; the budget was spent", len(staged.retried))
	}
	// The content stays: the sweep may revive this row, and it would have
	// nothing to publish if the document had been dropped.
	if staged.stagedCount() != 1 {
		t.Error("the staged document was released when the row was parked")
	}
}

// 0 attempts means unlimited, matching what crawlmanager does with the same
// setting -- a deployment that has not set one is never parked.
func TestPublishStagedNeverParksWithoutABudget(t *testing.T) {
	crawl := newPeerCrawl(t, &recordingPush{})
	crawl.sink = &recordingSink{fail: errors.New("discovery is unwell")}
	crawl.maxAttempts = 0
	staged := crawl.store.(*fakeStaged)

	if err := staged.StageAndEnqueue(context.Background(), store.StagedCatalog{
		NetworkID: "maha.oan.local", CatalogID: "c1", Audience: "a",
		Document: json.RawMessage(`{"id":"c1"}`),
	}, "http://maha.invalid/discover"); err != nil {
		t.Fatalf("StageAndEnqueue: %v", err)
	}
	staged.attempts = 99
	crawl.publishStaged(context.Background())

	if len(staged.parked) != 0 {
		t.Errorf("parked %d rows; 0 means unlimited", len(staged.parked))
	}
	if len(staged.retried) != 1 {
		t.Errorf("rescheduled %d rows, want 1", len(staged.retried))
	}
}
