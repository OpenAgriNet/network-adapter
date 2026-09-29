package pipeline

// source.go loads a pipeline from the URL the registry names -- the same way
// the adapter already loads every other mapping: the registry's `mappings`
// field is an https URL, and jsonmapper fetches it.
//
// A pipeline file is more sensitive than a select mapping, because it decides
// what the run does with the upstream's credentials. Two rules keep whoever
// can edit the hosted file (or the registry record naming it) from choosing
// where those credentials go:
//
//   - the upstream address is ${inputs.baseUrl}, resolved from env or plugin
//     config, never a literal host in the file (checkUpstreamIsAnInput);
//   - every call path starts with "/", so nothing appended to the base can
//     change its host (checkCallPath, in upstream.go).

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/beckn/catalog-core/pkg/catalog/crawler"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// Fault classes for the ways a pipeline itself is unusable -- as opposed to
// its upstream being unreachable, which is transient and worth retrying.
// Wrapped with crawler.PermanentFaultf so a caller with a retry budget (the
// crawler's publish sweep) can tell "this pipeline will never load" from "the
// network hiccuped", via the same crawler.IsPermanent it already uses for the
// crawl path's own signature failures -- one vocabulary, not two.
const (
	faultPipelineURL   crawler.FaultClass = "pipeline_url"
	faultPipelineSpec  crawler.FaultClass = "pipeline_spec"
	faultPipelineInput crawler.FaultClass = "pipeline_input"
)

// permanent marks cause as a permanent fault of class without losing it.
// crawler.PermanentError keeps only a Sprintf'd message and has no Unwrap, so
// the cause is joined beside it: IsPermanent still finds the marker, errors.Is
// and errors.As still reach the cause, and the message is the cause's own.
func permanent(class crawler.FaultClass, cause error) error {
	return fmt.Errorf("%w%w", crawler.PermanentFaultf(class, ""), cause)
}

// maxPipelineBytes caps a fetched pipeline file. The Mandi pipeline is about
// 15 KB; a megabyte is room to grow without an unbounded read.
const maxPipelineBytes = 1 << 20

// pipelineFetchTimeout bounds one fetch of a pipeline file.
const pipelineFetchTimeout = 30 * time.Second

// pipelineHTTP fetches pipeline files. Redirects are followed only on the
// same host: the URL was the registry's to choose, a redirect target is not.
var pipelineHTTP = &http.Client{
	Timeout:       pipelineFetchTimeout,
	CheckRedirect: util.RefuseOffHostRedirect,
}

// lastGood keeps the last copy of each pipeline URL that fetched AND
// validated, so a hosting outage at the firing does not cost the day: the
// run proceeds on the pipeline it ran last time, and says so. A file that
// fails validation is never stored.
var lastGood = struct {
	sync.Mutex
	copies map[string][]byte
}{copies: map[string][]byte{}}

// checkPipelineURL refuses anything but an https URL (http on loopback only,
// for a developer's server and this package's tests).
func checkPipelineURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || trimmed == "" || parsed.Scheme == "" {
		return crawler.PermanentFaultf(faultPipelineURL,
			"the registry's publish mappings is %q; it must be the https URL of the pipeline "+
				"file (e.g. https://raw.githubusercontent.com/<org>/<repo>/<ref>/.../catalogpublish/agmarknet.yaml)", raw)
	}
	if parsed.Host == "" {
		return crawler.PermanentFaultf(faultPipelineURL, "pipeline URL %q names no host", raw)
	}
	switch {
	case parsed.Scheme == "https":
		return nil
	case parsed.Scheme == "http" && isLoopback(parsed.Hostname()):
		return nil
	default:
		return crawler.PermanentFaultf(faultPipelineURL,
			"pipeline URL %q uses %s; a pipeline decides where upstream credentials go, "+
				"so it is fetched only over https", raw, parsed.Scheme)
	}
}

