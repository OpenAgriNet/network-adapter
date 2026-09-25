package pipeline

// The pipeline's publish step: which built files go out, in what order, under
// what safety rule, and what counts as success. It does NOT make the HTTP
// call. That is a Publisher's -- in production the crawler's sink
// (catalogcrawler/internal/sink), the one piece of code that posts to the
// provider adapter's /publish, for crawled catalogues and pipelines alike.
// Keeping the call out of here is what lets this package stay free of the
// crawler while the crawler owns the network.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Per-catalogue outcomes.
const (
	StatusPublished      = "published"
	StatusRejected       = "rejected"
	StatusTransportError = "transport-error"
)

// Outcome is what happened to one catalogue.
type Outcome struct {
	StateCode string
	CatalogID string
	Status    string
	Reason    string
}

// Result is one publish step.
type Result struct {
	Outcomes   []Outcome
	RetiredOld *Outcome

	// RetiredSkipped says why a declared retirement did NOT happen. Without
	// it, a skipped retirement and a pipeline that never asked for one look
	// identical in the report.
	RetiredSkipped string
}

// HasFailures reports whether anything did not reach the index intact, so a
// caller can exit non-zero. A PARTIAL counts: a Publisher reports it as
// StatusRejected.
func (r Result) HasFailures() bool {
	for _, outcome := range r.Outcomes {
		if outcome.Status != StatusPublished {
			return true
		}
	}
	return r.RetiredOld != nil && r.RetiredOld.Status != StatusPublished
}

// Publisher posts one catalog/publish body to the provider adapter whose
// base address is baseURL, and judges the answer: ACCEPTED is
// StatusPublished, anything else -- PARTIAL included -- is StatusRejected,
// and a failure to reach it is StatusTransportError. A transport failure is
// an Outcome, not an error, so one bad catalogue never hides the others.
type Publisher interface {
	Publish(ctx context.Context, baseURL string, body []byte) Outcome
}

// publishAddressHint is the fallback when a caller has not said how THIS
// pipeline's address is supplied. Generic on purpose: naming one pipeline's
// variable here once told every other pipeline to set it.
const publishAddressHint = "set inputs.publishUrl"

// publishAddressHintFor names the ways an operator can supply a pipeline's
// publish address -- its own flag and env, as the file declares them -- so an
// empty one says what to set rather than only that something is missing.
func publishAddressHintFor(input Input) string {
	var ways []string
	if input.Flag != "" {
		ways = append(ways, "flag --"+input.Flag)
	}
	if input.Env != "" {
		ways = append(ways, input.Env)
	}
	if len(ways) == 0 {
		return publishAddressHint
	}
	return publishAddressHint + " (" + strings.Join(ways, " or ") + ")"
}

// refuseWhenShape is the one form of refusal this step understands:
//
//	collection.<counter> > 0
//
// <counter> is a name the pipeline's own steps record into, so a second
// pipeline refuses on its own terms without editing Go. Anything else is
// REJECTED rather than ignored -- a safety rule that is quietly dropped is
// worse than one never declared -- and a file needing a richer rule needs a
// real expression evaluator here first.
var refuseWhenShape = regexp.MustCompile(`^collection\.([A-Za-z][A-Za-z0-9_]*)\s*>\s*0$`)

