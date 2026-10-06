package catalogcrawler

// peerrefresh.go — how often a network is re-crawled, and reading the ids out
// of what it returned.
//
// Refresh, not expiry. A projectionTtl is a licence with two halves: how long a
// copy may be served, and the obligation to refresh it before that runs out.
// This is the second half. The first -- removing what was not refreshed -- is
// deliberately not implemented: withdrawing means a FULL publish carrying no
// resources, because discovery has no delete, and destroying a catalog's
// content to express "stop serving this" is too blunt an instrument to run
// unattended.

import (
	"encoding/json"
	"time"
)

// refreshesPerTtl is how many passes fit inside the shortest licence.
//
// More than one, deliberately. Refreshing exactly at the ttl means a single
// failed pass -- one timeout, one restart at the wrong moment -- leaves a copy
// past its licence, and a cache that lapses on one bad request is worse than a
// slightly stale one. Three gives two retries inside every licence.
const refreshesPerTtl = 3

// minRefreshInterval floors the derived interval. Protects us, not the peer.
const minRefreshInterval = 30 * time.Second

// refreshInterval is how long to wait before the next pass.
//
// The SHORTEST licence any network granted governs the whole loop: a pass
// visits every network, so pacing it for the most permissive one would let the
// strictest one's data go stale between passes. The configured interval is an
// upper bound, never a licence to exceed what a network permitted.
func refreshInterval(configured time.Duration, targets []peerTarget) time.Duration {
	interval := configured
	for _, target := range targets {
		if target.ProjectionTtl <= 0 {
			// Not recorded -- an older registry record. The configured interval
			// is all we have; it is not evidence of a longer licence.
			continue
		}
		if derived := target.ProjectionTtl / refreshesPerTtl; derived < interval {
			interval = derived
		}
	}
	if interval < minRefreshInterval {
		// A network declaring a very short ttl must not turn the crawler into a
		// hot loop against its own discovery service.
		return minRefreshInterval
	}
	return interval
}

// catalogIDsOf reads the ids out of what a network returned.
//
// A catalog with no id is reported rather than guessed at: it is published
// under an id, and one we cannot read is one we cannot name again.
func catalogIDsOf(catalogs []json.RawMessage) (ids []string, unnamed int) {
	for _, document := range catalogs {
		var head struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(document, &head); err != nil || head.ID == "" {
			unnamed++
			continue
		}
		ids = append(ids, head.ID)
	}
	return ids, unnamed
}
