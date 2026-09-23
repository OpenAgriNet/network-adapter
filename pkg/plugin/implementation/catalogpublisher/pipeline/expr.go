package pipeline

// expr.go evaluates the two kinds of expression a pipeline file contains.
//
//  1. `${...}` interpolation -- "${inputs.fromDate}", "${state.code}",
//     "catalog:mandi-price:${slug}". Resolved against a run context: the
//     resolved inputs, the auth token, each step's output, the current loop
//     variable, and a few built-ins.
//
//  2. Bare JSONata -- "$count(commodities) = 0", "latitude = longitude".
//     Used where the file needs a predicate or a computed value over one
//     record, and used verbatim so a reader who knows JSONata needs no second
//     language.
//
// The two are deliberately distinct. Interpolation reaches OUTSIDE the record
// being processed (inputs, other steps' outputs); JSONata reaches INSIDE it.
// A file that blurs them reads as though a step could see anything.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jsonata-go/jsonata"
)

// evaluating serialises every JSONata evaluation in this package.
//
// It has to exist, and it has to be this wide, for the same reason
// jsonmapper's own lock does: the library keeps its built-in functions in a
// package-level frame, and applying one writes onto that shared state. Two
// concurrent evaluations race even when they are different expressions.
//
// This lock cannot protect against jsonmapper evaluating at the same time --
// that is a different package with a different lock over the same library
// state. Which is exactly why a step's concurrency is refused above 1: a
// parallel forEach would run mapping transforms and these evaluations at once.
// See runStep.
var evaluating sync.Mutex

// exprCache holds compiled expressions. Compiling is the expensive half and a
// pipeline evaluates the same handful of expressions once per record.
type exprCache struct {
	mu       sync.Mutex
	instance jsonata.JSONataInstance
	compiled map[string]jsonata.Expression
}

func newExprCache() (*exprCache, error) {
	instance, err := jsonata.OpenLatest()
	if err != nil {
		return nil, fmt.Errorf("opening jsonata: %w", err)
	}
	return &exprCache{instance: instance, compiled: map[string]jsonata.Expression{}}, nil
}

func (c *exprCache) expression(expr string) (jsonata.Expression, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if compiled, ok := c.compiled[expr]; ok {
		return compiled, nil
	}
	compiled, err := c.instance.Compile(expr, false)
	if err != nil {
		return nil, fmt.Errorf("compiling %q: %w", expr, err)
	}
	c.compiled[expr] = compiled
	return compiled, nil
}

