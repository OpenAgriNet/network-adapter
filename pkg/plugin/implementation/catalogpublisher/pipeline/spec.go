// spec.go types and parses mandi-price-agmarket.yaml itself. The engine that
// runs a CataloguePipeline doesn't exist yet (see the file's own header
// comment), so this is deliberately just the YAML<->struct mapping the
// polling layer will need to validate and read the file with -- not a
// pipeline executor. Field names and shapes come straight from the file, not
// from what would be convenient to build later.
package pipeline

import (
	"encoding/json"
	"sort"
	"sync"

	"embed"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"strings"

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
	// SchemaRef names the contract this file is held to. LoadSpec validates
	// against it before returning, so a file that does not match is refused
	// at startup rather than failing mid-run.
	SchemaRef SchemaRef `yaml:"schemaRef"`

	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   Metadata         `yaml:"metadata"`
	Schedule   Schedule         `yaml:"schedule"`
	Inputs     map[string]Input `yaml:"inputs"`
	Upstream   Upstream         `yaml:"upstream"`
	Pipeline   []Step           `yaml:"pipeline"`
	Catalog    Catalog          `yaml:"catalog"`
	Publish    Publish          `yaml:"publish"`
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
	BaseURL string `yaml:"baseUrl"`

	// AllowCleartext permits an http:// upstream that is not loopback.
	//
	// Off by default, because the credential exchange and every token after
	// it travel over this address. It exists because some upstreams offer no
	// TLS at all -- Agmarknet answers on neither 443 nor its own port over
	// https, measured -- and a rule that makes a real integration impossible
	// gets worked around rather than obeyed.
	//
	// Setting it is a DELIBERATE, reviewable decision recorded in the
	// pipeline file, not an accident of an unset variable, and every run logs
	// a warning while it is on.
	AllowCleartext bool        `yaml:"allowCleartext,omitempty"`
	Auth           Auth        `yaml:"auth"`
	Errors         []ErrorRule `yaml:"errors"`
	Guards         Guards      `yaml:"guards"`
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
// Left/Right/On/Type/Carry, derive sets Field/Rules/Then, dedupe sets Key,
// const sets Records --
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

	// Records is a const step's output, written in the file. It is how a
	// pipeline with no upstream -- a catalogue whose content is fixed --
	// still hands the catalog block a collection to group.
	Records []map[string]any `yaml:"records,omitempty"`

	Method      string `yaml:"method,omitempty"`
	ContentType string `yaml:"contentType,omitempty"`
	Body        string `yaml:"body,omitempty"`
	When        string `yaml:"when,omitempty"`
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

// Chunk is the geometry-budget split described in CatalogGeometryBudget's
// comment in steps.go -- Budget here is that same cap, read from the file
// rather than duplicated as a second constant.
type Chunk struct {
	Budget int    `yaml:"budget"`
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

	// AddressHint is not read from the file: the run fills it from the
	// pipeline's own publishUrl input, so an unset address names the flag and
	// env THIS pipeline reads rather than another's.
	AddressHint string `yaml:"-"`
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
// copy a binary was built with (see pkg/plugin/implementation/publishpipelines.go),
// never one edited on disk after the fact.
func LoadSpec(files embed.FS, path string) (Spec, error) {
	data, err := files.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("read %s: %w", path, err)
	}

	// Validated BEFORE parsing into Spec, and against the raw document rather
	// than the struct. Validating the struct would validate what survived
	// parsing -- and a key that did not survive is precisely the mistake this
	// is here to catch.
	if err := Validate(files, path); err != nil {
		return Spec{}, err
	}

	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return spec, nil
}

// Files is a capability's pipeline definition: the YAML, and the mappings it
// references, embedded in the binary.
type Files struct {
	// FS and Path locate the pipeline YAML inside the binary. The mappings
	// are expected in a `mappings/` directory alongside it, because that is
	// what a file's `mapping:` references are relative to.
	FS   embed.FS
	Path string

	// RegistryPath is the repo-relative path the registry's publish action is
	// expected to name, e.g.
	// "pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml".
	//
	// The gate compares the registry's answer against this rather than
	// deriving it from the capability's name. Deriving it would mean guessing
	// a filesystem layout and running a pipeline the registry never
	// sanctioned; comparing means a registry pointing somewhere else is
	// refused rather than silently served by whatever this binary embeds.
	RegistryPath string
}

// Catalogue is one rendered catalogue document and the identity it carries.
type Catalogue struct {
	// Slug names the file on disk and distinguishes catalogues within a run.
	Slug string

	// CatalogID is the network-facing identity the document publishes under.
	CatalogID string

	Content []byte
}

// UnmappedKeys returns the dotted paths the file declares that Spec has no
// field for, deepest included -- an empty result means every key in the file
// reaches code.
//
// It works by round-tripping: Spec marshalled back to YAML yields exactly the
// keys Spec knows how to hold, which the file's keys must be a subset of.
//
// A key is reported only when its PARENT survived the round trip. A value the
// file leaves empty, or a field carrying omitempty, legitimately does not come
// back, and flagging those would drown the real finding in noise.
func UnmappedKeys(files embed.FS, path string) ([]string, error) {
	raw, err := files.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var fromFile map[string]any
	if err := yaml.Unmarshal(raw, &fromFile); err != nil {
		return nil, fmt.Errorf("parsing %s as a bare map: %w", path, err)
	}

	// Parsed directly rather than through LoadSpec, which VALIDATES. This
	// check has to work on a file the contract rejects -- catching what the
	// contract cannot is the whole reason it still exists -- so running it
	// through validation first would make it useless exactly when it is
	// needed.
	var spec Spec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	encoded, err := yaml.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("re-marshalling the parsed spec: %w", err)
	}
	var fromSpec map[string]any
	if err := yaml.Unmarshal(encoded, &fromSpec); err != nil {
		return nil, fmt.Errorf("parsing the re-marshalled spec: %w", err)
	}

	return missingKeys(nil, fromFile, fromSpec), nil
}

