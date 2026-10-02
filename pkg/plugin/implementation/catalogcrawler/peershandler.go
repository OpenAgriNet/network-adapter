package catalogcrawler

import (
	"encoding/json"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// newPeersHandler serves POST /crawl/peers: one immediate pass over every
// admitted peer network.
//
// It takes no body. The set of peers is not the caller's to choose -- a peer is
// one an operator admitted, and the registry is the record of that. An endpoint
// that accepted a list would let anything able to reach the adapter crawl a
// network nobody agreed to deal with.
func newPeersHandler(crawler definition.Crawler) http.Handler {
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
