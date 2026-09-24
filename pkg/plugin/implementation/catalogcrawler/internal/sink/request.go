// Package sink implements crawlmanager.Sink: an HTTP publish to the provider
// adapter's /publish, which signs and forwards to discovery. It is also the
// pipeline.Publisher the scheduled publish pipelines go through, so there is
// one way onto the network.
//
// Ported from the catalog-crawler prototype's own publish package
// (request.go/batch.go/client.go), adapted to crawlmanager's simpler
// Send(ctx, entry, content) contract. It first pushed to a Discovery
// service's /push with UpdateMode FULL -- crawlmanager always resolves a
// catalog's complete current content (via catalog.Resolve), which FULL
// matches. /publish rejects FULL as unsupported, so it now publishes MERGE.
package sink

import (
	"encoding/json"
	"fmt"
)

// Discovery /push update modes (beckn-discovr publishDirectives.updateMode).
const (
	UpdateModeFull  = "FULL"  // replace: resources absent from the pushed doc are deleted
	UpdateModeMerge = "MERGE" // id-keyed upserts + removals
)

// PushMeta carries everything that varies per push call. IDs and timestamp
// are injected so the builder stays pure and testable.
type PushMeta struct {
	ParticipantID string   // publisher identity (a domain) -> context.bppId
	BppURI        string   // publisher URI -> context.bppUri
	MessageID     string   // per-call uuid
	TransactionID string   // per-call uuid
	Timestamp     string   // RFC3339
	UpdateMode    string   // UpdateModeFull | UpdateModeMerge
	CatalogType   string   // from the index entry -> publishDirective.catalogType (required)
	VisibleTo     []string // catalog networks; nil/empty => public (omitted)
	// SchemaContext mirrors the index entry's own schemaTypes into
	// context.schemaContext -- an array of JSON-LD context URLs. Discovery's
	// own schema-type resolution checks this FIRST, before falling back to
	// each resource's own resourceAttributes["@type"]. Omitted when empty.
	SchemaContext []string
}

// BuildPushBody builds the /publish request body: a Beckn catalog/publish
// context plus a CatalogPublishAction message
// (message.catalogs, min 1) and a matching message.publishDirectives entry
// carrying catalogType, updateMode, and visibleTo.
func BuildPushBody(meta PushMeta, catalog []byte) ([]byte, error) {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(catalog, &head); err != nil {
		return nil, fmt.Errorf("catalogcrawler: reading catalog id: %w", err)
	}

	directive := map[string]any{
		"catalogId":   head.ID,
		"catalogType": meta.CatalogType,
		"updateMode":  meta.UpdateMode,
	}
	if len(meta.VisibleTo) > 0 {
		directive["visibleTo"] = meta.VisibleTo
	}
	// /publish reads a catalogue's schema types from its directive; the
	// context.schemaContext below is kept for anything still reading it there.
	if len(meta.SchemaContext) > 0 {
		directive["schemaTypes"] = meta.SchemaContext
	}

	context := map[string]any{
		"action":        "catalog/publish",
		"bppId":         meta.ParticipantID,
		"bppUri":        meta.BppURI,
		"messageId":     meta.MessageID,
		"transactionId": meta.TransactionID,
		"timestamp":     meta.Timestamp,
		"version":       "2.0.0",
	}
	if len(meta.SchemaContext) > 0 {
		context["schemaContext"] = meta.SchemaContext
	}

	body := map[string]any{
		"context": context,
		"message": map[string]any{
			"catalogs":          []json.RawMessage{json.RawMessage(catalog)},
			"publishDirectives": []any{directive},
		},
	}
	return json.Marshal(body)
}
