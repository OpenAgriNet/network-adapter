// Package jsonmapper transforms one JSON document into another using a JSONata
// mapping fetched at runtime, so that translating between two parties' shapes is
// configuration rather than code.
//
// It is domain-free by design: it knows nothing about who is calling, nothing
// about what a mapping says, and nothing about the payloads passing through. It
// is handed a reference and an input, and it fetches, compiles, caches and runs
// whatever is there. Anything specific to a network or a provider belongs in the
// caller, which is what lets one mapper serve all of them.
package jsonmapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/jsonata-go/jsonata"
	"gopkg.in/yaml.v2"
)

// Defaults applied when an operator leaves a setting out.
const (
	DefaultFetchTimeout    = 5 * time.Second
	DefaultMaxMappingBytes = 256 << 10 // 256 KiB, far above any realistic mapping
	DefaultCacheTTL        = time.Hour
	DefaultNegativeTTL     = time.Minute
	DefaultMaxCacheEntries = 200
)

// codeAdaptationFailed reports a mapping that ran but could not produce a
// result. It is the payload's shape that is wrong, so it is a bad request
// rather than a fault of this adapter.
const codeAdaptationFailed = "SCH_SCHEMA_ADAPTATION_FAILED"

// mappingFile is the published form of a mapping: one binding-action, both
// directions.
//
// One file rather than two because both legs of one upstream call are one unit
// of configuration: they are published, reviewed and retired together, and a
// reference to one is a reference to the other. Which action the file serves is
// decided by the registry entry that points at it, so nothing inside names it.
//
// A half may be absent or empty. That is not an omission to report -- it means
// there is no transform for that direction, and what that means belongs to the
// caller.
type mappingFile struct {
	// Required are the preconditions this binding-action imposes on a payload,
	// verified before either half runs. Absent means none.
	Required []requirement `yaml:"required"`
	// FanOut yields the values this payload must be split across, one upstream
	// call per value. Absent means one call, which is the common case.
	FanOut   string `yaml:"fanOut"`
	Request  string `yaml:"request"`
	Response string `yaml:"response"`
}

// requirement is one precondition: what must hold, and what to tell the caller
// when it does not.
//
// Named check/message so each field says what it is: check is the predicate
// that has to hold, message is what the caller is told when it does not. A
// neutral name like "test" says nothing about which way the predicate must
// answer, and "otherwise" reads like an alternative value rather than an error.
//
// The message is required. A precondition that refuses without saying why is
// the failure this whole facility exists to avoid -- the caller is left with
// "rejected" and no way to act on it.
type requirement struct {
	Check   string `yaml:"check"`
	Message string `yaml:"message"`
}

// Config holds configuration parameters for the mapper.
type Config struct {
	// FetchTimeout bounds a single mapping fetch. A mapping host that accepts
	// the connection and then goes quiet must not hold a request open.
	FetchTimeout time.Duration `yaml:"fetchTimeout" json:"fetchTimeout"`

	// MaxMappingBytes caps what is read from a mapping host. References come
	// from the registry, so an unbounded read is an unbounded allocation driven
	// by whoever can write a registry record.
	MaxMappingBytes int64 `yaml:"maxMappingBytes" json:"maxMappingBytes"`

	// CacheTTL is how long a compiled mapping is reused. It is also how long a
	// corrected mapping takes to take effect.
	CacheTTL time.Duration `yaml:"cacheTTL" json:"cacheTTL"`

	// NegativeTTL is how long a failed fetch is remembered. Without it a broken
	// reference turns every inbound request into an outbound one.
	NegativeTTL time.Duration `yaml:"negativeTTL" json:"negativeTTL"`

	// MaxCacheEntries bounds the cache, which would otherwise grow with the
	// number of capabilities ever seen.
	MaxCacheEntries int `yaml:"maxCacheEntries" json:"maxCacheEntries"`
}