// RemotePipeline turns the registry's publish `mappings` value into the Files
// a run is given, refusing anything but an https pipeline URL. Nothing is
// fetched here: the run fetches, so each firing reads the file as it is then.
func RemotePipeline(registryValue string) (Files, error) {
	trimmed := strings.TrimSpace(registryValue)
	if err := checkPipelineURL(trimmed); err != nil {
		return Files{}, err
	}
	return Files{URL: trimmed}, nil
}

// loadPipeline fetches the pipeline at files.URL, validates it against the
// contract it names, and parses it. On a fetch failure it falls back to the
// last good copy of the same URL, if there is one.
func loadPipeline(ctx context.Context, files Files) (Spec, error) {
	if err := checkPipelineURL(files.URL); err != nil {
		return Spec{}, err
	}
	raw, fetchErr := fetchPipeline(ctx, files.URL)
	if fetchErr != nil {
		lastGood.Lock()
		cached, ok := lastGood.copies[files.URL]
		lastGood.Unlock()
		if !ok {
			return Spec{}, fetchErr
		}
		slog.WarnContext(ctx, "publish pipeline: could not fetch the pipeline; running the last good copy",
			"url", files.URL, "error", fetchErr)
		raw = cached
	}

	spec, err := parsePipeline(raw, files.URL)
	if err != nil {
		return Spec{}, err
	}
	if err := checkUpstreamIsAnInput(spec); err != nil {
		return Spec{}, fmt.Errorf("%s: %w", files.URL, err)
	}
	if fetchErr == nil {
		lastGood.Lock()
		lastGood.copies[files.URL] = raw
		lastGood.Unlock()
	}
	return spec, nil
}

// fetchPipeline GETs one pipeline file, bounded in time and size.
func fetchPipeline(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, pipelineFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pipeline %s: request could not be built: %w", rawURL, err)
	}
	resp, err := pipelineHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pipeline %s could not be fetched: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pipeline %s answered %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPipelineBytes+1))
	if err != nil {
		return nil, fmt.Errorf("pipeline %s could not be read: %w", rawURL, err)
	}
	if len(body) > maxPipelineBytes {
		return nil, fmt.Errorf("pipeline %s is over %d bytes", rawURL, maxPipelineBytes)
	}
	return body, nil
}

// parsePipeline validates raw against the contract it names, then parses it.
// Validated BEFORE parsing, against the raw document: validating the struct
// would validate what survived parsing, and a key that did not survive is
// the mistake this is here to catch.
func parsePipeline(raw []byte, name string) (Spec, error) {
	if err := validateBytes(raw, name); err != nil {
		return Spec{}, permanent(faultPipelineSpec, err)
	}
	var spec Spec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return Spec{}, permanent(faultPipelineSpec, fmt.Errorf("parse %s: %w", name, err))
	}
	return spec, nil
}

// upstreamInputRef is the only upstream address a pipeline may state.
const upstreamInputRef = "${inputs.baseUrl}"

// checkUpstreamIsAnInput refuses a pipeline that names its upstream as a
// literal host. The client is built from the resolved baseUrl input either
// way; refusing the literal keeps the file from appearing to say otherwise,
// and keeps "where do the credentials go" a deployment's decision.
func checkUpstreamIsAnInput(spec Spec) error {
	base := strings.TrimSpace(spec.Upstream.BaseURL)
	if base == "" || base == upstreamInputRef {
		return nil
	}
	return crawler.PermanentFaultf(faultPipelineSpec,
		"upstream.baseUrl is %q; it must be %s, so the address credentials are sent to "+
			"comes from the deployment (env or plugin config), not from the pipeline file", base, upstreamInputRef)
}

// resolveMappingRef resolves a file's mapping reference against the file's
// own URL, the way a link in a page is resolved against the page.
func resolveMappingRef(base, ref string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("pipeline URL %q: %w", base, err)
	}
	refURL, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("mapping %q: %w", ref, err)
	}
	return baseURL.ResolveReference(refURL).String(), nil
}
