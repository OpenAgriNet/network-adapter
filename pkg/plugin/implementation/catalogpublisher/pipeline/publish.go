package pipeline

// The pipeline's publish step: which built files go out, in what order, under
// what safety rule, and what counts as success. It does NOT make the HTTP
// call. That is a Publisher's -- in production the crawler's sink
// (catalogcrawler/internal/sink), the one piece of code that posts to the
// provider adapter's /publish, for crawled catalogs and pipelines alike.
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
	"strconv"
	"strings"
)

// Per-catalog outcomes.
const (
	StatusPublished      = "published"
	StatusRejected       = "rejected"
	StatusTransportError = "transport-error"
)

// Outcome is what happened to one catalog.
type Outcome struct {
	Group     string
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
// an Outcome, not an error, so one bad catalog never hides the others.
type Publisher interface {
	Publish(ctx context.Context, baseURL string, body []byte) Outcome

	// Retire deactivates a superseded catalog. The Publisher builds the
	// body -- with the same builder as every publish -- so a retirement
	// carries the same identity, and every field the file's retireOld states.
	Retire(ctx context.Context, baseURL string, retirement Retirement) Outcome
}

// Retirement is a resolved retireOld block: what to deactivate, and the
// directive it goes out under.
type Retirement struct {
	CatalogID      string
	DescriptorName string
	CatalogType    string   // REGULAR unless the file says otherwise
	UpdateMode     string   // MERGE or FULL
	VisibleTo      []string // the networks the retired catalog was visible to
	SchemaTypes    []string // the retired catalog's JSON-LD schema contexts
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
//	collection.<counter> > <N>
//
// <counter> is a name the pipeline's own steps record into, so a second
// pipeline refuses on its own terms without editing Go, and N is a
// non-negative integer: `> 0` refuses on any failure, `> 3` tolerates three.
// Anything else is REJECTED rather than ignored -- a safety rule that is
// quietly dropped is worse than one never declared -- and a file needing a
// richer rule needs a real expression evaluator here first.
var refuseWhenShape = regexp.MustCompile(`^collection\.([A-Za-z][A-Za-z0-9_]*)\s*>\s*([0-9]+)$`)

// refuseWhenRule returns the counter a refuseWhen rule names and the count it
// tolerates.
func refuseWhenRule(rule string) (string, int, bool) {
	match := refuseWhenShape.FindStringSubmatch(strings.TrimSpace(rule))
	if match == nil {
		return "", 0, false
	}
	threshold, err := strconv.Atoi(match[2])
	if err != nil {
		return "", 0, false
	}
	return match[1], threshold, true
}

// PublishCatalogs posts every catalog file in catalogDir through
// publisher, unless the declared safety rule forbids it.
//
// collected is every counter the run recorded; publish.refuseWhen names the
// one it judges. A part that failed to collect is not a part with nothing in
// it, so publishing past the file's tolerance would replace a whole group's
// catalog with a partial one, or with nothing.
//
// Sequential and per-catalog on purpose: one failed catalog is one
// retryable catalog, and must not discard the outcomes of the others.
func PublishCatalogs(ctx context.Context, spec Publish, resolved map[string]string,
	catalogDir, filenamePrefix string, collected map[string]int, publisher Publisher) (Result, error) {
	var result Result

	if publisher == nil {
		return result, fmt.Errorf("no publisher: this run was asked to publish but given nothing to publish with")
	}

	if rule := strings.TrimSpace(spec.RefuseWhen); rule != "" {
		counter, threshold, ok := refuseWhenRule(rule)
		if !ok {
			return result, fmt.Errorf(
				"publish.refuseWhen is %q; this step understands only `collection.<counter> > <N>` "+
					"and will not guess at another rule", rule)
		}
		if failed := collected[counter]; failed > threshold {
			return result, fmt.Errorf(
				"refusing to publish: %d of the collection failed (%s), above the %d this pipeline "+
					"tolerates, and a partial collection will not be published as though it were whole "+
					"(publish.refuseWhen: %s)", failed, counter, threshold, rule)
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

	files, err := catalogFiles(catalogDir, filenamePrefix)
	if err != nil {
		return result, err
	}
	// An empty directory must not look like a successful run: silently
	// publishing nothing is indistinguishable from publishing everything.
	if len(files) == 0 && retire == nil {
		return result, fmt.Errorf("no catalog files in %s", catalogDir)
	}

	for _, file := range files {
		result.Outcomes = append(result.Outcomes, publishFile(ctx, publisher, publishURL, file))
	}
	// The retirement goes out ONLY when everything that supersedes the old
	// catalog is actually on the network.
	//
	// Retiring is not a tidy-up, it is a deletion: deactivating the old
	// catalog is how its resources leave. Sending it after a run whose
	// replacements were rejected removes the old data and puts nothing in its
	// place, leaving the network with neither -- and the next tick repeats it.
	if retire != nil {
		if published, why := allPublished(result.Outcomes); !published {
			result.RetiredSkipped = why
		} else {
			outcome := publisher.Retire(ctx, publishURL, *retire)
			if outcome.CatalogID == "" {
				outcome.CatalogID = retire.CatalogID
			}
			result.RetiredOld = &outcome
		}
	}
	return result, nil
}

// catalogFile pairs a built catalog with the group it belongs to.
//
// slug and group differ for a split group: <prefix>-TN-2.json has slug
// TN-2 and group TN.
type catalogFile struct {
	slug  string
	group string
	path  string
}

// chunkSuffix matches the -N a split group's catalog carries.
var chunkSuffix = regexp.MustCompile(`-[0-9]+$`)

// catalogFiles lists <prefix>-<slug>.json in dir, sorted, so a run reads the
// same way twice. The match is the one WriteCatalogs and
// RemoveStaleCatalogs make -- three places, one naming contract.
func catalogFiles(dir, prefix string) ([]catalogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read catalog directory: %w", err)
	}
	namePrefix := prefix + "-"
	var files []catalogFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(name, namePrefix), ".json")
		files = append(files, catalogFile{
			slug: slug, group: chunkSuffix.ReplaceAllString(slug, ""), path: filepath.Join(dir, name),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].slug < files[j].slug })
	return files, nil
}

// publishFile reads one built catalog and posts it VERBATIM: the file is
// what a human reviewed, so re-encoding it would publish something nobody
// read.
func publishFile(ctx context.Context, publisher Publisher, publishURL string, file catalogFile) Outcome {
	body, err := os.ReadFile(file.path)
	if err != nil {
		return Outcome{Group: file.group, Status: StatusTransportError,
			Reason: fmt.Sprintf("read %s: %v", file.path, err)}
	}
	outcome := publisher.Publish(ctx, publishURL, body)
	outcome.Group = file.group
	if outcome.CatalogID == "" {
		outcome.CatalogID = catalogIDOf(body)
	}
	return outcome
}

// catalogIDOf reads the catalog's own id out of a publish body, so a
// reported id is the one actually sent rather than one rebuilt from a name.
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

// expectedPublishURL is the only publish.url this step can honour, for the
// reason given where the address is resolved: the base goes to
// the Publisher, which appends the path itself.
const expectedPublishURL = "${inputs.publishUrl}/publish"

// checkPublishURL refuses a publish.url this step would ignore.
//
// The address actually used comes from inputs.publishUrl, not from this
// field. Without this check an operator could repoint publish.url at another
// host, watch the edit take no effect, and have the catalog posted to
// CATALOG_PUBLISH_URL anyway -- a rule the file states and nothing enforces,
// which is also why refuseWhen is parsed strictly above.
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
// the file states and nothing performs.
//
// Enabled is an `${inputs.*}` reference. One naming an input the file does not
// declare is refused rather than read as false: treating an unresolvable
// enable flag as "off" would silently skip the retirement, which is the
// outcome an operator who wrote the block was trying to avoid.
func retirement(spec RetireOld, resolved map[string]string) (*Retirement, error) {
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
	if spec.IsActive != nil && *spec.IsActive {
		return nil, fmt.Errorf("publish.retireOld.isActive is true, but a retirement deactivates the catalog; " +
			"remove it or set it to false")
	}

	out := Retirement{
		CatalogID:      strings.TrimSpace(spec.CatalogID),
		DescriptorName: spec.DescriptorName,
		CatalogType:    orDefault(spec.CatalogType, "REGULAR"),
		UpdateMode:     strings.ToUpper(orDefault(spec.UpdateMode, "MERGE")),
	}
	if out.UpdateMode != "MERGE" && out.UpdateMode != "FULL" {
		return nil, fmt.Errorf("publish.retireOld.updateMode is %q; use MERGE or FULL", spec.UpdateMode)
	}
	var err error
	if out.VisibleTo, err = resolveInputList("visibleTo", spec.VisibleTo, resolved); err != nil {
		return nil, err
	}
	if out.SchemaTypes, err = resolveInputList("schemaTypes", spec.SchemaTypes, resolved); err != nil {
		return nil, err
	}
	return &out, nil
}

// resolveInputList resolves each entry: a whole-entry ${inputs.name} becomes
// that input's value, anything else is taken as written. An entry naming an
// input that is not declared, or that resolves to empty, is refused -- a
// visibility list with a hole in it scopes the deactivation to nobody.
func resolveInputList(field string, entries []string, resolved map[string]string) ([]string, error) {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if name, isRef := strings.CutPrefix(entry, "${inputs."); isRef && strings.HasSuffix(name, "}") {
			name = strings.TrimSuffix(name, "}")
			value, declared := resolved[name]
			if !declared || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("publish.retireOld.%s names %s, which is not declared or resolved to empty",
					field, entry)
			}
			entry = value
		}
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out, nil
}

func orDefault(value, fallback string) string {
	if v := strings.TrimSpace(value); v != "" {
		return v
	}
	return fallback
}

// allPublished reports whether every catalog this run built actually
// reached the network, and says what stopped it when one did not.
//
// A run that built NOTHING does not qualify either: an empty directory plus a
// retirement would deactivate the old catalog and replace it with nothing at
// all, which is the same harm by a quieter route.
func allPublished(outcomes []Outcome) (bool, string) {
	if len(outcomes) == 0 {
		return false, "no catalogs were built, so there is nothing to supersede the old one"
	}
	for _, outcome := range outcomes {
		if outcome.Status != StatusPublished {
			return false, fmt.Sprintf("%s is %s, so the old catalog still has to serve its resources",
				outcome.CatalogID, outcome.Status)
		}
	}
	return true, ""
}
