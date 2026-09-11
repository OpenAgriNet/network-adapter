package main

// Posting built catalogs to the provider adapter's own /publish module, which
// signs the request and forwards it to the network adapter and thence to the
// discovery service.
//
// NOT /catalog/publish. That is the decentralized-catalog path: it writes to a
// blob store a crawler walks, and never reaches the discovery service, so a
// catalog published there answers no discover.
//
// The request is unsigned. The caller is the provider's own catalogue system,
// inside its trust boundary -- the module signs the forwarded request as
// itself. Neither endpoint belongs on a network-facing address.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The catalog whose single India-wide polygon resource this work replaces.
//
// That resource matches every S_DWITHIN at any radius -- measured: a discover
// 300 km from anything still returns it -- so it must go, or every proximity
// answer carries a resource that says only "somewhere in India".
const oldCatalogID = "cat-agmarknet-mandi-prices"

// Per-catalog outcomes.
const (
	StatusPublished      = "published"
	StatusDryRun         = "dry-run"
	StatusRejected       = "rejected"
	StatusTransportError = "transport-error"
)

// publishTimeout is generous: a state catalog runs to hundreds of KB and the
// adapter signs, forwards and indexes it before answering.
const publishTimeout = 180 * time.Second

// publishConfig is one publish run's inputs.
type publishConfig struct {
	publishURL string
	catalogIn  string
	states     []string
	dryRun     bool
	retireOld  bool
}

// PublishOutcome is what happened to one catalog.
type PublishOutcome struct {
	StateCode string
	CatalogID string
	Status    string
	Reason    string
}

// PublishResult is the whole run.
type PublishResult struct {
	Outcomes   []PublishOutcome
	RetiredOld *PublishOutcome
}

// HasFailures reports whether anything did not reach the index intact, so main
// can exit non-zero. A PARTIAL counts: see publishOne.
func (r PublishResult) HasFailures() bool {
	for _, outcome := range r.Outcomes {
		if outcome.Status == StatusRejected || outcome.Status == StatusTransportError {
			return true
		}
	}
	if r.RetiredOld != nil {
		return r.RetiredOld.Status == StatusRejected || r.RetiredOld.Status == StatusTransportError
	}
	return false
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

// onPublishEnvelope is the answer.
type onPublishEnvelope struct {
	Message struct {
		Results []onPublishResult `json:"results"`
	} `json:"message"`
}

// publish posts every catalog file in cfg.catalogIn, one at a time.
//
// Sequential and per-catalog on purpose. A failed state is one retryable
// catalog, and one bad state must not discard the outcomes of the others --
// which is why a transport failure lands in an outcome rather than returning
// an error.
func publish(ctx context.Context, cfg publishConfig) (PublishResult, error) {
	result := PublishResult{}

	if strings.TrimSpace(cfg.publishURL) == "" {
		return result, errors.New(
			"no publish address: pass --publish-url or set MANDI_PUBLISH_URL")
	}
	base := strings.TrimRight(cfg.publishURL, "/") + "/publish"

	files, err := catalogFiles(cfg.catalogIn, cfg.states)
	if err != nil {
		return result, err
	}
	// An empty directory must not look like a successful run: silently
	// publishing nothing is indistinguishable from publishing everything.
	if len(files) == 0 && !cfg.retireOld {
		return result, fmt.Errorf("no catalog files in %s", cfg.catalogIn)
	}

	client := &http.Client{Timeout: publishTimeout}

	for _, file := range files {
		result.Outcomes = append(result.Outcomes, publishFile(ctx, client, base, file, cfg.dryRun))
	}

	if cfg.retireOld {
		outcome := publishEnvelope(ctx, client, base, "", oldCatalogID,
			tombstone(oldCatalogID), cfg.dryRun)
		result.RetiredOld = &outcome
	}
	return result, nil
}

// catalogFile pairs a catalog with the state it belongs to.
//
// slug and stateCode differ for a split state: mandi-TN-2.json has slug TN-2
// and state TN. The state is what a --states filter matches, so filtering a
// chunked state posts all of its chunks rather than none.
type catalogFile struct {
	slug      string
	stateCode string
	path      string
}

// chunkSuffix matches the -N a split state's catalog carries.
var chunkSuffix = regexp.MustCompile(`-[0-9]+$`)

// stateOf strips a chunk suffix, so TN-2 is a catalog of TN.
func stateOf(slug string) string {
	return chunkSuffix.ReplaceAllString(slug, "")
}

// catalogFiles lists mandi-<STATE>.json in dir, filtered and sorted, so a run
// reads the same way twice.
func catalogFiles(dir string, states []string) ([]catalogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read catalog directory: %w", err)
	}

	wanted := make(map[string]bool, len(states))
	for _, state := range states {
		wanted[state] = true
	}

	var files []catalogFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "mandi-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(name, "mandi-"), ".json")
		stateCode := stateOf(slug)
		if len(wanted) > 0 && !wanted[stateCode] {
			continue
		}
		files = append(files, catalogFile{slug: slug, stateCode: stateCode, path: filepath.Join(dir, name)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].slug < files[j].slug })
	return files, nil
}

