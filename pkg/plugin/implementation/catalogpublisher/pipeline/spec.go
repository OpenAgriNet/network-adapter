// spec.go types and parses mandi-price-agmarket.yaml itself. The engine that
// runs a CataloguePipeline doesn't exist yet (see the file's own header
// comment), so this is deliberately just the YAML<->struct mapping the
// polling layer will need to validate and read the file with -- not a
// pipeline executor. Field names and shapes come straight from the file, not
// from what would be convenient to build later.
package pipeline

import (
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Spec is the whole pipeline definition, one file mapped one-to-one.
//
// EVERY top-level block in the file has a field here, including ones nothing
// reads yet. yaml.v3 drops unknown keys silently, so a block left out of this
// struct parses "successfully" while vanishing -- which is how a rule written
// in the file (an exclusion, a refusal, a catalogId template) can be believed
// to be in force while no code ever sees it.
type Spec struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   Metadata         `yaml:"metadata"`
	Discover   Discover         `yaml:"discover"`
	Schedule   Schedule         `yaml:"schedule"`
	Registry   Registry         `yaml:"registry"`
	Inputs     map[string]Input `yaml:"inputs"`
	Upstream   Upstream         `yaml:"upstream"`
	Pipeline   []Step           `yaml:"pipeline"`
	Catalog    Catalog          `yaml:"catalog"`
	Publish    Publish          `yaml:"publish"`
	Report     Report           `yaml:"report"`
}

// Discover is where this package sits and where its mappings live, so the
// polling layer can integrity-check the pair before running anything.
type Discover struct {
	Path        string `yaml:"path"`
	MappingsDir string `yaml:"mappingsDir"`
}

// Registry is the metadata lookup that gates a run: no confirmed
// provider/capability binding, no publish. Kind names which registry
// implementation answers it (sunbirdRC today).
type Registry struct {
	Kind           string         `yaml:"kind"`
	URL            string         `yaml:"url"`
	Entity         string         `yaml:"entity"`
	ProviderEntity string         `yaml:"providerEntity"`
	Lookup         RegistryLookup `yaml:"lookup"`
	Fields         []string       `yaml:"fields"`
}

// RegistryLookup is the pair the bindingKey is built from:
// "<participantId>|<capabilityCode>", never typed twice.
type RegistryLookup struct {
	ParticipantID  string `yaml:"participantId"`
	CapabilityCode string `yaml:"capabilityCode"`
}

// Report is what a run prints, per stage.
type Report struct {
	Collection      []string `yaml:"collection"`
	Build           []string `yaml:"build"`
	Publish         []string `yaml:"publish"`
	ExitNonZeroWhen []string `yaml:"exitNonZeroWhen"`
}

// Metadata identifies this pipeline for registry lookups (see the file's
// registry block: capabilityCode there is metadata.capability).
type Metadata struct {
	Name       string `yaml:"name"`
	Capability string `yaml:"capability"`
	Provider   string `yaml:"provider"`
	Summary    string `yaml:"summary"`
}

// Schedule is when the polling layer should run this pipeline, as a standard
// five-field cron expression resolved in Timezone (see cron.go).
type Schedule struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

// Input is one entry of the CLI/env surface, resolved flag > env > default
// by tools/publish/mandi_publish's proven order. Secret marks the two
// credential inputs that have no flag and must never be logged or echoed.
type Input struct {
	Flag    string      `yaml:"flag,omitempty"`
	Env     string      `yaml:"env,omitempty"`
	Type    string      `yaml:"type,omitempty"`
	Format  string      `yaml:"format,omitempty"`
	Default interface{} `yaml:"default,omitempty"`
	Enum    []string    `yaml:"enum,omitempty"`
	Secret  bool        `yaml:"secret,omitempty"`
}

// Upstream is the one service this pipeline calls, how it authenticates, how
// its failures are classified, and the limits every call is held to.
type Upstream struct {
	BaseURL string      `yaml:"baseUrl"`
	Auth    Auth        `yaml:"auth"`
	Errors  []ErrorRule `yaml:"errors"`
	Guards  Guards      `yaml:"guards"`
}

// ErrorRule classifies one upstream failure. Classification, not cosmetics:
// recording "no rows" as a failure once turned 27 of 36 states into 27
// outages on a collection that was as complete as the upstream allows.
//
// A rule either matches on When or is the Default catch-all, never both.
type ErrorRule struct {
	When     *ErrorMatch `yaml:"when,omitempty"`
	Classify string      `yaml:"classify,omitempty"`
	Default  string      `yaml:"default,omitempty"`
}

