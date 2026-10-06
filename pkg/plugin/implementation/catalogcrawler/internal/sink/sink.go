package sink

// sink.go — DiscoverySink: crawlmanager.Sink backed by an HTTP push to a
// Discovery service. Batches the resolved catalog if it exceeds MaxDocBytes,
// pushes each batch, and rolls the outcomes up into one SinkOutcome.

import (
	"context"
	"fmt"
	"time"

	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
	"github.com/google/uuid"
)

// DiscoverySink pushes a resolved catalog's current content to a Discovery
// endpoint as one or more FULL-mode /push requests.
//
// UpdateMode is always FULL: unlike the catalog-crawler prototype's runner,
// crawlmanager never tracks an incremental Changeset (upserts/removals since
// a cursor) -- catalog.Resolve always folds a catalog's COMPLETE current
// content, so a FULL replace is the only mode that matches what SyncNext
// actually resolved. A batch after the first still omits offers (Discovery's
// existing MERGE semantics for the spillover batches of one push), even
// though it's still conceptually "the same full push" split across requests.
type DiscoverySink struct {
	Endpoint      string // Discovery's /push URL
	ParticipantID string // this deployment's bppId
	BppURI        string // this deployment's bppUri
	MaxDocBytes   int64  // 0 => no batching
	Client        *Client
	Now           func() time.Time // nil => time.Now

	// Action is the Beckn action the receiving route expects.
	//
	// Empty keeps "catalog/push", which is what the push route takes and what
	// every existing caller already sends. A deployment whose endpoint is a
	// different route sets this -- discovery-service's /publish takes
	// "catalog/publish" and refuses a mismatch with CTX_ACTION_MISMATCH,
	// because it looks the action up by what the BODY says.
	Action string

	// UpdateMode is the mode a catalog's lead batch is published with.
	//
	// Empty keeps FULL: the pushed document is the catalog's complete current
	// content, so a resource it omits is meant to be gone. MERGE is for a
	// caller whose source answers in pieces, where FULL would delete most of a
	// catalog on every pass.
	//
	// Only the LEAD batch uses it. A catalog too large for one push is split by
	// BatchCatalog into a lead and then MERGE batches regardless, so re-pushing
	// stays idempotent either way.
	UpdateMode string
}

// updateMode is the configured lead-batch mode, or FULL.
func (d *DiscoverySink) updateMode() string {
	if d.UpdateMode == "" {
		return UpdateModeFull
	}
	return d.UpdateMode
}

// NewDiscoverySink builds a DiscoverySink. timeout bounds each batch's push.
func NewDiscoverySink(endpoint, participantID, bppURI string, maxDocBytes int64, timeout time.Duration) *DiscoverySink {
	return &DiscoverySink{Endpoint: endpoint, ParticipantID: participantID, BppURI: bppURI, MaxDocBytes: maxDocBytes, Client: NewClient(timeout)}
}

func (d *DiscoverySink) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Send implements crawlmanager.Sink.
func (d *DiscoverySink) Send(ctx context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error) {
	batches, err := BatchCatalog(content, d.MaxDocBytes, d.updateMode())
	if err != nil {
		return crawlmanager.SinkOutcome{}, fmt.Errorf("catalogcrawler: batching %s: %w", entry.CatalogID, err)
	}

	var outcomes []BatchOutcome
	for _, batch := range batches {
		meta := PushMeta{
			ParticipantID: d.ParticipantID,
			BppURI:        d.BppURI,
			MessageID:     uuid.NewString(),
			TransactionID: uuid.NewString(),
			Timestamp:     d.now().UTC().Format(time.RFC3339),
			Action:        d.Action,
			UpdateMode:    batch.UpdateMode,
			CatalogType:   entry.CatalogType,
			VisibleTo:     entry.NetworkIDs,
			SchemaContext: entry.SchemaTypes,
		}
		body, err := BuildPushBody(meta, batch.Doc)
		if err != nil {
			return crawlmanager.SinkOutcome{}, fmt.Errorf("catalogcrawler: building push body for %s: %w", entry.CatalogID, err)
		}
		outcome, err := d.Client.Push(ctx, d.Endpoint, body)
		if err != nil {
			return crawlmanager.SinkOutcome{}, fmt.Errorf("catalogcrawler: pushing %s: %w", entry.CatalogID, err)
		}
		outcomes = append(outcomes, outcome)
	}

	accepted, reason := Rollup(outcomes)
	return crawlmanager.SinkOutcome{Accepted: accepted, Reason: reason}, nil
}
