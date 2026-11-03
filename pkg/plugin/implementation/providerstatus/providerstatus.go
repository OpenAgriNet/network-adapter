package providerstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// activeProviderLister is the half of the registry this step needs. Narrowed so
// a registry that cannot list providers fails at startup, not per request.
type activeProviderLister interface {
	ActiveProviderIDs(ctx context.Context) ([]string, error)
}

// step keeps providers the registry does not mark active out of publish and
// discover.
type step struct {
	registry activeProviderLister
}

// New fails when the registry cannot list providers: the gate would otherwise
// wave every publish through.
func New(registry definition.RegistryLookup) (definition.Step, error) {
	lister, ok := registry.(activeProviderLister)
	if !ok {
		return nil, fmt.Errorf("invalid config: the Registry plugin cannot list active providers, " +
			"so providerstatus would let every provider through")
	}
	return &step{registry: lister}, nil
}

func (s *step) Run(ctx *model.StepContext) error {
	root, becknContext, becknErr := model.ExtractContext(ctx.Body)
	if becknErr != nil {
		return model.WrapExtractContextErr("could not read the request context", becknErr)
	}

	// The router picks the target from the URL path, not from context.action,
	// so the path decides what runs here too. Deciding from the body would let a
	// request to /discover or /publish with any other action skip the step.
	if ctx.Request == nil || ctx.Request.URL == nil {
		return fmt.Errorf("providerstatus: the step has no request, so its path cannot be read")
	}
	endpoint := strings.ToLower(path.Base(ctx.Request.URL.Path))
	allowed, gated := actionsForEndpoint[endpoint]
	if !gated {
		return nil
	}

	if err := refuseLookalikeKeys(root, "the request body"); err != nil {
		return model.NewBadReqErr("", err)
	}
	if err := refuseLookalikeKeys(becknContext, "context"); err != nil {
		return model.NewBadReqErr("", err)
	}
	action, _ := becknContext["action"].(string)
	if !slices.Contains(allowed, strings.ToLower(strings.TrimSpace(action))) {
		return model.NewBadReqErr("", fmt.Errorf(
			"context.action %q does not match the request path %q: want one of %q",
			action, ctx.Request.URL.Path, allowed))
	}
	if endpoint == "publish" {
		return s.checkPublish(ctx)
	}
	return s.narrowDiscover(ctx)
}

// actionsForEndpoint maps the last segment of the request path to the
// context.action values a body sent there may carry. The segment is the
// router's endpoint under either module base path (`/` or
// `/_internal/network/`). Paths not listed pass through untouched.
var actionsForEndpoint = map[string][]string{
	"publish":  {"catalog/publish", "publish"},
	"discover": {"discover"},
}

func (s *step) checkPublish(ctx *model.StepContext) error {
	named, err := providerIDsOf(ctx.Body)
	if err != nil {
		return model.NewBadReqErr("", err)
	}

	active, err := s.registry.ActiveProviderIDs(ctx)
	if err != nil {
		// Not a verdict on the provider: the registry could not be asked, and an
		// unanswered question must never read as "active".
		return fmt.Errorf("could not check provider status: %w", err)
	}

	// The whole batch is refused on one bad provider, so a partial publish never
	// leaves the sender guessing which catalogs landed.
	for _, id := range named {
		if !slices.Contains(active, id) {
			return model.NewBadReqErr("", fmt.Errorf(
				"provider %q is not an active provider in the registry", id))
		}
	}
	return nil
}

func (s *step) narrowDiscover(ctx *model.StepContext) error {
	active, err := s.registry.ActiveProviderIDs(ctx)
	if err != nil {
		return fmt.Errorf("could not check provider status: %w", err)
	}
	narrowed, err := narrowFilter(ctx.Body, active)
	if err != nil {
		return err
	}
	ctx.Body = narrowed
	return nil
}

// providerIDPattern is the registry's own provider id pattern. Ids that match
// it hold no quote or backslash, so they go into a path expression unescaped.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{2,252}$`)

