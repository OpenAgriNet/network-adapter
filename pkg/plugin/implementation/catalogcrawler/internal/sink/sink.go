// Package sink is the ONE piece of code that puts a catalogue on the network:
// it posts to the provider adapter's /publish and judges the answer.
//
// Two callers, one path:
//
//   - crawled catalogues, through Send (crawlmanager.Sink): each is wrapped
//     in a catalog/publish envelope here;
//   - the scheduled publish pipelines, through Publish (pipeline.Publisher):
//     their built files are already envelopes and go verbatim.
//
// Only ACCEPTED counts as published. PARTIAL means the catalogue indexed with
// resources missing -- 273 markets published as 128 findable ones, because
// discovery caps a catalogue at 256 geometries -- and is a rejection.
//
// It used to push straight to discovery's /push as catalog/push with
// updateMode FULL. /publish rejects FULL as unsupported, so every request is
// MERGE: resources a source stops listing are no longer removed by a crawl.
// A source that needs something gone deactivates its catalogue instead.
package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
	"github.com/beckn/catalog-core/pkg/catalog"
	"github.com/beckn/catalog-core/pkg/catalog/crawlmanager"
	"github.com/google/uuid"
)

// updateModeMerge is the only update mode /publish accepts; FULL is rejected
// as unsupported.
const updateModeMerge = "MERGE"

// maxAnswerBytes bounds what is read of an answer: enough for any real
// on_publish, not enough for a misbehaving server to exhaust memory.
const maxAnswerBytes = 1 << 20

var _ pipeline.Publisher = (*PublishSink)(nil)

// PublishSink publishes crawled catalogues to a provider adapter.
type PublishSink struct {
	PublishURL  string // the provider adapter's base address; /publish is appended
	MaxDocBytes int64  // 0 => one request per catalogue, however large
	Client      *http.Client
}

// NewPublishSink builds a PublishSink. timeout bounds each request.
func NewPublishSink(publishURL string, maxDocBytes int64, timeout time.Duration) *PublishSink {
	return &PublishSink{PublishURL: publishURL, MaxDocBytes: maxDocBytes, Client: &http.Client{Timeout: timeout}}
}

// Send implements crawlmanager.Sink.
//
// A catalogue over MaxDocBytes goes as several requests, each a slice of its
// resources; offers ride with the first only. It is accepted only if every
// request was, and a rejection carries every failed request's reason.
func (s *PublishSink) Send(ctx context.Context, entry catalog.CatalogEntry, content []byte) (crawlmanager.SinkOutcome, error) {
	docs, err := splitCatalogue(content, s.MaxDocBytes)
	if err != nil {
		return crawlmanager.SinkOutcome{}, fmt.Errorf("catalogcrawler: splitting %s: %w", entry.CatalogID, err)
	}

	var failures []string
	for _, doc := range docs {
		body, err := envelope(doc, entry)
		if err != nil {
			return crawlmanager.SinkOutcome{}, fmt.Errorf("catalogcrawler: building the publish request for %s: %w", entry.CatalogID, err)
		}
		outcome := s.Publish(ctx, s.PublishURL, body)
		if outcome.Status != pipeline.StatusPublished {
			failures = append(failures, outcome.Status+": "+outcome.Reason)
		}
	}

	if len(failures) > 0 {
		return crawlmanager.SinkOutcome{Accepted: false, Reason: strings.Join(failures, "; ")}, nil
	}
	return crawlmanager.SinkOutcome{Accepted: true}, nil
}

// splitCatalogue cuts a catalogue into documents that each fit maxDocBytes by
// serialized size. One that already fits is returned as is. A larger one is
// a lead document carrying the offers, then the remaining resources without
// them. A single resource over the budget still forms its own document -- it
// cannot be split further -- and the far end's rejection says so.
func splitCatalogue(catalogue []byte, maxDocBytes int64) ([][]byte, error) {
	if maxDocBytes <= 0 || int64(len(catalogue)) <= maxDocBytes {
		return [][]byte{catalogue}, nil
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(catalogue, &doc); err != nil {
		return nil, fmt.Errorf("reading the catalogue: %w", err)
	}
	var resources []json.RawMessage
	if raw := doc["resources"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &resources); err != nil {
			return nil, fmt.Errorf("reading resources: %w", err)
		}
	}
	if len(resources) == 0 {
		return [][]byte{catalogue}, nil
	}

	var docs [][]byte
	for i, first := 0, true; i < len(resources); first = false {
		part := make(map[string]json.RawMessage, len(doc))
		for key, value := range doc {
			if key != "resources" && (key != "offers" || first) {
				part[key] = value
			}
		}
		scaffold, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		budget := maxDocBytes - int64(len(scaffold))

		start, used := i, int64(0)
		for i < len(resources) {
			cost := int64(len(resources[i])) + 1 // + separator
			if i > start && used+cost > budget {
				break // always at least one resource per document
			}
			used += cost
			i++
		}

		slice, err := json.Marshal(resources[start:i])
		if err != nil {
			return nil, err
		}
		part["resources"] = slice
		encoded, err := json.Marshal(part)
		if err != nil {
			return nil, err
		}
		docs = append(docs, encoded)
	}
	return docs, nil
}

