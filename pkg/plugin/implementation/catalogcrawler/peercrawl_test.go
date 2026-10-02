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

// recordingPush captures what a crawl would have sent on, so the origin tag can
// be asserted without a discovery-service to send it to.
type recordingPush struct {
	origins []string
	bodies  [][]byte
}

func (r *recordingPush) Push(_ context.Context, _ string, body []byte, sourceNetwork string) (sink.BatchOutcome, error) {
	r.origins = append(r.origins, sourceNetwork)
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

// catalogsPage serves a peer that answers with n catalogs, then nothing.
func catalogsPage(t *testing.T, pages int, perPage int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		catalogs := []string{}
		if calls <= pages {
			for i := 0; i < perPage; i++ {
				catalogs = append(catalogs, `{"id":"c1"}`)
			}
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
func TestCrawlPeerDeclaresOurNetwork(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"message":{"catalogs":[]}}`)
	}))
	defer server.Close()

	newPeerCrawl(t, &recordingPush{}).
		crawlPeer(context.Background(), peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})

	envelope, _ := body["context"].(map[string]any)
	if envelope["networkId"] != "bharatvistar.oan.local" {
		t.Fatalf("networkId = %v, want our own network", envelope["networkId"])
	}
}

// Everything stored from a peer is tagged with where it came from, which is what
// the origin filter reads to keep it from being re-exported.
func TestCrawlPeerTagsWhatItPushedWithTheOrigin(t *testing.T) {
	server, _ := catalogsPage(t, 1, 1)
	push := &recordingPush{}

	err := newPeerCrawl(t, push).crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha.oan.local", DiscoveryURL: server.URL})
	if err != nil {
		t.Fatalf("crawlPeer: %v", err)
	}

	if len(push.origins) != 1 {
		t.Fatalf("pushed %d catalogs, want 1", len(push.origins))
	}
	if push.origins[0] != "maha.oan.local" {
		t.Fatalf("origin = %q, want the peer we fetched it from", push.origins[0])
	}
}

// A discover answers one page, so the crawl keeps asking until the peer stops
// returning rows. Without this a peer with more catalogs than fit one page is
// silently half-crawled.
func TestCrawlPeerPagesUntilExhausted(t *testing.T) {
	server, calls := catalogsPage(t, 2, 1)

	newPeerCrawl(t, &recordingPush{}).crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})

	if *calls != 3 {
		t.Fatalf("made %d requests, want 3 — two pages of results and one empty", *calls)
	}
}

// A peer that never stops must not hold the pass open for ever.
func TestCrawlPeerStopsAtTheCap(t *testing.T) {
	server, calls := catalogsPage(t, 1000, 1)

	newPeerCrawl(t, &recordingPush{}).crawlPeer(context.Background(),
		peerTarget{NetworkID: "maha", DiscoveryURL: server.URL})

	if *calls != 5 {
		t.Fatalf("made %d requests, want the 5-page cap", *calls)
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