// evaluateLocked runs one expression under the package lock.
//
// The unlock is DEFERRED, not written after the call. A panic inside the
// library -- and this is a library we have already found a data race in --
// would otherwise leave the mutex held with nothing to release it. Because the
// lock is package-wide, that is not one wedged mapping but every mapping in
// the process, for every provider step, until a restart. Widening the lock
// widened that blast radius, so the defer matters more here than it did when
// the lock was per mapping.
//
// A helper rather than a defer at each call site, because the precondition
// loop evaluates once per check: a defer there would release only when the
// whole loop returned, holding the lock across every check in the file.
//
// The panic is converted to an error rather than re-raised. net/http recovers
// a panic per connection, so re-raising costs the caller its connection with
// no NACK and nothing in our log naming the mapping -- for what is, from the
// caller's side, indistinguishable from a mapping that could not be applied.
// Reported as one, so it lands where the fault is.
func evaluateLocked(expr jsonata.Expression, document []byte) (result []byte, err error) {
	evaluating.Lock()
	defer evaluating.Unlock()

	// Registered after the unlock so it runs BEFORE it: recover, name the
	// failure, then release.
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("jsonata evaluation panicked: %v", recovered)
		}
	}()

	return expr.Evaluate(document, nil)
}

// evaluating serialises every Evaluate in this package, across all mappings.
//
// It has to be this wide. Evaluate mutates more than the expression it is
// called on: the library keeps its built-in functions in a package-level frame
// (v206's staticFrame), and applying one writes token and position onto that
// shared *Function for error reporting. Every mapping uses built-ins, so any
// two concurrent evaluations race -- including two DIFFERENT mappings, which a
// per-mapping lock explicitly allowed to run in parallel. That was this code's
// previous shape, and it was wrong.
//
// Measured, not reasoned about: eight goroutines, each with its own
// jsonata.OpenLatest() instance and its own compiled expression, evaluating an
// expression shaped like the shipped mappings, produce race reports under
// -race. Separate instances are not separate state, so nothing narrower than
// package scope is sufficient.
//
// The cost is real and bounded: mapping evaluation no longer overlaps, at ~22us
// a call, while the upstream HTTP request each mapped call goes on to make is
// outside this lock and dominates. If it ever does matter, the fix is upstream
// -- the shared writes are error-reporting metadata that could be carried on
// the call rather than the function -- not a narrower lock here.
//
// reqmapper and schemaversionmediator evaluate JSONata too and have the same
// exposure. Not addressed here: they are separate plugins with their own
// owners, and this lock cannot reach across a .so boundary anyway.
var evaluating sync.Mutex

// cacheEntry is one compiled mapping, or the failure that stopped it compiling.
// Failures are cached too, which is the whole point of the negative TTL.
//
// Evaluation is serialised process-wide by evaluating, above -- see there for
// why it cannot be per mapping.
//
// Evaluating under a lock rather than compiling per request is the cheaper
// trade by a wide margin: evaluation is ~22us against ~184us to compile, and
// both are dwarfed by the upstream call the mapped request goes on to make.
type cacheEntry struct {
	// directions holds the compiled halves the file carries. A file is fetched
	// and compiled as a whole, so both are ready after the first request for
	// either.
	directions map[definition.Direction]*compiledMapping
	// checks are the file's preconditions, in the order it declared them.
	checks []*compiledRequirement
	// err is a failure that applies to the whole file -- it could not be
	// fetched, or not parsed -- as opposed to one action failing to compile.
	err       error
	expiresAt time.Time
}

// compiledRequirement is one precondition, or the failure that stopped it
// compiling. Held per requirement for the same reason a half is: a broken
// precondition is the mapping's fault and should be reported as one, without
// taking the halves down with it.
type compiledRequirement struct {
	expression jsonata.Expression
	message    string
	err        error
}

// compiledMapping is one half of a mapping, or the failure that stopped it
// compiling. Failures are held per half deliberately: a typo in the response
// mapping is no reason for the request half to stop working, and finding out on
// the way out beats finding out before the call was even made.
//
// A nil expression with no error is a half the file carries no transform for --
// held rather than treated as absent, so "there is no request half" and "the
// file could not be read" stay different answers.
type compiledMapping struct {
	expression jsonata.Expression
	err        error
}

// hasTransform reports whether this half has something to run.
func (m *compiledMapping) hasTransform() bool {
	return m != nil && m.expression != nil
}

