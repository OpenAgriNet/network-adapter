package federation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WellKnownDescriptor and WellKnownJWKS are the two fixed paths. They are not
// configurable: a peer derives them from the networkId alone, which is the whole
// reason a network can be found knowing nothing but its domain.
const (
	WellKnownDescriptor = "/.well-known/openagrinet"
	WellKnownJWKS       = "/.well-known/jwks.json"
)

// maxDocumentBytes caps what will be read from a peer.
//
// These documents are small and a peer is not yet trusted -- it has not been
// admitted at the point they are fetched. Without a cap, the first thing an
// unknown host can do to us is stream until we run out of memory.
const maxDocumentBytes = 1 << 20 // 1 MiB

// Fetcher retrieves and verifies another network's published documents.
//
// Scheme is "https" in any real deployment. It is settable because a local POC
// has no certificate authority and cannot prove domain ownership -- which is
// precisely the property a real deployment relies on, so this is a knob that
// must never be turned in one.
type Fetcher struct {
	Client *http.Client
	Scheme string
}

// NewFetcher returns a Fetcher with a bounded timeout.
func NewFetcher(scheme string, timeout time.Duration) *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: timeout}, Scheme: scheme}
}

// TrustCA makes this fetcher verify certificates against the CAs in a PEM file,
// instead of the system roots.
//
// This is real certificate verification against a different root, NOT skipping
// it: a peer presenting a certificate this CA did not sign, or one whose name
// does not match, is still refused. It exists because a local stack has no
// publicly trusted certificate, and the alternative -- turning verification off
// -- would quietly remove the check in the one place a POC is meant to prove it.
func (f *Fetcher) TrustCA(pemBytes []byte) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return fmt.Errorf("federation: no certificate found in the CA file")
	}

	transport := f.transport()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.RootCAs = pool

	client := *f.client()
	client.Transport = transport
	f.Client = &client
	return nil
}

// transport returns a private copy of this fetcher's transport, so changing it
// cannot reach into a client the caller shares with anything else.
func (f *Fetcher) transport() *http.Transport {
	if existing, ok := f.client().Transport.(*http.Transport); ok && existing != nil {
		return existing.Clone()
	}
	return http.DefaultTransport.(*http.Transport).Clone()
}

// ResolveTo makes this fetcher reach networkID at a given address, the way
// curl --resolve does.
//
// The request still carries the networkId as its Host, and the descriptor is
// still checked against it, so nothing about verification is relaxed -- only
// where the connection goes. It exists because a network id is a DNS domain
// and an operator's own machine usually cannot resolve one belonging to a
// stack running in containers.
func (f *Fetcher) ResolveTo(networkID, address string) {
	base := f.transport()
	dial := base.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, _, err := net.SplitHostPort(addr); err == nil && host == networkID {
			addr = address
		}
		return dial(ctx, network, addr)
	}
	client := *f.client()
	client.Transport = base
	f.Client = &client
}

// Fetch retrieves a network's descriptor and keys, and verifies one against the
// other.
//
// A verified descriptor is a CANDIDATE, not a trusted peer. Both documents came
// from the same host, so this proves self-consistency and nothing more -- any
// network can sign its own claims. Admission is the separate, local decision
// that turns a candidate into a peer.
func (f *Fetcher) Fetch(ctx context.Context, networkID string) (Descriptor, KeySet, error) {
	if networkID == "" {
		return Descriptor{}, KeySet{}, ErrMissingNetwork
	}

	var descriptor Descriptor
	if err := f.get(ctx, networkID, WellKnownDescriptor, &descriptor); err != nil {
		return Descriptor{}, KeySet{}, err
	}
	var keys KeySet
	if err := f.get(ctx, networkID, WellKnownJWKS, &keys); err != nil {
		return Descriptor{}, KeySet{}, err
	}

	// The descriptor says who it is for. A document served at one domain that
	// names another is either a misconfiguration or an attempt to be admitted
	// under someone else's name, and neither should get further than here.
	if descriptor.NetworkID != networkID {
		return Descriptor{}, KeySet{}, fmt.Errorf(
			"federation: %s publishes a descriptor for %q", networkID, descriptor.NetworkID)
	}
	if err := Verify(descriptor, keys); err != nil {
		return Descriptor{}, KeySet{}, fmt.Errorf("federation: verifying %s: %w", networkID, err)
	}
	return descriptor, keys, nil
}

// get reads one published document.
func (f *Fetcher) get(ctx context.Context, networkID, path string, into any) error {
	endpoint := (&url.URL{Scheme: f.scheme(), Host: networkID, Path: path}).String()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("federation: %s: %w", endpoint, err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := f.client().Do(request)
	if err != nil {
		return fmt.Errorf("federation: fetching %s: %w", endpoint, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("federation: %s returned %s", endpoint, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDocumentBytes))
	if err != nil {
		return fmt.Errorf("federation: reading %s: %w", endpoint, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("federation: parsing %s: %w", endpoint, err)
	}
	return nil
}

func (f *Fetcher) scheme() string {
	if strings.TrimSpace(f.Scheme) == "" {
		return "https"
	}
	return f.Scheme
}

func (f *Fetcher) client() *http.Client {
	if f.Client == nil {
		return http.DefaultClient
	}
	return f.Client
}
