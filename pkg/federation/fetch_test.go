package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// peer is one network's published documents plus the host they are served from.
//
// The fetcher builds its URL out of the networkId alone, so a test server's
// host:port has to BE the network id. That forces the order here: start the
// server, learn its host, then sign a descriptor naming it.
type peer struct {
	fetcher    *Fetcher
	host       string
	descriptor *Descriptor
	keys       *KeySet
}

// newPeer starts a server whose documents can be rewritten by the test after it
// knows the host.
func newPeer(t *testing.T) *peer {
	t.Helper()
	p := &peer{descriptor: &Descriptor{}, keys: &KeySet{}}

	mux := http.NewServeMux()
	mux.HandleFunc(WellKnownDescriptor, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(p.descriptor)
	})
	mux.HandleFunc(WellKnownJWKS, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(p.keys)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	p.host = parsed.Host
	p.fetcher = &Fetcher{Client: server.Client(), Scheme: "http"}
	return p
}

// publishes signs a descriptor for networkID and serves it with matching keys.
func (p *peer) publishes(t *testing.T, networkID string) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(nil)

	d := descriptorAt(1)
	d.NetworkID = networkID
	signed, err := Sign(d, "gov-1", private)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	*p.descriptor = signed
	*p.keys = keysetOf(t, "gov-1", public)
}

func TestFetchReturnsAVerifiedDescriptor(t *testing.T) {
	p := newPeer(t)
	p.publishes(t, p.host)

	descriptor, keys, err := p.fetcher.Fetch(context.Background(), p.host)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if descriptor.NetworkID != p.host {
		t.Fatalf("networkId = %q, want %q", descriptor.NetworkID, p.host)
	}
	if len(keys.Keys) != 1 {
		t.Fatalf("got %d keys, want the one that signed it", len(keys.Keys))
	}
}

// A network serving a descriptor that names a DIFFERENT network is either
// misconfigured or trying to be admitted under someone else's identity. Either
// way it must not become a candidate.
func TestFetchRefusesADescriptorForAnotherNetwork(t *testing.T) {
	p := newPeer(t)
	p.publishes(t, "someone-else.oan.local")

	_, _, err := p.fetcher.Fetch(context.Background(), p.host)
	if err == nil {
		t.Fatal("a descriptor naming another network was accepted")
	}
	if !strings.Contains(err.Error(), "someone-else.oan.local") {
		t.Fatalf("err = %v, want it to name the network the descriptor claims", err)
	}
}

// The two documents are served together but are separate objects. A JWKS that
// does not carry the signing key means the pair cannot be trusted, however
// well-formed each half is alone.
func TestFetchRefusesWhenTheKeysCannotHaveSignedIt(t *testing.T) {
	p := newPeer(t)
	p.publishes(t, p.host)

	// Swap in a key that did not sign this descriptor.
	otherPublic, _, _ := ed25519.GenerateKey(nil)
	*p.keys = keysetOf(t, "gov-1", otherPublic)

	_, _, err := p.fetcher.Fetch(context.Background(), p.host)
	if err == nil {
		t.Fatal("a descriptor verified against keys that cannot have signed it")
	}
	if !strings.Contains(err.Error(), "verifying") {
		t.Fatalf("err = %v, want a verification failure", err)
	}
}

func TestFetchRefusesAnEmptyNetworkID(t *testing.T) {
	if _, _, err := NewFetcher("http", time.Second).Fetch(context.Background(), ""); err != ErrMissingNetwork {
		t.Fatalf("err = %v, want ErrMissingNetwork", err)
	}
}

// A peer that is not there must fail naming the document, not in a way a caller
// could mistake for a verification problem.
func TestFetchReportsAMissingDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)

	fetcher := &Fetcher{Client: server.Client(), Scheme: "http"}

	_, _, err := fetcher.Fetch(context.Background(), parsed.Host)
	if err == nil || !strings.Contains(err.Error(), WellKnownDescriptor) {
		t.Fatalf("err = %v, want it to name the document that was missing", err)
	}
}

// An unadmitted host is the least trusted thing we talk to, and it is talked to
// before any trust decision. A response that never ends must not be read until
// we run out of memory.
func TestFetchStopsReadingAnEndlessDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("a", 64*1024)
		for i := 0; i < 64; i++ { // 4 MiB, well past the 1 MiB cap
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)

	fetcher := &Fetcher{Client: server.Client(), Scheme: "http"}

	// It must fail to parse rather than hang or exhaust memory: the read stops
	// at the cap, leaving truncated JSON.
	if _, _, err := fetcher.Fetch(context.Background(), parsed.Host); err == nil {
		t.Fatal("an oversized document was accepted")
	}
}