// ErrorMatch is what an ErrorRule matches on. Status is one code or several,
// so it stays `any` rather than forcing the file to write a list for the
// single-status case.
type ErrorMatch struct {
	Status       any    `yaml:"status,omitempty"`
	BodyContains string `yaml:"bodyContains,omitempty"`
}

// Guards are the per-call limits. NeverQuoteBodyInErrors is not decoration:
// this upstream echoes the request back in error bodies, and the request
// carries the token in its query string.
type Guards struct {
	ResponseMustBe         string `yaml:"responseMustBe"`
	NeverQuoteBodyInErrors bool   `yaml:"neverQuoteBodyInErrors"`
	MaxResponseBytes       string `yaml:"maxResponseBytes"`
}

// Auth is the token-exchange upstream auth needs: no expiry in the response,
// so a token is held for the run and re-exchanged only on the statuses in
// Token.ReexchangeOn, never on a timer.
type Auth struct {
	Kind    string        `yaml:"kind"`
	Request AuthRequest   `yaml:"request"`
	Token   AuthTokenSpec `yaml:"token"`
}

// AuthRequest is the call that mints a token.
type AuthRequest struct {
	Method string            `yaml:"method"`
	Path   string            `yaml:"path"`
	Body   map[string]string `yaml:"body"`
}

// AuthTokenSpec is where the token comes from in the response and how it
// rides on later requests.
type AuthTokenSpec struct {
	At           string `yaml:"at"`
	CarriedAs    string `yaml:"carriedAs"`
	Name         string `yaml:"name"`
	ReexchangeOn []int  `yaml:"reexchangeOn"`
}

// Step is one pipeline stage. Only With's fields a given step actually sets
// are populated; the rest stay zero.
//
// The fields beyond id/uses/with/out are not decoration, and leaving them out
// of this struct is how the file can state a rule that nothing enforces: each
// one carries a decision the reference tool paid for in production.
type Step struct {
	ID   string `yaml:"id"`
	Uses string `yaml:"uses"`
	With With   `yaml:"with"`
	Out  string `yaml:"out,omitempty"`

	// When and Else make a step conditional: `states` only calls the upstream
	// when no state list was supplied, otherwise the supplied list IS the
	// result.
	When string   `yaml:"when,omitempty"`
	Else StepElse `yaml:"else,omitempty"`

	// FailWhenEmpty is a refusal, not a warning. Walking an empty state list
	// produces a well-formed, zero-error, empty collection -- a run that reads
	// as "India has no markets" and exits zero.
	FailWhenEmpty string `yaml:"failWhenEmpty,omitempty"`

	// ForEach/As drive the per-state loop. Concurrency is deliberately 1:
	// thirty-six calls against a service that publishes no rate limit is the
	// polite default, and parallelism would buy seconds while risking a
	// throttle that looks exactly like data loss.
	ForEach     string `yaml:"forEach,omitempty"`
	As          string `yaml:"as,omitempty"`
	Concurrency int    `yaml:"concurrency,omitempty"`

	// OnError and OnEmptyOutput are the 27-of-36-states lesson written as
	// data: an upstream saying "no rows for this state" is a coverage fact to
	// record and continue past, not a failure to abort on.
	OnError       map[string]StepOutcome `yaml:"onError,omitempty"`
	OnEmptyOutput StepOutcome            `yaml:"onEmptyOutput,omitempty"`
}

// StepElse is what a conditional step yields when its When is false. Const is
// a literal rather than another call: `states` falls back to the list the
// operator already supplied.
type StepElse struct {
	Const string `yaml:"const,omitempty"`
}

// StepOutcome is what to do with a classified result: name the bucket it is
// recorded in, and whether the run carries on. Continue is what keeps one
// state's empty answer from ending a thirty-six state collection.
type StepOutcome struct {
	Record   string `yaml:"record,omitempty"`
	Continue bool   `yaml:"continue,omitempty"`
}

// With is a step's parameters. It is the union of every field any step in
// this file uses -- http.get steps set Path/Mapping/Local, join sets
// Left/Right/On/Type/Carry, derive sets Field/Rules/Then, dedupe sets Key --
// because YAML steps are heterogeneous and Go structs are not.
type With struct {
	Path    string            `yaml:"path,omitempty"`
	Mapping string            `yaml:"mapping,omitempty"`
	Local   map[string]string `yaml:"local,omitempty"`

	Left  string   `yaml:"left,omitempty"`
	Right string   `yaml:"right,omitempty"`
	On    string   `yaml:"on,omitempty"`
	Type  string   `yaml:"type,omitempty"`
	Carry []string `yaml:"carry,omitempty"`

	Field string                   `yaml:"field,omitempty"`
	Rules []map[string]interface{} `yaml:"rules,omitempty"`
	Then  []map[string]interface{} `yaml:"then,omitempty"`

	Key string `yaml:"key,omitempty"`
}

