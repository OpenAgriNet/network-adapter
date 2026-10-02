package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/federation"
)

// peerAccess is how an operator reaches a peer's published documents.
//
// Shared by verify and admit so the two cannot disagree about what verification
// means -- the difference between them is what they do with the result, not how
// they obtain it.
type peerAccess struct {
	scheme  string
	caFile  string
	resolve string
	timeout time.Duration
}

func (a *peerAccess) bind(set *flag.FlagSet) {
	set.StringVar(&a.scheme, "scheme", "https", "http only for a stack with no certificates at all")
	set.StringVar(&a.caFile, "ca-file", "", "verify the peer's certificate against the CAs in this PEM file")
	set.StringVar(&a.resolve, "resolve", "", "reach the peer at this host:port instead of resolving its domain")
	set.DurationVar(&a.timeout, "timeout", 10*time.Second, "per-request timeout")
}

// fetcher builds a Fetcher from these options.
func (a peerAccess) fetcher(networkID string) (*federation.Fetcher, error) {
	f := federation.NewFetcher(a.scheme, a.timeout)

	if a.caFile != "" {
		pem, err := os.ReadFile(a.caFile)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", a.caFile, err)
		}
		if err := f.TrustCA(pem); err != nil {
			return nil, err
		}
	}
	if a.resolve != "" {
		// The Host header and the descriptor check still use the network id;
		// only the connection goes elsewhere.
		f.ResolveTo(networkID, a.resolve)
	}
	return f, nil
}
