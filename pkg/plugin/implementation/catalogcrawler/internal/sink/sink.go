package sink

// sink.go — DiscoverySink: crawlmanager.Sink backed by an HTTP publish to the
// provider adapter's /publish. Batches the resolved catalog if it exceeds
// MaxDocBytes, publishes each batch, and rolls the outcomes up into one
// SinkOutcome. Publish is the same client for the scheduled publish pipelines.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"

	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
	"github.com/google/uuid"
)

// DiscoverySink publishes a resolved catalog's current content to the
// provider adapter's /publish as one or more MERGE-mode catalog/publish
// requests.
//
// UpdateMode is MERGE. catalog.Resolve folds a catalog's COMPLETE current
// content, which FULL would match, but /publish rejects FULL as unsupported,
// so every batch is a MERGE: a resource a source stops listing is no longer
// removed by a crawl. A batch after the first still omits offers.
type DiscoverySink struct {
	Endpoint      string // the provider adapter's /publish URL
	ParticipantID string // this deployment's bppId
	BppURI        string // this deployment's bppUri
	MaxDocBytes   int64  // 0 => no batching
	Client        *Client
	Now           func() time.Time // nil => time.Now
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
	batches, err := BatchCatalog(content, d.MaxDocBytes, UpdateModeMerge)
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

var _ pipeline.Publisher = (*DiscoverySink)(nil)

// Publish implements pipeline.Publisher: a pipeline-built catalog/publish body
// goes VERBATIM to baseURL's /publish through the same Client.Push the crawl
// path uses, and the batch outcome maps onto the pipeline's.
func (d *DiscoverySink) Publish(ctx context.Context, baseURL string, body []byte) pipeline.Outcome {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/publish"
	out, err := d.Client.Push(ctx, endpoint, body)
	switch {
	case err != nil:
		return pipeline.Outcome{Status: pipeline.StatusTransportError, Reason: err.Error()}
	case out.Acked:
		return pipeline.Outcome{Status: pipeline.StatusPublished}
	case out.HTTPStatus == 200:
		return pipeline.Outcome{Status: pipeline.StatusRejected, Reason: out.Reason}
	default:
		return pipeline.Outcome{Status: pipeline.StatusTransportError,
			Reason: fmt.Sprintf("HTTP %d: %s", out.HTTPStatus, out.Reason)}
	}
}