// Mapper fetches, compiles and runs mappings. It is safe for concurrent use:
// one mapper serves every inbound request.
type Mapper struct {
	config     *Config
	httpClient *http.Client
	instance   jsonata.JSONataInstance

	mu      sync.RWMutex
	entries map[string]cacheEntry

	// inflight collapses concurrent misses for the same reference into one
	// fetch. Not part of the cache: it holds work in progress, and an entry
	// that has been stored is served by entries above without reaching it.
	inflight singleflight.Group
}

// New creates a Mapper, applying defaults for anything left unset.
func New(ctx context.Context, cfg *Config) (*Mapper, func() error, error) {
	if cfg == nil {
		return nil, nil, errors.New("jsonmapper: config cannot be nil")
	}
	applyDefaults(cfg)

	instance, err := jsonata.OpenLatest()
	if err != nil {
		return nil, nil, fmt.Errorf("jsonmapper: failed to open jsonata: %w", err)
	}

	mapper := &Mapper{
		config:     cfg,
		httpClient: &http.Client{Timeout: cfg.FetchTimeout},
		instance:   instance,
		entries:    make(map[string]cacheEntry),
	}

	closer := func() error {
		log.Debugf(ctx, "Cleaning up JSON mapper resources")
		mapper.httpClient.CloseIdleConnections()
		return nil
	}

	log.Infof(ctx, "JSON mapper created successfully")
	return mapper, closer, nil
}

// applyDefaults fills in every setting an operator left out.
func applyDefaults(cfg *Config) {
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = DefaultFetchTimeout
	}
	if cfg.MaxMappingBytes <= 0 {
		cfg.MaxMappingBytes = DefaultMaxMappingBytes
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = DefaultNegativeTTL
	}
	if cfg.MaxCacheEntries <= 0 {
		cfg.MaxCacheEntries = DefaultMaxCacheEntries
	}
}

// Transform runs one direction of the mapping at mappingRef over input.
func (m *Mapper) Transform(ctx context.Context, mappingRef string, direction definition.Direction, input any) ([]byte, error) {
	switch direction {
	case definition.DirectionRequest, definition.DirectionResponse, definition.DirectionFanOut:
	default:
		return nil, fmt.Errorf("jsonmapper: mapping %q: %q is not a direction; want %q, %q or %q",
			mappingRef, direction, definition.DirectionRequest, definition.DirectionResponse,
			definition.DirectionFanOut)
	}

	entry, err := m.compiled(ctx, mappingRef)
	if err != nil {
		return nil, err
	}

	mapping, present := entry.directions[direction]
	if !present {
		return nil, fmt.Errorf("jsonmapper: mapping %q carries no %s half", mappingRef, direction)
	}
	if mapping.err != nil {
		return nil, mapping.err
	}
	if !mapping.hasTransform() {
		// Nothing to apply. That is an answer, not a failure: the caller decides
		// what an absent transform means for the leg it is on.
		log.Debugf(ctx, "JSON mapping %s carries no %s transform", mappingRef, direction)
		return nil, nil
	}
	return m.evaluate(ctx, mapping, mappingRef, direction, input)
}

// Verify checks the preconditions the mapping declares, in the order declared.
//
// The first failure is the one reported. Reporting the last, or all of them,
// buries the thing to fix.
func (m *Mapper) Verify(ctx context.Context, mappingRef string, input any) error {
	entry, err := m.compiled(ctx, mappingRef)
	if err != nil {
		return err
	}
	if len(entry.checks) == 0 {
		return nil
	}

	document, err := marshalInput(input)
	if err != nil {
		return fmt.Errorf("jsonmapper: mapping %q: %w", mappingRef, err)
	}

	for _, precondition := range entry.checks {
		if precondition.err != nil {
			return precondition.err
		}

		// See evaluateLocked: serialised across the package, and released even
		// if the library panics.
		result, evalErr := evaluateLocked(precondition.expression, document)
		if evalErr != nil {
			log.Errorf(ctx, evalErr, "JSON mapping %s precondition failed to evaluate: %v", mappingRef, evalErr)
			return model.NewBadReqErr(codeAdaptationFailed, fmt.Errorf(
				"mapping %q precondition could not be applied: %w", mappingRef, evalErr))
		}

		holds, answered := asBool(result)
		if !answered {
			// Not read as permission. A typo yielding nothing would otherwise
			// wave every request through the check meant to stop it.
			return fmt.Errorf("jsonmapper: mapping %q precondition answered %q, want true or false",
				mappingRef, result)
		}
		if !holds {
			// The mapping's own words: the caller is told what is wrong with
			// their payload, not that an expression somewhere returned false.
			return model.NewBadReqErr("", errors.New(precondition.message))
		}
	}
	return nil
}

