package catalogcrawler

import (
	"context"
	"encoding/json"
	"net/http"
)

// peerCrawler is the one method this endpoint needs.
//
// Declared here rather than added to definition.Crawler, the shared plugin
// contract. Every deployment compiles that interface, including those that will
// never federate, and a method there would oblige any future Crawler to
// implement peer crawling to satisfy it. One handler asks this question; the
// interface belongs beside the handler.
type peerCrawler interface {
	// CrawlPeers runs an immediate pass over every ADMITTED PEER NETWORK.
	//
	// A different input model from CrawlRegistry, because a peer is a different
	// thing: it publishes no catalog index, so it is asked with a signed Beckn
	// discover rather than fetched. It takes no networks because the set is not
	// the caller's to choose.
	//
	// Returns a run ID, or an error when federation is not configured.
	CrawlPeers(ctx context.Context) (string, error)
}

// newPeersHandler serves POST /crawl/peers: one immediate pass over every
// admitted peer network.
//
// It takes no body. The set of peers is not the caller's to choose -- a peer is
// one an operator admitted, and the registry is the record of that. An endpoint
// that accepted a list would let anything able to reach the adapter crawl a
// network nobody agreed to deal with.
func newPeersHandler(crawler peerCrawler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		runID, err := crawler.CrawlPeers(r.Context())
		if err != nil {
			// The crawler refuses for exactly two reasons -- federation is not
			// configured, or it is not running -- and both are this
			// deployment's state rather than anything about the request.
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
	})
}
