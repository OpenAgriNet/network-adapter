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

// A peer's catalogue is stored visible to OUR network and no one else. That
// single value is the whole no-re-export rule: a third network asking us
// matches nothing, with no origin column needed to enforce it.
func TestCrawlPeerStoresAPeersCatalogVisibleToUsOnly(t *testing.T) {
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
	body := string(push.bodies[0])

	if !strings.Contains(body, `"bharatvistar.oan.local"`) {
		t.Errorf("a crawled catalog was not made visible to our own network: %s", body)
	}
	if strings.Contains(body, "maha.oan.local") {
		t.Errorf("the peer was named in the audience, which would re-export it: %s", body)
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
