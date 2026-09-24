package pipeline

// schema.go holds every pipeline file to a published contract.
//
// A pipeline file is a program, and until it runs, a mistake in it is
// invisible: a misspelled key parses "successfully" and the rule it was meant
// to state simply never fires. This file makes that a STARTUP error instead --
// the crawler refuses to come up, naming the exact path that is wrong, rather
// than failing at midnight against a live upstream.
//
// The contract lives in schema/ as JSON Schema, and a file says which version
// it was written against:
//
//	schemaRef:
//	  uses: publish.oan/CataloguePipeline/v1
//
// THREE ARTIFACTS MUST AGREE and this is worth stating plainly, because it is
// the cost of validating at all: the YAML, the Go structs in spec.go, and the
// JSON Schema. A key can exist in the schema but not the struct (parsed, then
// silently dropped) or in the struct but not the schema (rejected though the
// engine would happily read it). Validation catches NEITHER.
//
// What catches them is TestContractAndStructAgree in schema_test.go: the
// fixture must both validate against the contract and survive UnmappedKeys.
// That guard is only as good as the fixture's coverage -- a key neither the
// fixture nor any real pipeline uses can still drift unnoticed, and the first
// provider to reach for it finds out.

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

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