// publishFile reads one built catalog and posts it verbatim.
//
// Verbatim matters: the file is what a human reviewed, so re-encoding it here
// would publish something nobody read.
func publishFile(ctx context.Context, client *http.Client, url string,
	file catalogFile, dryRun bool) PublishOutcome {
	body, err := os.ReadFile(file.path)
	if err != nil {
		return PublishOutcome{StateCode: file.stateCode, Status: StatusTransportError,
			Reason: fmt.Sprintf("read %s: %v", file.path, err)}
	}
	return publishEnvelope(ctx, client, url, file.stateCode, catalogIDOf(body), body, dryRun)
}

// catalogIDOf reads the catalog's own id out of the payload, so the reported
// id is the one that was actually sent rather than one rebuilt from the
// filename.
func catalogIDOf(body []byte) string {
	var envelope struct {
		Message struct {
			Catalogs []struct {
				ID string `json:"id"`
			} `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Message.Catalogs) > 0 {
		return envelope.Message.Catalogs[0].ID
	}
	return ""
}

// publishEnvelope posts one body and judges the answer.
func publishEnvelope(ctx context.Context, client *http.Client, url, stateCode, catalogID string,
	body []byte, dryRun bool) PublishOutcome {
	outcome := PublishOutcome{StateCode: stateCode, CatalogID: catalogID}

	if dryRun {
		outcome.Status = StatusDryRun
		return outcome
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		outcome.Status, outcome.Reason = StatusTransportError, err.Error()
		return outcome
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		outcome.Status, outcome.Reason = StatusTransportError, err.Error()
		return outcome
	}
	defer func() { _ = response.Body.Close() }()

	answer := make([]byte, 0)
	buffer := bytes.NewBuffer(answer)
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		outcome.Status, outcome.Reason = StatusTransportError, err.Error()
		return outcome
	}
	answer = buffer.Bytes()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		// The adapter's own error code is the only thing that says WHY, so it
		// is carried through rather than flattened to the HTTP status.
		outcome.Status = StatusTransportError
		outcome.Reason = fmt.Sprintf("HTTP %d: %s", response.StatusCode, firstLine(answer))
		return outcome
	}

	var envelope onPublishEnvelope
	if err := json.Unmarshal(answer, &envelope); err != nil {
		outcome.Status = StatusTransportError
		outcome.Reason = fmt.Sprintf("answer could not be read: %v", err)
		return outcome
	}
	if len(envelope.Message.Results) == 0 {
		outcome.Status = StatusRejected
		outcome.Reason = "the adapter answered with no results"
		return outcome
	}

	first := envelope.Message.Results[0]
	if first.CatalogID != "" {
		outcome.CatalogID = first.CatalogID
	}

	switch strings.ToUpper(first.Status) {
	case "ACCEPTED":
		outcome.Status = StatusPublished
	default:
		// Anything else is a failure, PARTIAL included. A PARTIAL means the
		// catalog was indexed with pieces missing -- measured against the
		// running service, 273 markets published as 128 findable ones,
		// because discovery caps a catalog at 256 geometries. Reporting that
		// as success would hide every market a proximity search cannot find.
		outcome.Status = StatusRejected
		outcome.Reason = describeFailure(first)
	}
	return outcome
}

// describeFailure builds a reason that names the status, how much was lost,
// and the first thing the service actually complained about.
func describeFailure(result onPublishResult) string {
	parts := []string{strings.ToUpper(result.Status)}
	if result.Reason != "" {
		parts = append(parts, result.Reason)
	}
	if len(result.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("%d errors, first: %s",
			len(result.Errors), result.Errors[0].Message))
	}
	return strings.Join(parts, ": ")
}

// firstLine trims an answer down to something a summary line can carry.
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

// tombstone is a publish body that deactivates a whole catalog.
//
// Deactivating the catalog is how its resources go away. updateMode FULL is
// rejected as unsupported, and MERGE's removal semantics are documented
// nowhere in these repos, so republishing the catalog WITHOUT the unwanted
// resource cannot be relied on to remove it.
func tombstone(catalogID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"action":        "catalog/publish",
			"version":       "2.0.0",
			"transactionId": uuid.NewString(),
			"messageId":     uuid.NewString(),
			"timestamp":     time.Now().UTC().Format(time.RFC3339),
		},
		"message": map[string]any{
			"catalogs": []any{map[string]any{
				"id":       catalogID,
				"isActive": false,
				"descriptor": map[string]any{
					"code": catalogID,
					"name": "Retired: superseded by the per-state market catalogs",
				},
				"resources": []any{},
			}},
			"publishDirectives": []any{map[string]any{
				"catalogId":   catalogID,
				"catalogType": "REGULAR",
				"updateMode":  "MERGE",
			}},
		},
	})
	return body
}