// catalogsRoot is how the discovery service requires every filter to begin.
var catalogsRoot = regexp.MustCompile(`^\$\.catalogs\[\*\]\s*\?\s*\(`)

// narrowFilter restricts a discover to the given provider ids by AND-ing an
// allowlist onto message.intent.filters. It refuses what it cannot place
// safely rather than guessing a rewrite.
func narrowFilter(body []byte, ids []string) ([]byte, error) {
	allow := `@.provider.id == ""` // provider ids are never empty: matches nothing
	if len(ids) > 0 {
		tests := make([]string, len(ids))
		for i, id := range ids {
			if !providerIDPattern.MatchString(id) {
				return nil, model.NewBadReqErr("", fmt.Errorf(
					"provider id %q from the registry is not a valid id, so the discover cannot be narrowed", id))
			}
			tests[i] = `@.provider.id == "` + id + `"`
		}
		allow = strings.Join(tests, " || ")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, model.NewBadReqErr("", fmt.Errorf("could not read the discover body: %w", err))
	}

	message, err := objectAt(doc, "message")
	if err != nil {
		return nil, err
	}
	if err := refuseLookalikeKeys(message, "message"); err != nil {
		return nil, model.NewBadReqErr("", err)
	}
	intent, err := objectAt(message, "intent")
	if err != nil {
		return nil, err
	}
	if err := refuseLookalikeKeys(intent, "message.intent"); err != nil {
		return nil, model.NewBadReqErr("", err)
	}

	var expression string
	if raw, present := intent["filters"]; !present || raw == nil {
		expression = "$.catalogs[*] ? (" + allow + ")"
	} else {
		filters, ok := raw.(map[string]any)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf("message.intent.filters must be an object"))
		}
		if err := refuseLookalikeKeys(filters, "message.intent.filters"); err != nil {
			return nil, model.NewBadReqErr("", err)
		}
		// Discovery reads an empty type as jsonpath and ignores case and padding.
		grammar, _ := filters["type"].(string)
		if filters["type"] != nil && filters["type"] != grammar {
			return nil, model.NewBadReqErr("", fmt.Errorf("message.intent.filters.type must be a string"))
		}
		if g := strings.ToLower(strings.TrimSpace(grammar)); g != "" && g != "jsonpath" {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"message.intent.filters.type must be %q to be narrowed to active providers", "jsonpath"))
		}
		original, ok := filters["expression"].(string)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf("message.intent.filters.expression must be a string"))
		}
		predicate, err := predicateOf(original)
		if err != nil {
			return nil, model.NewBadReqErr("", err)
		}
		expression = "$.catalogs[*] ? ( (" + predicate + ") && (" + allow + ") )"
	}
	intent["filters"] = map[string]any{"type": "jsonpath", "expression": expression}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("could not write the discover body: %w", err)
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// objectAt returns parent[key] as an object, creating it when absent.
func objectAt(parent map[string]any, key string) (map[string]any, error) {
	raw, present := parent[key]
	if !present || raw == nil {
		child := map[string]any{}
		parent[key] = child
		return child, nil
	}
	child, ok := raw.(map[string]any)
	if !ok {
		return nil, model.NewBadReqErr("", fmt.Errorf("%s must be an object", key))
	}
	return child, nil
}