// refuseWhenCounter returns the counter a refuseWhen rule names.
func refuseWhenCounter(rule string) (string, bool) {
	match := refuseWhenShape.FindStringSubmatch(strings.TrimSpace(rule))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// PublishCatalogues posts every catalogue file in catalogDir through
// publisher, unless the declared safety rule forbids it.
//
// stateErrors is how many parts of the collection failed. A part that failed
// to collect is not a part with nothing in it, so publishing then would
// replace a whole group's catalogue with a partial one, or with nothing.
//
// Sequential and per-catalogue on purpose: one failed catalogue is one
// retryable catalogue, and must not discard the outcomes of the others.
func PublishCatalogues(ctx context.Context, spec Publish, resolved map[string]string,
	catalogDir, filenamePrefix string, stateErrors int, publisher Publisher) (Result, error) {
	var result Result

	if publisher == nil {
		return result, fmt.Errorf("no publisher: this run was asked to publish but given nothing to publish with")
	}
	if err := checkJudgement(spec); err != nil {
		return result, err
	}

	if rule := strings.TrimSpace(spec.RefuseWhen); rule != "" {
		counter, ok := refuseWhenCounter(rule)
		if !ok {
			return result, fmt.Errorf(
				"publish.refuseWhen is %q; this step understands only `collection.<counter> > 0` "+
					"and will not guess at another rule", rule)
		}
		if stateErrors > 0 {
			return result, fmt.Errorf(
				"refusing to publish: %d of the collection failed (%s), and a partial collection "+
					"will not be published as though it were whole (publish.refuseWhen: %s)",
				stateErrors, counter, rule)
		}
	}

	// The YAML's publish.url is ${inputs.publishUrl}/publish, but a Publisher
	// is given the BASE address and appends /publish itself. Passing the
	// rendered url would post to /publish/publish, which fails as a 404 far
	// from here and reads like an unreachable adapter rather than a bug.
	if err := checkPublishURL(spec); err != nil {
		return result, err
	}

	hint := spec.AddressHint
	if hint == "" {
		hint = publishAddressHint
	}
	publishURL := strings.TrimSpace(resolved["publishUrl"])
	if publishURL == "" {
		return result, fmt.Errorf("no publish address: %s", hint)
	}

	retire, err := retirement(spec.RetireOld, resolved)
	if err != nil {
		return result, err
	}

	files, err := catalogueFiles(catalogDir, filenamePrefix)
	if err != nil {
		return result, err
	}
	// An empty directory must not look like a successful run: silently
	// publishing nothing is indistinguishable from publishing everything.
	if len(files) == 0 && retire == nil {
		return result, fmt.Errorf("no catalogue files in %s", catalogDir)
	}

	for _, file := range files {
		result.Outcomes = append(result.Outcomes, publishFile(ctx, publisher, publishURL, file))
	}
	// The tombstone goes out ONLY when everything that supersedes the old
	// catalogue is actually on the network.
	//
	// Retiring is not a tidy-up, it is a deletion: deactivating the old
	// catalogue is how its resources leave. Sending it after a run whose
	// replacements were rejected removes the old data and puts nothing in its
	// place, leaving the network with neither -- and the next tick repeats it.
	if retire != nil {
		if published, why := allPublished(result.Outcomes); !published {
			result.RetiredSkipped = why
		} else {
			outcome := publisher.Publish(ctx, publishURL, tombstone(retire.CatalogID, retire.DescriptorName))
			if outcome.CatalogID == "" {
				outcome.CatalogID = retire.CatalogID
			}
			result.RetiredOld = &outcome
		}
	}
	return result, nil
}

// catalogueFile pairs a built catalogue with the group it belongs to.
//
// slug and stateCode differ for a split group: <prefix>-TN-2.json has slug
// TN-2 and group TN.
type catalogueFile struct {
	slug      string
	stateCode string
	path      string
}

// chunkSuffix matches the -N a split group's catalogue carries.
var chunkSuffix = regexp.MustCompile(`-[0-9]+$`)

// catalogueFiles lists <prefix>-<slug>.json in dir, sorted, so a run reads the
// same way twice. The match is the one WriteCatalogues and
// RemoveStaleCatalogues make -- three places, one naming contract.
func catalogueFiles(dir, prefix string) ([]catalogueFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read catalogue directory: %w", err)
	}
	namePrefix := prefix + "-"
	var files []catalogueFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(name, namePrefix), ".json")
		files = append(files, catalogueFile{
			slug: slug, stateCode: chunkSuffix.ReplaceAllString(slug, ""), path: filepath.Join(dir, name),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].slug < files[j].slug })
	return files, nil
}

// publishFile reads one built catalogue and posts it VERBATIM: the file is
// what a human reviewed, so re-encoding it would publish something nobody
// read.
func publishFile(ctx context.Context, publisher Publisher, publishURL string, file catalogueFile) Outcome {
	body, err := os.ReadFile(file.path)
	if err != nil {
		return Outcome{StateCode: file.stateCode, Status: StatusTransportError,
			Reason: fmt.Sprintf("read %s: %v", file.path, err)}
	}
	outcome := publisher.Publish(ctx, publishURL, body)
	outcome.StateCode = file.stateCode
	if outcome.CatalogID == "" {
		outcome.CatalogID = catalogueIDOf(body)
	}
	return outcome
}