// Catalog is how a collection becomes catalog files, one per group.
type Catalog struct {
	GroupBy  string         `yaml:"groupBy"`
	Exclude  []ExcludeRule  `yaml:"exclude"`
	Annotate []AnnotateRule `yaml:"annotate"`
	Order    Order          `yaml:"order"`
	Chunk    Chunk          `yaml:"chunk"`
	Identity Identity       `yaml:"identity"`
	Render   Render         `yaml:"render"`
	Output   Output         `yaml:"output"`
}

// ExcludeRule keeps a market out of the catalog entirely, with a stated
// reason. Every excluded market is NAMED in the report rather than merely
// counted: "95 markets have missing coordinates" tells nobody which ones, and
// the reason for reporting them at all is that somebody can look one up.
type ExcludeRule struct {
	When   string `yaml:"when"`
	Reason string `yaml:"reason"`
}

// AnnotateRule labels a market that still publishes. A geometry-less market
// is findable by state or district but by no proximity search, which is a
// fact about the result worth carrying rather than hiding.
type AnnotateRule struct {
	When string `yaml:"when"`
	As   string `yaml:"as"`
	Note string `yaml:"note,omitempty"`
}

// Order makes a run read the same way twice.
type Order struct {
	By        string `yaml:"by"`
	Direction string `yaml:"direction"`
}

// Identity is the id templates a rendered catalog and its resources carry.
type Identity struct {
	CatalogID  string `yaml:"catalogId"`
	ResourceID string `yaml:"resourceId"`
}

// Output is where a rendered catalog lands. FilenamePrefix is the contract
// between what build writes and what publish matches -- one value, both
// sides.
type Output struct {
	Dir            string `yaml:"dir"`
	File           string `yaml:"file"`
	FilenamePrefix string `yaml:"filenamePrefix"`
}

// Chunk is the geometry-budget split described in CatalogGeometryBudget's
// comment in steps.go -- Budget here is that same cap, read from the file
// rather than duplicated as a second constant.
type Chunk struct {
	Budget int    `yaml:"budget"`
	Of     string `yaml:"of"`
	Cost   string `yaml:"cost"`
	Slug   string `yaml:"slug"`
}

// Render is the mapping that turns a chunk into a catalog document.
type Render struct {
	Mapping string            `yaml:"mapping"`
	Local   map[string]string `yaml:"local,omitempty"`
}

// Publish is where catalog files go and what counts as success.
//
// RefuseWhen is a safety rule, not a preference: a state that failed to
// collect is not a state with no markets, so a partial collection must never
// publish as though it were whole.
type Publish struct {
	URL            string    `yaml:"url"`
	Concurrency    int       `yaml:"concurrency,omitempty"`
	Timeout        string    `yaml:"timeout,omitempty"`
	Accept         []string  `yaml:"accept"`
	TreatAsFailure []string  `yaml:"treatAsFailure,omitempty"`
	RefuseWhen     string    `yaml:"refuseWhen,omitempty"`
	RetireOld      RetireOld `yaml:"retireOld,omitempty"`
}

// RetireOld deactivates a superseded catalog. Deactivating it is how its
// resources go away: updateMode FULL is rejected as unsupported, and MERGE's
// removal semantics are documented nowhere, so republishing without the
// unwanted resource cannot be relied on to remove it.
type RetireOld struct {
	Enabled        string `yaml:"enabled"`
	CatalogID      string `yaml:"catalogId"`
	DescriptorName string `yaml:"descriptorName"`
	IsActive       bool   `yaml:"isActive"`
	UpdateMode     string `yaml:"updateMode"`
	CatalogType    string `yaml:"catalogType"`
}

// LoadSpec reads and parses the pipeline definition at path inside files.
// It takes an embed.FS rather than a bare path so callers always read the
// copy a binary was built with (see Files in pipeline_files.go), never one
// edited on disk after the fact.
func LoadSpec(files embed.FS, path string) (Spec, error) {
	data, err := files.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("read %s: %w", path, err)
	}

	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return spec, nil
}