// envelope wraps one crawled catalogue document in the catalog/publish
// request /publish takes, with the directive its index entry declares. The
// document goes in verbatim: it is what the source published.
func envelope(doc []byte, entry catalog.CatalogEntry) ([]byte, error) {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(doc, &head); err != nil {
		return nil, fmt.Errorf("reading catalogue id: %w", err)
	}
	if head.ID == "" {
		return nil, fmt.Errorf("the catalogue carries no id, so it cannot be published")
	}

	directive := map[string]any{
		"catalogId":   head.ID,
		"catalogType": entry.CatalogType,
		"updateMode":  updateModeMerge,
	}
	if len(entry.NetworkIDs) > 0 {
		directive["visibleTo"] = entry.NetworkIDs
	}
	if len(entry.SchemaTypes) > 0 {
		directive["schemaTypes"] = entry.SchemaTypes
	}

	return json.Marshal(map[string]any{
		"context": map[string]any{
			"action":        "catalog/publish",
			"version":       "2.0.0",
			"transactionId": uuid.NewString(),
			"messageId":     uuid.NewString(),
			"timestamp":     time.Now().UTC().Format(time.RFC3339),
		},
		"message": map[string]any{
			"catalogs":          []json.RawMessage{json.RawMessage(doc)},
			"publishDirectives": []any{directive},
		},
	})
}

// onPublishResult is one entry of the adapter's answer.
type onPublishResult struct {
	CatalogID string `json:"catalogId"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Errors    []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Publish implements pipeline.Publisher: it posts one catalog/publish body to
// baseURL's /publish and judges the answer.
//
// A transport failure is an Outcome, not an error, so one bad catalogue never
// hides the outcomes of the others. The request is unsigned: the provider
// adapter's /publish module signs the forwarded request as itself, which is
// why this address never belongs on a network-facing interface.
func (s *PublishSink) Publish(ctx context.Context, baseURL string, body []byte) pipeline.Outcome {
	var outcome pipeline.Outcome
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/publish"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		outcome.Status, outcome.Reason = pipeline.StatusTransportError, err.Error()
		return outcome
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := s.Client.Do(request)
	if err != nil {
		outcome.Status, outcome.Reason = pipeline.StatusTransportError, err.Error()
		return outcome
	}
	defer func() { _ = response.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes))
	if err != nil {
		outcome.Status, outcome.Reason = pipeline.StatusTransportError, err.Error()
		return outcome
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// The adapter's own error text is the only thing that says WHY, so it
		// is carried through rather than flattened to the HTTP status.
		outcome.Status = pipeline.StatusTransportError
		outcome.Reason = fmt.Sprintf("HTTP %d: %s", response.StatusCode, firstLine(answer))
		return outcome
	}

	var envelope struct {
		Message struct {
			Results []onPublishResult `json:"results"`
		} `json:"message"`
	}
	if err := json.Unmarshal(answer, &envelope); err != nil {
		outcome.Status = pipeline.StatusTransportError
		outcome.Reason = fmt.Sprintf("answer could not be read: %v", err)
		return outcome
	}
	if len(envelope.Message.Results) == 0 {
		outcome.Status, outcome.Reason = pipeline.StatusRejected, "the adapter answered with no results"
		return outcome
	}

	first := envelope.Message.Results[0]
	outcome.CatalogID = first.CatalogID
	if strings.EqualFold(first.Status, "ACCEPTED") {
		outcome.Status = pipeline.StatusPublished
		return outcome
	}
	// Anything else is a failure, PARTIAL included -- see the package doc.
	outcome.Status = pipeline.StatusRejected
	outcome.Reason = describeFailure(first)
	return outcome
}

// describeFailure names the status, the reason, and the first thing the
// service actually complained about.
func describeFailure(result onPublishResult) string {
	parts := []string{strings.ToUpper(result.Status)}
	if result.Reason != "" {
		parts = append(parts, result.Reason)
	}
	if len(result.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("%d errors, first: %s", len(result.Errors), result.Errors[0].Message))
	}
	return strings.Join(parts, ": ")
}

// firstLine trims an answer down to something a log line can carry.
func firstLine(body []byte) string {
	text := strings.TrimSpace(string(body))
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	const limit = 300
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}