// missingKeys walks the file's structure against the round-tripped Spec's and
// returns the dotted paths the Spec cannot hold.
func missingKeys(path []string, file, spec any) []string {
	var missing []string

	switch fileNode := file.(type) {
	case map[string]any:
		specNode, ok := spec.(map[string]any)
		if !ok {
			return nil // parent did not survive; its children are not the finding
		}
		for key, fileChild := range fileNode {
			here := append(append([]string{}, path...), key)
			specChild, ok := specNode[key]
			if !ok {
				missing = append(missing, strings.Join(here, "."))
				continue
			}
			missing = append(missing, missingKeys(here, fileChild, specChild)...)
		}

	case []any:
		specNode, ok := spec.([]any)
		if !ok {
			return nil
		}
		// Sequence entries are heterogeneous -- one step declares `when`,
		// another declares `onError` -- so each entry is compared against the
		// Spec entry in the same position rather than against the first.
		for i, fileChild := range fileNode {
			here := append(append([]string{}, path...), fmt.Sprintf("[%d]", i))
			if i < len(specNode) {
				missing = append(missing, missingKeys(here, fileChild, specNode[i])...)
			}
		}
	}

	return missing
}

//go:embed schema/*.json
var schemaFS embed.FS

// SchemaRef names the contract a pipeline file was written against.
//
// A map rather than a bare string so it can gain keys -- a strictness flag, a
// deprecation note -- without every provider's file having to change shape.
type SchemaRef struct {
	Uses string `yaml:"uses"`
}

// Contracts this engine carries, by the id a file names in schemaRef.uses.
//
// The id is matched, never derived. Deriving it from a filename or a version
// field would mean guessing which rules a file is held to, and quietly holding
// it to the wrong ones.
var schemaFiles = map[string]string{
	"publish.oan/CataloguePipeline/v1": "schema/pipeline.v1.schema.json",
}

// APIVersionFor is the apiVersion that must accompany a contract.
//
// apiVersion and schemaRef.uses say the same thing in two places, which is a
// standing invitation to disagree. They are checked against each other rather
// than one being preferred silently -- the same failure this whole file exists
// to prevent, one level up.
var apiVersionFor = map[string]string{
	"publish.oan/CataloguePipeline/v1": "publish.oan/v1",
}

// compiled schemas, built once. Compiling is the expensive half and a process
// loads the same handful of contracts repeatedly.
var compiledSchemas = map[string]*jsonschema.Schema{}

// compiling guards compiledSchemas.
//
// The crawler runs one runner per binding key, each on its own goroutine, so
// two pipelines validating at the same time is the ordinary case rather than
// an edge one. An unguarded map write there is not a subtle race: Go detects
// it and kills the PROCESS with "fatal error: concurrent map writes", taking
// the crawl loops down with the publish ones.
var compiling sync.Mutex

// Validate holds the pipeline file at path against the contract it names.
//
// It reports EVERY violation it can see, not just the first: an operator
// fixing a file one error per run is an operator who stops reading the errors.
func Validate(files embed.FS, path string) error {
	raw, err := files.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return validateBytes(raw, path)
}

// validateBytes is Validate over bytes already in hand, so a caller holding a
// document (a test mutating a fixture, a future on-disk loader) runs exactly
// the same checks rather than a parallel approximation of them.
func validateBytes(raw []byte, path string) error {
	// Parsed twice on purpose: once into a bare document to validate the
	// file's own shape, and (by the caller) once into Spec. Validating the
	// STRUCT would validate what survived parsing, which is exactly the set
	// of mistakes this is meant to catch.
	var document any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	ref, err := schemaRefOf(document, path)
	if err != nil {
		return err
	}

	schema, err := schemaFor(ref)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// Through JSON, because YAML's number and key types are not the ones a
	// JSON Schema validator expects.
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("%s: could not be re-encoded for validation: %w", path, err)
	}
	var asJSON any
	if err := json.Unmarshal(encoded, &asJSON); err != nil {
		return fmt.Errorf("%s: could not be re-encoded for validation: %w", path, err)
	}

	if err := schema.Validate(asJSON); err != nil {
		var invalid *jsonschema.ValidationError
		if ok := asValidationError(err, &invalid); ok {
			return fmt.Errorf("%s does not match %s:\n%s", path, ref, violations(invalid))
		}
		return fmt.Errorf("%s does not match %s: %w", path, ref, err)
	}

	return checkAPIVersionAgrees(document, ref, path)
}