// asBool reads a precondition's answer. JSONata yields JSON, so a predicate
// answers with the two literals and nothing else counts.
func asBool(result []byte) (bool, bool) {
	switch strings.TrimSpace(string(result)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// compiled returns the compiled mapping for a reference, fetching and compiling
// it on first use. A failure is cached too, for a shorter time.
func (m *Mapper) compiled(ctx context.Context, mappingRef string) (cacheEntry, error) {
	if entry, found := m.cached(mappingRef); found {
		return entry, entry.err
	}

	// One fetch per reference, however many requests miss at once.
	//
	// Without this, a cold start sends every concurrent request for the same
	// capability to fetch the mapping over HTTP and then compile it. The
	// compile is the cheaper half: the round trip is bounded by fetchTimeout,
	// so fifty requests arriving together waited fifty times for the same
	// document instead of once, and the publisher saw fifty identical reads.
	//
	// The shared call inherits the FIRST caller's context, which is
	// singleflight's known trade: if that caller goes away the work is
	// cancelled for everyone waiting on it. Bounded here by fetchTimeout, and
	// the losers see a cancellation they can retry rather than a wrong answer.
	shared, err, _ := m.inflight.Do(mappingRef, func() (any, error) {
		// Re-checked inside the group: a concurrent store may have landed
		// between the miss above and the turn to run, and reusing it is both
		// cheaper and more consistent than fetching a second copy.
		if entry, found := m.cached(mappingRef); found {
			return entry, entry.err
		}
		directions, checks, err := m.fetchAndCompile(ctx, mappingRef)
		return m.remember(mappingRef, directions, checks, err), err
	})
	entry, _ := shared.(cacheEntry)
	return entry, err
}

// cached returns a live cache entry, if there is one.
func (m *Mapper) cached(mappingRef string) (cacheEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, found := m.entries[mappingRef]
	if !found || time.Now().After(entry.expiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

// remember caches a compiled mapping, or the failure that stopped it compiling.
// A failure gets the shorter TTL: it should stop hammering a broken reference
// without outlasting the fix.
func (m *Mapper) remember(mappingRef string, directions map[definition.Direction]*compiledMapping,
	checks []*compiledRequirement, err error) cacheEntry {
	ttl := m.config.CacheTTL
	if err != nil {
		ttl = m.config.NegativeTTL
	}
	entry := cacheEntry{
		directions: directions,
		checks:     checks,
		err:        err,
		expiresAt:  time.Now().Add(ttl),
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Expired entries are dropped before the cap is measured. Without this the
	// map only ever grows: cached() treats an expiry as a miss but leaves the
	// entry behind, so the count reaches the cap and then refuses every ref it
	// is not already holding -- permanently, for the life of the process. The
	// cost is not "compile again next time" but compile every time, for every
	// mapping the deployment has.
	//
	// A sweep rather than an eviction policy: the entries are few, this runs
	// only on a store, and it drops nothing that was still usable.
	m.purgeExpired()

	// Bounded rather than evicting: references come from the registry, and a
	// deployment serving more live capabilities than the cap wants a bigger
	// cap, not a cache that silently thrashes. The entry is still returned to
	// its caller when it is not stored, so a request over the cap is served
	// rather than refused -- it just pays to compile again next time.
	if len(m.entries) >= m.config.MaxCacheEntries {
		if _, replacing := m.entries[mappingRef]; !replacing {
			return entry
		}
	}
	m.entries[mappingRef] = entry
	return entry
}

// purgeExpired drops entries past their TTL. The caller holds m.mu.
func (m *Mapper) purgeExpired() {
	now := time.Now()
	for ref, entry := range m.entries {
		if now.After(entry.expiresAt) {
			delete(m.entries, ref)
		}
	}
}

// cachedCount reports how many mappings are held. Used by tests to assert the
// cache stays bounded.
func (m *Mapper) cachedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// fetchAndCompile retrieves a mapping and turns it into a runnable expression.
func (m *Mapper) fetchAndCompile(ctx context.Context, mappingRef string) (
	map[definition.Direction]*compiledMapping, []*compiledRequirement, error) {
	body, err := m.fetch(ctx, mappingRef)
	if err != nil {
		return nil, nil, err
	}
	file, err := parseMapping(body)
	if err != nil {
		return nil, nil, fmt.Errorf("jsonmapper: mapping %q: %w", mappingRef, err)
	}

	// Preconditions compile with the halves, so one round trip leaves the whole
	// file ready and a precondition costs no extra fetch.
	checks := make([]*compiledRequirement, 0, len(file.Required))
	for _, declared := range file.Required {
		checks = append(checks, m.compileRequirement(ctx, mappingRef, declared))
	}

	// Both halves are compiled now rather than on first use, so one fetch leaves
	// the file ready in both directions. A compile failure is recorded against
	// its own half and goes no further.
	directions := make(map[definition.Direction]*compiledMapping, 3)
	directions[definition.DirectionRequest] = m.compileMapping(ctx, mappingRef, definition.DirectionRequest, file.Request)
	directions[definition.DirectionResponse] = m.compileMapping(ctx, mappingRef, definition.DirectionResponse, file.Response)
	directions[definition.DirectionFanOut] = m.compileMapping(ctx, mappingRef, definition.DirectionFanOut, file.FanOut)
	log.Debugf(ctx, "JSON mapper compiled mapping: %s (%d precondition(s))", mappingRef, len(checks))
	return directions, checks, nil
}

// compileRequirement compiles one precondition, keeping any failure local to it.
func (m *Mapper) compileRequirement(ctx context.Context, mappingRef string, declared requirement) *compiledRequirement {
	if strings.TrimSpace(declared.Message) == "" {
		return &compiledRequirement{err: fmt.Errorf(
			"jsonmapper: mapping %q declares a precondition with no message", mappingRef)}
	}
	if strings.TrimSpace(declared.Check) == "" {
		return &compiledRequirement{err: fmt.Errorf(
			"jsonmapper: mapping %q declares a precondition with no check", mappingRef)}
	}
	expression, err := m.instance.Compile(declared.Check, false)
	if err != nil {
		log.Errorf(ctx, err, "JSON mapper could not compile a precondition of %s: %v", mappingRef, err)
		return &compiledRequirement{err: fmt.Errorf(
			"jsonmapper: mapping %q precondition %q failed to compile: %w", mappingRef, declared.Check, err)}
	}
	return &compiledRequirement{
		expression: expression,
		message:    declared.Message,
	}
}

// compileMapping compiles one half, keeping any failure local to it.
func (m *Mapper) compileMapping(ctx context.Context, mappingRef string, direction definition.Direction, source string) *compiledMapping {
	if strings.TrimSpace(source) == "" {
		// No transform for this direction. Not an error: a request half is
		// legitimately empty when the caller builds its own request.
		return &compiledMapping{}
	}
	expression, err := m.instance.Compile(source, false)
	if err != nil {
		log.Errorf(ctx, err, "JSON mapper could not compile the %s half of %s: %v", direction, mappingRef, err)
		return &compiledMapping{err: fmt.Errorf("jsonmapper: mapping %q %s half failed to compile: %w", mappingRef, direction, err)}
	}
	return &compiledMapping{expression: expression}
}

// fetch retrieves a mapping's bytes, bounded in both time and size.
func (m *Mapper) fetch(ctx context.Context, mappingRef string) ([]byte, error) {
	if err := verifyFetchable(mappingRef); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mappingRef, nil)
	if err != nil {
		return nil, fmt.Errorf("jsonmapper: failed to build request for mapping %q: %w", mappingRef, err)
	}

	log.Debugf(ctx, "Fetching mapping: %s", mappingRef)
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jsonmapper: failed to fetch mapping %q: %w", mappingRef, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Errorf(ctx, nil, "Mapping fetch failed with status: %s, mapping: %s", resp.Status, mappingRef)
		return nil, fmt.Errorf("jsonmapper: mapping %q returned %s", mappingRef, resp.Status)
	}

	// LimitReader with one spare byte, so an oversized body is refused rather
	// than silently truncated into a mapping that compiles to something else.
	body, err := io.ReadAll(io.LimitReader(resp.Body, m.config.MaxMappingBytes+1))
	if err != nil {
		return nil, fmt.Errorf("jsonmapper: failed to read mapping %q: %w", mappingRef, err)
	}
	if int64(len(body)) > m.config.MaxMappingBytes {
		return nil, fmt.Errorf("jsonmapper: mapping %q exceeds the %d byte limit", mappingRef, m.config.MaxMappingBytes)
	}
	return body, nil
}

// verifyFetchable rejects a reference this mapper will not retrieve.
//
// A reference is a fully-qualified http or https URL, carried verbatim from the
// registry. That makes it external input, so it is checked rather than trusted:
// without this a registry record could name a file path or an internal scheme
// and have the adapter read it. What the check cannot constrain is WHICH host --
// a registry record chooses that, and this mapper compiles and runs what comes
// back from it. Who may write a registry record is therefore part of this
// plugin's threat model, not an unrelated concern.
func verifyFetchable(mappingRef string) error {
	if mappingRef == "" {
		return errors.New("jsonmapper: mapping reference is empty")
	}
	parsed, err := url.Parse(mappingRef)
	if err != nil {
		return fmt.Errorf("jsonmapper: invalid mapping reference %q: %w", mappingRef, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("jsonmapper: mapping reference %q must be http or https", mappingRef)
	}
	if parsed.Host == "" {
		return fmt.Errorf("jsonmapper: mapping reference %q names no host", mappingRef)
	}
	return nil
}

// parseMapping reads the two halves a published mapping carries.
func parseMapping(body []byte) (mappingFile, error) {
	var file mappingFile
	if err := yaml.Unmarshal(body, &file); err != nil {
		return mappingFile{}, fmt.Errorf("could not be parsed: %w", err)
	}
	if strings.TrimSpace(file.Request) == "" && strings.TrimSpace(file.Response) == "" {
		// Neither half present at all -- not an empty request, which is
		// meaningful, but a file that says nothing.
		return mappingFile{}, errors.New("carries neither a request nor a response half")
	}
	return file, nil
}

// marshalInput renders the named inputs a mapping reads -- beckn and, on the
// response leg, response -- as the single JSON document JSONata evaluates
// against.
func marshalInput(input any) ([]byte, error) {
	document, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("input could not be encoded: %w", err)
	}
	return document, nil
}

// evaluate runs a compiled mapping over the input document.
func (m *Mapper) evaluate(ctx context.Context, mapping *compiledMapping, mappingRef string, direction definition.Direction, input any) ([]byte, error) {
	document, err := marshalInput(input)
	if err != nil {
		return nil, fmt.Errorf("jsonmapper: mapping %q: %w", mappingRef, err)
	}

	// See evaluateLocked: serialised across the package, because the library's
	// shared built-ins make even two different mappings unsafe to overlap.
	// Marshalling above is deliberately outside the lock.
	result, err := evaluateLocked(mapping.expression, document)
	if err != nil {
		log.Errorf(ctx, err, "JSON mapping %s %s half failed to evaluate: %v", mappingRef, direction, err)
		wrapped := fmt.Errorf("mapping %q %s half could not be applied: %w", mappingRef, direction, err)
		if direction == definition.DirectionResponse {
			// The input here is the PROVIDER's answer, not the caller's
			// request. A provider that changed shape, or a bug in the response
			// half, is nothing the caller did -- reporting 400 sends them off
			// to fix a request that was fine. 502: the upstream exchange is
			// what failed.
			return nil, model.NewCodedErr(http.StatusBadGateway, codeAdaptationFailed, wrapped)
		}
		// On the request leg the mapping is valid and the payload is not what it
		// expected, so this is the caller's request being wrong.
		return nil, model.NewBadReqErr(codeAdaptationFailed, wrapped)
	}
	return result, nil
}
