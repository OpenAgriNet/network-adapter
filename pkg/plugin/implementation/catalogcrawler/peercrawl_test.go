package catalogcrawler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
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

func newPeerCrawl(t *testing.T, push catalogPusher) *peerCrawl {
	t.Helper()
	return &peerCrawl{
		signer:       stubSigner{},
		localNetwork: "bharatvistar.oan.local",
		subscriberID: "network.bharatvistar.oan.local",
		privateKey:   "test-key",
		window:       time.Minute,
		intent:       map[string]any{},
		// Low enough that a peer which never returns an empty page still
		// terminates the test.
		maxPages:     5,
		client:       http.DefaultClient,
		push:         push,
		pushEndpoint: "http://discovery.invalid/publish",
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

// Our OWN network id goes in the context, not the peer's. It is what the peer
// scopes its answer by, and so what keeps a third network's cached rows out of
// what we receive.
// The discover names WHOSE CATALOGS we want, not who is asking.
//
// A peer's database holds its own catalogs and copies of what it crawled
// elsewhere. Naming the peer is what leaves those copies behind.
func TestCrawlPeerAsksForThePeersOwnCatalogs(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	newPeerCrawl(t, &recordingPush{}).
		crawlPeer(context.Background(), peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})

	envelope, _ := body["context"].(map[string]any)
	if envelope["networkId"] != "maha" {
		t.Fatalf("networkId = %v, want the peer we are asking", envelope["networkId"])
	}
}

// A crawled catalogue stays labelled as the SOURCE network's. We hold a copy;
// we do not become its owner.
//
// This is what makes "answer with your own data only" true by construction: a
// peer asks us for a specific owner, so copies of a third network's data cannot
// match. Stamping our own id here would relabel Maha's catalog as ours.
func TestCrawlPeerKeepsTheSourceNetworkOnWhatItStores(t *testing.T) {
	server, _ := peerWith(t, 1)
	push := &recordingPush{}

	err := newPeerCrawl(t, push).crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha.oan.local", DiscoveryURL: server.URL})
	if err != nil {
		t.Fatalf("crawlPeer: %v", err)
	}

	if len(push.bodies) != 1 {
		t.Fatalf("pushed %d catalogs, want 1", len(push.bodies))
	}
	// The AUDIENCE specifically, not the body as a whole: our own id also
	// appears as bppId, and legitimately -- we are the one publishing into our
	// own discovery. Only visibleTo says whose catalog it is.
	var pushed struct {
		Message struct {
			PublishDirectives []struct {
				VisibleTo []string `json:"visibleTo"`
			} `json:"publishDirectives"`
		} `json:"message"`
	}
	if err := json.Unmarshal(push.bodies[0], &pushed); err != nil {
		t.Fatalf("push body is not JSON: %v", err)
	}
	if len(pushed.Message.PublishDirectives) != 1 {
		t.Fatalf("got %d directives, want 1", len(pushed.Message.PublishDirectives))
	}

	audience := pushed.Message.PublishDirectives[0].VisibleTo
	if len(audience) != 1 || audience[0] != "maha.oan.local" {
		t.Errorf("visibleTo = %v, want only the network that owns it", audience)
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

	push := &recordingPush{}
	if err := newPeerCrawl(t, push).crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha.oan.local", DiscoveryURL: server.URL}); err != nil {
		t.Fatalf("crawlPeer: %v", err)
	}

	if len(push.bodies) != 1 {
		t.Fatalf("pushed %d bodies, want 1", len(push.bodies))
	}
	body := string(push.bodies[0])

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