// catalogueIDOf reads the catalogue's own id out of a publish body, so a
// reported id is the one actually sent rather than one rebuilt from a name.
func catalogueIDOf(body []byte) string {
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

// tombstone is a publish body that deactivates a whole catalogue.
//
// Deactivating the catalogue is how its resources go away. updateMode FULL is
// rejected as unsupported, and MERGE's removal semantics are documented
// nowhere, so republishing the catalogue WITHOUT the unwanted resource cannot
// be relied on to remove it.
func tombstone(catalogID, retiredName string) []byte {
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
				"id":         catalogID,
				"isActive":   false,
				"descriptor": map[string]any{"code": catalogID, "name": retiredName},
				"resources":  []any{},
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

// expectedPublishURL is the only publish.url this step can honour, for the
// reason given where the address is resolved: the base goes to
// the Publisher, which appends the path itself.
const expectedPublishURL = "${inputs.publishUrl}/publish"

// checkPublishURL refuses a publish.url this step would ignore.
//
// The address actually used comes from inputs.publishUrl, not from this
// field. Without this check an operator could repoint publish.url at another
// host, watch the edit take no effect, and have the catalog posted to
// CATALOG_PUBLISH_URL anyway -- the same silent-divergence failure checkJudgement
// exists to prevent, and the reason refuseWhen is compared literally above.
func checkPublishURL(spec Publish) error {
	if url := strings.TrimSpace(spec.URL); url != expectedPublishURL {
		return fmt.Errorf(
			"publish.url is %q, but this step publishes to inputs.publishUrl and would ignore it; "+
				"only %q is honoured", url, expectedPublishURL)
	}
	return nil
}

// retirement resolves the declared retireOld block: nil when there is none
// or it is switched off, the block itself when it is on.
//
// A declared retireOld that never reached the publish step would be a rule
// the file states and nothing performs -- the same failure checkJudgement
// guards.
//
// Enabled is an `${inputs.*}` reference. One naming an input the file does not
// declare is refused rather than read as false: treating an unresolvable
// enable flag as "off" would silently skip the retirement, which is the
// outcome an operator who wrote the block was trying to avoid.
func retirement(spec RetireOld, resolved map[string]string) (*RetireOld, error) {
	enabled := strings.TrimSpace(spec.Enabled)
	if enabled == "" {
		return nil, nil // no retireOld block
	}

	value, ok := resolved[strings.TrimSuffix(strings.TrimPrefix(enabled, "${inputs."), "}")]
	if !ok {
		return nil, fmt.Errorf(
			"publish.retireOld.enabled is %q, but no such input is declared, so the retirement "+
				"cannot be turned on or off; declare the input or remove the block", enabled)
	}
	if !strings.EqualFold(value, "true") {
		return nil, nil
	}
	if strings.TrimSpace(spec.CatalogID) == "" {
		return nil, fmt.Errorf("publish.retireOld is enabled but names no catalogId to retire")
	}
	return &spec, nil
}

// checkJudgement verifies the spec's declared verdicts are the ones a
// Publisher actually applies: ACCEPTED is the only success, and anything
// else -- PARTIAL included -- is a failure.
//
// The judgement is not re-implemented here, so a pipeline declaring something
// different would not change behaviour, it would only make the YAML lie about
// it. A PARTIAL that read as success is precisely the failure this guards:
// the catalog indexes with resources missing (273 markets published as 128
// findable ones), and nothing downstream would say so.
func checkJudgement(spec Publish) error {
	const accepted = "ACCEPTED"

	if len(spec.Accept) != 1 || !strings.EqualFold(strings.TrimSpace(spec.Accept[0]), accepted) {
		return fmt.Errorf(
			"publish.accept is %v, but publishing treats %s as the only success; "+
				"this step cannot honour any other rule",
			spec.Accept, accepted)
	}

	// PARTIAL must be declared a failure where the spec lists failures at all:
	// a treatAsFailure that omits it reads as though PARTIAL were tolerated.
	failsOnPartial := false
	for _, status := range spec.TreatAsFailure {
		status = strings.TrimSpace(status)
		if strings.EqualFold(status, accepted) {
			return fmt.Errorf("publish.treatAsFailure lists %s, which publishing treats as success", accepted)
		}
		if strings.EqualFold(status, "PARTIAL") {
			failsOnPartial = true
		}
	}
	if len(spec.TreatAsFailure) > 0 && !failsOnPartial {
		return fmt.Errorf(
			"publish.treatAsFailure is %v and omits PARTIAL, but publishing counts a PARTIAL as a failure: "+
				"a PARTIAL means the catalog indexed with resources missing",
			spec.TreatAsFailure)
	}
	return nil
}

// allPublished reports whether every catalogue this run built actually
// reached the network, and says what stopped it when one did not.
//
// A run that built NOTHING does not qualify either: an empty directory plus a
// tombstone would deactivate the old catalogue and replace it with nothing at
// all, which is the same harm by a quieter route.
func allPublished(outcomes []Outcome) (bool, string) {
	if len(outcomes) == 0 {
		return false, "no catalogues were built, so there is nothing to supersede the old one"
	}
	for _, outcome := range outcomes {
		if outcome.Status != StatusPublished {
			return false, fmt.Sprintf("%s is %s, so the old catalogue still has to serve its resources",
				outcome.CatalogID, outcome.Status)
		}
	}
	return true, ""
}
