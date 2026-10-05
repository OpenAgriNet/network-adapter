package catalogcrawler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/store"
	"github.com/google/uuid"
)

// This file is the half of peer crawling that takes things AWAY.
//
// Crawling adds; a projection is a licence, and a licence ends. Three distinct
// things end one, and they are deliberately separate here because the evidence
// for each is different:
//
//   - the peer stopped publishing it -- a successful crawl that omits it
//   - the licence lapsed          -- the ttl ran out, no crawl needed
//   - the peer was suspended      -- our decision, and the peer is never asked
//
// Only the second is a timer. The first and third are facts we already have,
// and waiting out a ttl to act on them would mean knowingly serving something
// we know we should not.

// projectionStore is the slice of the crawler's store that peer projections
// need, narrowed so this can be tested without a database.
type projectionStore interface {
	RecordProjections(ctx context.Context, networkID string, catalogIDs []string, expiresAt time.Time) error
	ProjectionsFor(ctx context.Context, networkID string) ([]string, error)
	ExpiredProjections(ctx context.Context, asOf time.Time) ([]store.Projection, error)
	ProjectedNetworks(ctx context.Context) ([]string, error)
	ForgetProjection(ctx context.Context, networkID, catalogID string) error
}

// catalogIDsOf reads the ids out of what a peer returned.
//
// A catalog with no id is skipped rather than failing the pass: we cannot
// record it, so we could never purge it, and publishing something unpurgeable
// is worse than dropping it. It is logged where it is called.
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

// missingFrom returns the held ids that the fresh list does not contain.
func missingFrom(held, fresh []string) []string {
	present := make(map[string]struct{}, len(fresh))
	for _, id := range fresh {
		present[id] = struct{}{}
	}
	var gone []string
	for _, id := range held {
		if _, ok := present[id]; !ok {
			gone = append(gone, id)
		}
	}
	return gone
}

// purge stops a projected catalog being served, and forgets it.
//
// HOW it stops being served: a FULL publish carrying the catalog id and nothing
// else. FULL means "resources the payload omits are deleted", and every discover
// retriever selects from resources with `AND r.active` -- so a catalog with no
// resources matches nothing, in every retrieval mode. isActive:false is set too,
// so the catalog row itself is inactive and not merely empty.
//
// Discovery-service has no delete endpoint. This is not a workaround for one; it
// is the documented way a publisher withdraws, used by the only party entitled
// to withdraw this catalog on the peer's behalf.
func (p *peerCrawl) purge(ctx context.Context, networkID, catalogID string) error {
	withdrawn, err := json.Marshal(map[string]any{
		"id":       catalogID,
		"isActive": false,
	})
	if err != nil {
		return err
	}

	meta := sink.PushMeta{
		ParticipantID: p.subscriberID,
		MessageID:     uuid.NewString(),
		TransactionID: uuid.NewString(),
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		Action:        "catalog/publish",
		UpdateMode:    sink.UpdateModeFull,
		CatalogType:   "REGULAR",
		VisibleTo:     []string{p.localNetwork},
	}
	body, err := sink.BuildPushBody(meta, withdrawn)
	if err != nil {
		return fmt.Errorf("build withdrawal for %s/%s: %w", networkID, catalogID, err)
	}
	if _, err := p.push.Push(ctx, p.pushEndpoint, body); err != nil {
		return fmt.Errorf("withdraw %s/%s: %w", networkID, catalogID, err)
	}
	// Forgotten only after the withdrawal succeeded. A row dropped first would
	// leave a catalog served with nothing left that could name it again.
	return p.projections.ForgetProjection(ctx, networkID, catalogID)
}

// purgeAll withdraws a list, and reports how many it managed.
//
// A failure on one is logged and the rest continue: an unreachable discovery
// service must not mean that the one catalog it choked on blocks every other
// peer's expiry. The rows it failed on stay, so the next sweep retries.
func (p *peerCrawl) purgeAll(ctx context.Context, networkID string, catalogIDs []string, why string) int {
	purged := 0
	for _, catalogID := range catalogIDs {
		if err := p.purge(ctx, networkID, catalogID); err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: withdrawing a peer projection failed",
				"networkId", networkID, "catalogId", catalogID, "reason", why, "error", err)
			continue
		}
		p.log.InfoContext(ctx, "catalogcrawler: peer projection withdrawn",
			"networkId", networkID, "catalogId", catalogID, "reason", why)
		purged++
	}
	return purged
}

// purgeExpired withdraws every projection whose licence has lapsed.
//
// The obligation that needs no cooperation from anyone: a peer that has been
// unreachable for longer than its ttl stops being served, because the ttl ran
// out and not because anything told us to.
func (p *peerCrawl) purgeExpired(ctx context.Context, asOf time.Time) {
	expired, err := p.projections.ExpiredProjections(ctx, asOf)
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: listing expired peer projections", "error", err)
		return
	}
	for _, projection := range expired {
		p.purgeAll(ctx, projection.NetworkID, []string{projection.CatalogID}, "projection ttl expired")
	}
}

// purgeUnadmitted withdraws everything held from a network the registry no
// longer admits.
//
// Suspension is a decision that we have stopped trusting a network. Leaving its
// catalogue in our cache would mean it keeps answering our consumers through us
// for the rest of its ttl, which is the opposite of what suspending it meant.
// The peer is never contacted: it has no part in a decision about it.
func (p *peerCrawl) purgeUnadmitted(ctx context.Context, admitted []peerTarget) {
	held, err := p.projections.ProjectedNetworks(ctx)
	if err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: listing projected networks", "error", err)
		return
	}
	stillAdmitted := make(map[string]struct{}, len(admitted))
	for _, target := range admitted {
		stillAdmitted[target.NetworkID] = struct{}{}
	}

	for _, networkID := range held {
		if _, ok := stillAdmitted[networkID]; ok {
			continue
		}
		catalogIDs, err := p.projections.ProjectionsFor(ctx, networkID)
		if err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: listing projections of a withdrawn peer",
				"networkId", networkID, "error", err)
			continue
		}
		p.purgeAll(ctx, networkID, catalogIDs, "peer is no longer admitted")
	}
}

// refreshesPerTtl is how many refresh attempts a projection gets before it
// lapses.
//
// More than one, deliberately. Refreshing exactly at the ttl means a single
// failed pass -- one timeout, one restart at the wrong moment -- is an expiry,
// and a cache that empties on one bad request is worse than a slightly stale
// one. Three gives two retries inside every licence.
const refreshesPerTtl = 3

// refreshInterval is how long to wait before the next pass.
//
// The SHORTEST licence any peer granted governs the whole loop: a pass visits
// every peer, so pacing it for the most permissive one would let the strictest
// peer's data lapse between passes. The configured interval is an upper bound,
// never a licence to exceed what a peer permitted.
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
		// A peer declaring a very short ttl must not turn the crawler into a
		// hot loop against its own discovery service. The floor means such a
		// peer's data does lapse between passes -- correct, and visible, rather
		// than served stale.
		return minRefreshInterval
	}
	return interval
}

// minRefreshInterval floors the derived interval. Protects us, not the peer.
const minRefreshInterval = 30 * time.Second
