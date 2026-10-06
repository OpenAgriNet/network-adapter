package catalogcrawler

// peerpublish.go — the publishing half of a cross-network crawl.
//
// The crawl stages what it fetched and queues it; this drains that queue. The
// split is what bounds memory: a crawl holds one page, the publisher holds one
// catalog, and a network with ten thousand catalogs costs no more than a
// network with ten.
//
// It is also what gives a failed publish the same treatment the local crawl
// gets. The content is already stored, so a retry republishes it rather than
// re-asking the network that owns it, and the backoff, park and abandon rules
// are the ones already in the queue.

import (
	"context"
	"fmt"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/store"
	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
)

// stagedStore is the queue half a cross-network crawl needs, declared at the
// consumer and satisfied implicitly -- the same shape projectionStore follows.
type stagedStore interface {
	StageAndEnqueue(ctx context.Context, staged store.StagedCatalog, discoverURL string) error
	ClaimNextStaged(ctx context.Context) (*store.ClaimedStaged, error)
	CompleteStaged(ctx context.Context, item *store.ClaimedStaged) error
	RescheduleStaged(ctx context.Context, id, claimID string, nextAttemptAt time.Time) error
}

// catalogSink is the publish half, declared at the consumer -- the same shape
// projectionStore and stagedStore follow. Satisfied by sink.DiscoverySink,
// which is also what the local crawl publishes through.
type catalogSink interface {
	Send(ctx context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error)
}

// publishStaged drains the staged queue until there is nothing ready.
//
// Mirrors the local crawl's own drain: claim, work, settle, repeat; stop on the
// first claim that returns nothing, rather than erroring in a tight loop
// against a store that is unwell.
func (p *peerCrawl) publishStaged(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		item, err := p.store.ClaimNextStaged(ctx)
		if err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: claiming a staged catalog failed", "error", err)
			return
		}
		if item == nil {
			return
		}
		p.publishOne(ctx, item)
	}
}

// publishOne sends one staged catalog and settles its queue row.
func (p *peerCrawl) publishOne(ctx context.Context, item *store.ClaimedStaged) {
	// A queue row whose content is gone -- a settled publish that crashed
	// between the two deletes, or a staging row removed by hand. Settled rather
	// than retried: there is nothing to send, and leaving it would have it
	// claimed again every pass for ever.
	if len(item.Document) == 0 {
		p.log.WarnContext(ctx, "catalogcrawler: queued catalog has no staged content; dropping",
			"networkId", item.NetworkID, "catalogId", item.CatalogID)
		if err := p.store.CompleteStaged(ctx, item); err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: settling an empty staged catalog failed", "error", err)
		}
		return
	}

	if err := p.sendStaged(ctx, item); err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: publishing a crawled catalog failed",
			"networkId", item.NetworkID, "catalogId", item.CatalogID, "error", err)
		// Rescheduled, not parked. A publish fails for reasons that pass --
		// discovery restarting, a connection reset -- and the content is still
		// staged, so the retry costs nothing but the push. The park sweep is
		// what eventually gives up on a row that keeps coming back.
		if err := p.store.RescheduleStaged(ctx, item.ID, item.ClaimID,
			time.Now().Add(p.retryDelay())); err != nil {
			p.log.ErrorContext(ctx, "catalogcrawler: rescheduling a staged catalog failed", "error", err)
		}
		return
	}

	if err := p.store.CompleteStaged(ctx, item); err != nil {
		p.log.ErrorContext(ctx, "catalogcrawler: settling a published catalog failed",
			"networkId", item.NetworkID, "catalogId", item.CatalogID, "error", err)
	}
}

// sendStaged publishes one catalog through the SAME sink the local crawl uses.
//
// Nothing here knows about batching, size ceilings or push bodies: Send splits
// a catalog larger than the budget, stamps this deployment's identity, and
// reports what discovery did with it. The only thing this path supplies that
// the local one does not is the entry, because a discover answers with catalogs
// and not with the index entry they came from.
func (p *peerCrawl) sendStaged(ctx context.Context, item *store.ClaimedStaged) error {
	// A synthetic entry carrying the two fields Send reads off one. Everything
	// else on a CatalogEntry describes an index -- baseline files, versions,
	// signatures -- and a crawled catalog has no index behind it.
	entry := catalog.CatalogEntry{
		CatalogID: item.CatalogID,
		// Upper case: the enum is ["MASTER","REGULAR"] and the comparison is
		// exact. A crawled catalog is never a master -- a master is a
		// deployment's own shared definition, not something mirrored.
		CatalogType: "REGULAR",
		// What becomes visibleTo. The audience the discover ASKED for, because
		// a discover response carries no visibleTo of its own: it is a publish
		// directive and is never returned.
		NetworkIDs: []string{item.Audience},
	}

	outcome, err := p.sink.Send(ctx, entry, item.Document)
	if err != nil {
		return fmt.Errorf("publishing %s: %w", item.CatalogID, err)
	}
	if !outcome.Accepted {
		return fmt.Errorf("discovery refused %s: %s", item.CatalogID, outcome.Reason)
	}
	return nil
}

// retryDelay is how long a failed publish waits. Fixed rather than growing:
// the queue's own park sweep is what bounds a row that keeps failing, so a
// backoff here would only slow the common case of a brief outage.
func (p *peerCrawl) retryDelay() time.Duration {
	if p.publishRetry > 0 {
		return p.publishRetry
	}
	return defaultPublishRetry
}