// evaluate runs a JSONata expression over one record and returns the result.
//
// A panic inside the library is converted to an error, as jsonmapper does and
// for the same reason: a panic here would take down whatever goroutine the
// pipeline is running on, for what is really "this expression could not be
// applied to this record".
func (c *exprCache) evaluate(expr string, record any) (result any, err error) {
	compiled, err := c.expression(expr)
	if err != nil {
		return nil, err
	}
	document, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encoding the record for %q: %w", expr, err)
	}

	evaluating.Lock()
	defer evaluating.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = nil, fmt.Errorf("jsonata evaluation of %q panicked: %v", expr, recovered)
		}
	}()

	out, err := compiled.Evaluate(document, nil)
	if err != nil {
		return nil, fmt.Errorf("evaluating %q: %w", expr, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(out, &decoded); err != nil {
		return nil, fmt.Errorf("decoding the result of %q: %w", expr, err)
	}
	return decoded, nil
}

// truthy applies a JSONata predicate. Anything JSONata calls false -- absent,
// null, false, "" -- is false; everything else is true.
//
// An expression that fails to evaluate is an ERROR, never false. A predicate
// that silently reads as false is how an exclusion rule stops excluding, or a
// guard stops guarding, with nothing to see.
func (c *exprCache) truthy(expr string, record any) (bool, error) {
	value, err := c.evaluate(expr, record)
	if err != nil {
		return false, err
	}
	switch typed := value.(type) {
	case nil:
		return false, nil
	case bool:
		return typed, nil
	case string:
		return typed != "", nil
	case float64:
		return typed != 0, nil
	case []any:
		return len(typed) > 0, nil
	case map[string]any:
		return len(typed) > 0, nil
	default:
		return true, nil
	}
}

// runContext is everything a `${...}` can name.
type runContext struct {
	// inputs are the pipeline's resolved inputs.
	inputs map[string]string

	// token is the exchanged upstream credential, reachable as ${auth.token}.
	token string

	// outputs are each completed step's result, keyed by its `out:` name.
	outputs map[string]any

	// locals are the current loop variable (`as: state` gives ${state.…}) and
	// any per-group values the catalogue builder adds (${slug}, ${group.…}).
	locals map[string]any
}

func newRunContext(inputs map[string]string, token string) *runContext {
	return &runContext{
		inputs:  inputs,
		token:   token,
		outputs: map[string]any{},
		locals:  map[string]any{},
	}
}

// with returns a copy carrying one extra local, so a loop iteration cannot
// leak its variable into the next.
func (c *runContext) with(name string, value any) *runContext {
	locals := make(map[string]any, len(c.locals)+1)
	for k, v := range c.locals {
		locals[k] = v
	}
	locals[name] = value
	return &runContext{inputs: c.inputs, token: c.token, outputs: c.outputs, locals: locals}
}

// placeholder matches ${...}, non-greedy so two in one string stay separate.
var placeholder = regexp.MustCompile(`\$\{([^}]+)\}`)

// interpolate replaces every ${...} in text.
//
// An unresolvable reference is an ERROR, not an empty string. A silently empty
// interpolation reaches the upstream as a missing parameter or lands in a
// catalogId as a hole, and both read as data problems far from the typo.
func (c *runContext) interpolate(text string) (string, error) {
	var failed error
	out := placeholder.ReplaceAllStringFunc(text, func(match string) string {
		path := strings.TrimSpace(placeholder.FindStringSubmatch(match)[1])
		value, err := c.lookup(path)
		if err != nil {
			if failed == nil {
				failed = err
			}
			return match
		}
		return renderScalar(value)
	})
	return out, failed
}

// interpolateAny walks a nested with:-block, interpolating every string it
// holds, so `local: {token: "${auth.token}", stateCode: "${state.code}"}`
// resolves without the caller knowing its shape.
func (c *runContext) interpolateAny(value any) (any, error) {
	switch typed := value.(type) {
	case string:
		return c.interpolate(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			resolved, err := c.interpolateAny(child)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			resolved, err := c.interpolateAny(child)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	default:
		return value, nil
	}
}

// lookup resolves one dotted path.
func (c *runContext) lookup(path string) (any, error) {
	// Built-ins first: they are values, not lookups, and a pipeline that
	// declared an input called `uuid` should not shadow them silently.
	switch path {
	case "uuid":
		return uuid.NewString(), nil
	case "now.rfc3339":
		return time.Now().UTC().Format(time.RFC3339), nil
	case "auth.token":
		if c.token == "" {
			return nil, fmt.Errorf("${auth.token} used before a token was exchanged")
		}
		return c.token, nil
	}

	head, rest, _ := strings.Cut(path, ".")
	switch head {
	case "inputs":
		value, ok := c.inputs[rest]
		if !ok {
			return nil, fmt.Errorf("${inputs.%s} is not a declared input", rest)
		}
		return value, nil
	case "metadata":
		// metadata is resolved by the caller before a run starts; anything
		// still holding ${metadata...} here is a mistake worth naming.
		return nil, fmt.Errorf("${%s} is not available during a run", path)
	}

	// A step output, or a local (loop variable, group value).
	root, ok := c.locals[head]
	if !ok {
		root, ok = c.outputs[head]
	}
	if !ok {
		return nil, fmt.Errorf("${%s} names nothing: no input, step output or loop variable called %q", path, head)
	}
	if rest == "" {
		return root, nil
	}
	return traverse(root, rest, path)
}

// traverse walks the remaining dotted path into a decoded JSON value.
func traverse(value any, path, full string) (any, error) {
	for _, field := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("${%s}: %q is not an object", full, field)
		}
		value, ok = object[field]
		if !ok {
			return nil, fmt.Errorf("${%s}: no field %q", full, field)
		}
	}
	return value, nil
}

// renderScalar turns a resolved value into the text that replaces a ${...}.
//
// Floats are rendered without a trailing ".0" because every number arriving
// from JSON is a float64, and a marketId reaching an upstream as "101.0" is
// rejected as a bad identifier.
func renderScalar(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(encoded)
	}
}

// interpolatePredicate resolves the ${...} in a JSONata predicate by
// substituting each one as a LITERAL, not as raw text.
//
// This is not a style choice, it is the difference between a rule working and
// silently never firing. mandi's exclusion reads:
//
//	when: "${inputs.withoutGeometry} = 'skip' and coordinateQuality != 'ok'"
//
// Substituted as raw text that becomes `skip = 'skip'`, where the bare `skip`
// is a PATH into the record being tested, not a string. The record has no such
// field, so the comparison is false, so the rule never excludes anything and
// --without-geometry=skip quietly does nothing. Substituted as a literal it
// becomes `"skip" = 'skip'`, which is true.
//
// Measured, not reasoned about: `skip = 'skip'` evaluates to false under the
// jsonata build this repo pins.
func interpolatePredicate(scope *runContext, when string) (string, error) {
	if !strings.Contains(when, "${") {
		return when, nil
	}

	var failed error
	out := placeholder.ReplaceAllStringFunc(when, func(match string) string {
		path := strings.TrimSpace(placeholder.FindStringSubmatch(match)[1])
		value, err := scope.lookup(path)
		if err != nil {
			if failed == nil {
				failed = err
			}
			return match
		}
		literal, err := exprLiteral(value)
		if err != nil {
			if failed == nil {
				failed = fmt.Errorf("${%s}: %w", path, err)
			}
			return match
		}
		return literal
	})
	return out, failed
}

// catalogLiteral writes one resolved value as JSONata source.
//
// JSON literal syntax is JSONata literal syntax for the scalars, escaping
// included, so a value carrying a quote cannot break out of the expression it
// travels in. A composite has no one obvious spelling and is refused rather
// than guessed at.
func exprLiteral(value any) (string, error) {
	switch value.(type) {
	case nil, bool, string, float64, int, int64:
	default:
		return "", fmt.Errorf("a %T cannot be written into an expression as a literal", value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("value could not be written as a literal: %w", err)
	}
	return string(encoded), nil
}