// predicateOf returns the text inside the outer parentheses of an expression of
// the form `$.catalogs[*] ? ( ... )`. Parentheses inside double-quoted string
// literals are skipped. Anything after the closing parenthesis, or an
// unbalanced expression, is refused.
func predicateOf(expr string) (string, error) {
	expr = strings.TrimSpace(expr)
	loc := catalogsRoot.FindStringIndex(expr)
	if loc == nil {
		return "", fmt.Errorf("message.intent.filters.expression must have the form " +
			`$.catalogs[*] ? ( ... )`)
	}
	start := loc[1] // just past the opening parenthesis
	depth, inString := 1, false
	for i := start; i < len(expr); i++ {
		c := expr[i]
		switch {
		case inString:
			if c == '\\' {
				i++ // skip the escaped character
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '\'' || c == '\\' || (c == '/' && i+1 < len(expr) && expr[i+1] == '*'):
			// PostgreSQL jsonpath has comments, no single-quoted strings, and
			// escapes in unquoted key names (so a backslash can make a quote a
			// different token than it looks); this scanner does not model them,
			// so refuse rather than mis-match.
			return "", fmt.Errorf("message.intent.filters.expression must not use single quotes, " +
				"backslashes or /* comments */ outside double-quoted strings")
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				if strings.TrimSpace(expr[i+1:]) != "" {
					return "", fmt.Errorf("message.intent.filters.expression has text after the closing parenthesis of $.catalogs[*] ? ( ... )")
				}
				return expr[start:i], nil
			}
		}
	}
	return "", fmt.Errorf("message.intent.filters.expression has unbalanced parentheses or an unterminated string")
}

// readKeys are the JSON keys this step reads, anywhere it reads them.
var readKeys = []string{
	"message", "intent", "filters", "catalogs", "provider", "id",
	"type", "expression", "context", "action",
}

// refuseLookalikeKeys refuses an object holding a key that differs from one
// this step reads only by case or Unicode folding ("ID", "Filters",
// "catalog\u017f"). Go's struct decoding would match such a key, other readers
// would not, so the step and the service behind it could read different
// fields. A key spelled exactly is fine; so is any key nobody here reads.
func refuseLookalikeKeys(obj map[string]any, where string) error {
	for key := range obj {
		for _, name := range readKeys {
			if key != name && strings.EqualFold(key, name) {
				return fmt.Errorf("%s holds the key %q, which differs from %q only by case or Unicode folding: "+
					"spell it %q or remove it", where, key, name, name)
			}
		}
	}
	return nil
}

// refuseDuplicateKeys refuses a JSON document in which any object holds the
// same key twice. Go keeps the last value, other decoders keep the first, so a
// forwarded body with a duplicate could name one provider here and another
// downstream. The body must already be known to be valid JSON.
func refuseDuplicateKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				if _, dup := seen[key]; dup {
					return fmt.Errorf("the key %q appears twice in one object", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // the closing delimiter
		return err
	}
	return walk()
}

// providerIDsOf returns the provider of every catalog in a publish. A catalog
// that names none is an error rather than a skip: an unnamed provider cannot be
// checked, so it must not slip through.
//
// Keys are read exactly, never through a struct: a struct would also match
// "ID" or a folded "catalogs", and the body is forwarded as is.
func providerIDsOf(body []byte) ([]string, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("could not read the publish body: %w", err)
	}
	if err := refuseDuplicateKeys(body); err != nil {
		return nil, fmt.Errorf("could not read the publish body: %w", err)
	}
	message, ok := doc["message"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the publish carries no message object: set message.catalogs")
	}
	if err := refuseLookalikeKeys(message, "message"); err != nil {
		return nil, err
	}
	catalogs, _ := message["catalogs"].([]any)
	if len(catalogs) == 0 {
		return nil, fmt.Errorf("the publish carries no catalogs: set message.catalogs")
	}
	ids := make([]string, 0, len(catalogs))
	for i, raw := range catalogs {
		catalog, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message.catalogs[%d] must be an object", i)
		}
		if err := refuseLookalikeKeys(catalog, fmt.Sprintf("message.catalogs[%d]", i)); err != nil {
			return nil, err
		}
		provider, _ := catalog["provider"].(map[string]any)
		if err := refuseLookalikeKeys(provider, fmt.Sprintf("message.catalogs[%d].provider", i)); err != nil {
			return nil, err
		}
		id, _ := provider["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("catalog %d names no provider: set message.catalogs[%d].provider.id", i, i)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