// schemaRefOf reads schemaRef.uses, and says what to write when it is absent.
//
// A file with no schemaRef is refused rather than validated against a guessed
// default: the whole point is that a file states which rules it is held to.
func schemaRefOf(document any, path string) (string, error) {
	root, ok := document.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s is not a YAML mapping", path)
	}
	block, ok := root["schemaRef"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s declares no schemaRef, so there is no contract to hold it to; add:\n"+
			"    schemaRef:\n      uses: %s", path, defaultContract())
	}
	uses, ok := block["uses"].(string)
	if !ok || strings.TrimSpace(uses) == "" {
		return "", fmt.Errorf("%s has a schemaRef with no `uses:`; it should name a contract, e.g. %s",
			path, defaultContract())
	}
	return strings.TrimSpace(uses), nil
}

// schemaFor compiles the contract, or says which ones exist.
func schemaFor(ref string) (*jsonschema.Schema, error) {
	compiling.Lock()
	defer compiling.Unlock()

	if schema, ok := compiledSchemas[ref]; ok {
		return schema, nil
	}

	file, ok := schemaFiles[ref]
	if !ok {
		return nil, fmt.Errorf("schemaRef.uses is %q, which this binary carries no contract for; it has %s",
			ref, knownContracts())
	}

	raw, err := schemaFS.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading the contract %s: %w", file, err)
	}
	document, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parsing the contract %s: %w", file, err)
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(ref, document); err != nil {
		return nil, fmt.Errorf("loading the contract %s: %w", file, err)
	}
	schema, err := compiler.Compile(ref)
	if err != nil {
		return nil, fmt.Errorf("compiling the contract %s: %w", file, err)
	}

	compiledSchemas[ref] = schema
	return schema, nil
}

// checkAPIVersionAgrees refuses a file whose apiVersion contradicts its
// schemaRef, rather than picking whichever one the code happens to read.
func checkAPIVersionAgrees(document any, ref, path string) error {
	root, ok := document.(map[string]any)
	if !ok {
		return nil
	}
	declared, present := root["apiVersion"].(string)
	if !present {
		return nil // optional; the schema decides whether it is required
	}
	want, known := apiVersionFor[ref]
	if !known || declared == want {
		return nil
	}
	return fmt.Errorf("%s declares apiVersion %q but schemaRef.uses %q, which expects apiVersion %q; "+
		"the two must agree, because a reader cannot tell which one the engine obeys",
		path, declared, ref, want)
}

// violations renders every leaf failure as one line, deepest path first, so
// the output reads as a list of things to fix.
func violations(err *jsonschema.ValidationError) string {
	lines := collectViolations(err, nil)
	sort.Strings(lines)

	var out strings.Builder
	for _, line := range lines {
		out.WriteString("  ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// collectViolations walks to the leaves: a parent error says "something below
// is wrong", which is not actionable, and the leaf says what.
func collectViolations(err *jsonschema.ValidationError, into []string) []string {
	if len(err.Causes) == 0 {
		location := strings.Join(err.InstanceLocation, ".")
		if location == "" {
			location = "(root)"
		}
		// Error(), not ErrorKind.LocalizedString: the latter takes a printer
		// and dereferences it, so passing nil panics. schemavalidator.go
		// reaches for Error() here for the same reason.
		message := err.Error()
		if line, _, found := strings.Cut(message, "\n"); found {
			message = line
		}
		// Error() prefixes the instance location; the location is already the
		// first column here, so the prefix would read twice.
		if _, rest, found := strings.Cut(message, ": "); found {
			message = rest
		}
		return append(into, fmt.Sprintf("%s: %s", location, strings.TrimSpace(message)))
	}
	for _, cause := range err.Causes {
		into = collectViolations(cause, into)
	}
	return into
}

// asValidationError unwraps to the validator's own error type.
func asValidationError(err error, target **jsonschema.ValidationError) bool {
	for err != nil {
		if typed, ok := err.(*jsonschema.ValidationError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func knownContracts() string {
	names := make([]string, 0, len(schemaFiles))
	for id := range schemaFiles {
		names = append(names, fmt.Sprintf("%q", id))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// defaultContract is the one to suggest when a file names none. The newest,
// because a file being written today should be written against it.
func defaultContract() string {
	names := make([]string, 0, len(schemaFiles))
	for id := range schemaFiles {
		names = append(names, id)
	}
	sort.Strings(names)
	return names[len(names)-1]
}
