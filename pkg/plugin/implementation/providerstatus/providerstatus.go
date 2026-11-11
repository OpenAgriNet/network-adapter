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
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// activeProviderLister is the half of the registry this step needs. Narrowed so
// a registry that cannot list providers fails at startup, not per request.
type activeProviderLister interface {
	ActiveProviderIDs(ctx context.Context, participantType, role string) ([]string, error)
}

// Config is the step's plugin config. BuildConfig fills the defaults, which
// reproduce the step's original fixed behaviour.
type Config struct {
	// PublishEndpoint is the last request path segment that is gated. A body
	// sent there must carry it, or "catalog/" + it, as context.action.
	PublishEndpoint string
	// DiscoverEndpoint is the last request path segment whose filter is
	// narrowed. A body sent there must carry it as context.action.
	DiscoverEndpoint string
	// ProviderField is the dotted path to the provider id inside each catalog.
	ProviderField string
	// ParticipantType and ParticipantRole restrict which active participants
	// count as providers; empty means any.
	ParticipantType string
	ParticipantRole string
	// RefreshInterval is how often the active list is re-read from the
	// registry: the longest a status change takes to apply.
	RefreshInterval time.Duration
	// MaxStaleness is how old the list may get, while refreshes fail, before
	// requests are refused.
	MaxStaleness time.Duration
}

// The config keys, and the defaults that keep today's behaviour.
const (
	keyPublishEndpoint  = "publishEndpoint"
	keyDiscoverEndpoint = "discoverEndpoint"
	keyProviderField    = "providerField"
	keyParticipantType  = "participantType"
	keyParticipantRole  = "participantRole"
	keyRefreshInterval  = "refreshInterval"
	keyMaxStaleness     = "maxStaleness"

	defaultPublishEndpoint  = "publish"
	defaultDiscoverEndpoint = "discover"
	defaultProviderField    = "provider.id"
	defaultRefreshInterval  = 10 * time.Second
	defaultMaxStaleness     = 60 * time.Second
	minRefreshInterval      = time.Second
)

var (
	endpointPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// providerFieldPattern is strict because the field is spliced into a
	// jsonpath: plain dotted names cannot carry a quote, bracket or operator.
	providerFieldPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	// The registry's Participant schema enums.
	participantTypes = []string{"node", "upstream"}
	participantRoles = []string{"consumer", "provider", "network"}
)

// BuildConfig reads the plugin config. Every key is optional; an unknown key
// is an error, so a typo cannot silently fall back to a default on a check
// that gates publishes.
func BuildConfig(config map[string]string) (Config, error) {
	cfg := Config{
		PublishEndpoint:  defaultPublishEndpoint,
		DiscoverEndpoint: defaultDiscoverEndpoint,
		ProviderField:    defaultProviderField,
		RefreshInterval:  defaultRefreshInterval,
		MaxStaleness:     defaultMaxStaleness,
	}
	for key, value := range config {
		switch key {
		case keyPublishEndpoint:
			cfg.PublishEndpoint = value
		case keyDiscoverEndpoint:
			cfg.DiscoverEndpoint = value
		case keyProviderField:
			cfg.ProviderField = value
		case keyParticipantType:
			cfg.ParticipantType = value
		case keyParticipantRole:
			cfg.ParticipantRole = value
		case keyRefreshInterval, keyMaxStaleness:
			d, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, fmt.Errorf("invalid config: providerstatus %s %q is not a duration", key, value)
			}
			if key == keyRefreshInterval {
				cfg.RefreshInterval = d
			} else {
				cfg.MaxStaleness = d
			}
		default:
			return Config{}, fmt.Errorf("invalid config: providerstatus has no key %q", key)
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	for key, value := range map[string]string{
		keyPublishEndpoint:  c.PublishEndpoint,
		keyDiscoverEndpoint: c.DiscoverEndpoint,
	} {
		if !endpointPattern.MatchString(value) {
			return fmt.Errorf("invalid config: providerstatus %s %q must match %s", key, value, endpointPattern)
		}
	}
	if c.PublishEndpoint == c.DiscoverEndpoint {
		return fmt.Errorf("invalid config: providerstatus %s and %s are both %q",
			keyPublishEndpoint, keyDiscoverEndpoint, c.PublishEndpoint)
	}
	// Crossed, the router's real publish would only have its filter narrowed,
	// never gated.
	if c.PublishEndpoint == defaultDiscoverEndpoint || c.DiscoverEndpoint == defaultPublishEndpoint {
		return fmt.Errorf("invalid config: providerstatus %s may not be %q nor %s %q",
			keyPublishEndpoint, defaultDiscoverEndpoint, keyDiscoverEndpoint, defaultPublishEndpoint)
	}
	if !providerFieldPattern.MatchString(c.ProviderField) {
		return fmt.Errorf("invalid config: providerstatus %s %q must match %s",
			keyProviderField, c.ProviderField, providerFieldPattern)
	}
	// A segment folding onto another read key would make every body hold a
	// lookalike key, so every request would be a 400.
	field := strings.Split(c.ProviderField, ".")
	for _, segment := range field {
		for _, name := range append(slices.Clone(fixedKeys), field...) {
			if segment != name && strings.EqualFold(segment, name) {
				return fmt.Errorf("invalid config: providerstatus %s segment %q differs from the read key %q only by case",
					keyProviderField, segment, name)
			}
		}
	}
	if c.ParticipantType != "" && !slices.Contains(participantTypes, c.ParticipantType) {
		return fmt.Errorf("invalid config: providerstatus %s %q must be one of %q",
			keyParticipantType, c.ParticipantType, participantTypes)
	}
	if c.ParticipantRole != "" && !slices.Contains(participantRoles, c.ParticipantRole) {
		return fmt.Errorf("invalid config: providerstatus %s %q must be one of %q",
			keyParticipantRole, c.ParticipantRole, participantRoles)
	}
	if c.RefreshInterval < minRefreshInterval {
		return fmt.Errorf("invalid config: providerstatus %s %s must be at least %s",
			keyRefreshInterval, c.RefreshInterval, minRefreshInterval)
	}
	if c.MaxStaleness < c.RefreshInterval {
		return fmt.Errorf("invalid config: providerstatus %s %s must be at least %s %s",
			keyMaxStaleness, c.MaxStaleness, keyRefreshInterval, c.RefreshInterval)
	}
	return nil
}

// step keeps providers the registry does not mark active out of publish and
// discover.
type step struct {
	list *refresher
	cfg  Config
	// actions maps each gated path segment to the context.action values a body
	// sent there may carry.
	actions map[string][]string
	// field is cfg.ProviderField split into its keys.
	field []string
	// keys are the JSON keys this step reads, anywhere it reads them.
	keys []string
}

// New fails when the registry cannot list providers: the gate would otherwise
// wave every publish through. It validates cfg itself, so a Config built
// without BuildConfig cannot skip the checks. It loads the active list once and
// keeps it fresh in the background; the returned func stops that. A registry
// that cannot be read at startup does not stop startup: gated requests are
// refused until a refresh succeeds.
func New(registry definition.RegistryLookup, cfg Config) (definition.Step, func() error, error) {
	lister, ok := registry.(activeProviderLister)
	if !ok {
		return nil, nil, fmt.Errorf("invalid config: the Registry plugin cannot list active providers, " +
			"so providerstatus would let every provider through")
	}
	if err := cfg.validate(); err != nil {
		return nil, nil, err
	}
	list := newRefresher(lister, cfg.ParticipantType, cfg.ParticipantRole, cfg.RefreshInterval, cfg.MaxStaleness)
	if err := list.refresh(context.Background()); err != nil {
		log.Errorf(context.Background(), err, "providerstatus: could not load the active provider list at startup")
	}
	list.start()
	field := strings.Split(cfg.ProviderField, ".")
	return &step{
		list: list,
		cfg:  cfg,
		actions: map[string][]string{
			cfg.PublishEndpoint:  {"catalog/" + cfg.PublishEndpoint, cfg.PublishEndpoint},
			cfg.DiscoverEndpoint: {cfg.DiscoverEndpoint},
		},
		field: field,
		keys:  append(slices.Clone(fixedKeys), field...),
	}, list.close, nil
}

func (s *step) Run(ctx *model.StepContext) error {
	// The router picks the target from the URL path, not from context.action,
	// so the path decides what runs here too. Deciding from the body would let a
	// request to /discover or /publish with any other action skip the step. The
	// path is read first, so a bodyless GET or DELETE elsewhere passes untouched.
	if ctx.Request == nil || ctx.Request.URL == nil {
		return fmt.Errorf("providerstatus: the step has no request, so its path cannot be read")
	}
	endpoint := strings.ToLower(path.Base(ctx.Request.URL.Path))
	allowed, gated := s.actions[endpoint]
	if !gated {
		return nil
	}

	root, becknContext, becknErr := model.ExtractContext(ctx.Body)
	if becknErr != nil {
		return model.WrapExtractContextErr("could not read the request context", becknErr)
	}

	if err := refuseLookalikeKeys(root, s.keys, "the request body"); err != nil {
		return model.NewBadReqErr("", err)
	}
	if err := refuseLookalikeKeys(becknContext, s.keys, "context"); err != nil {
		return model.NewBadReqErr("", err)
	}
	action, _ := becknContext["action"].(string)
	if !slices.Contains(allowed, strings.ToLower(strings.TrimSpace(action))) {
		return model.NewBadReqErr("", fmt.Errorf(
			"context.action %q does not match the request path %q: want one of %q",
			action, ctx.Request.URL.Path, allowed))
	}
	if endpoint == s.cfg.PublishEndpoint {
		return s.checkPublish(ctx)
	}
	return s.narrowDiscover(ctx)
}

func (s *step) checkPublish(ctx *model.StepContext) error {
	named, err := providerIDsOf(ctx.Body, s.field, s.keys)
	if err != nil {
		return model.NewBadReqErr("", err)
	}

	// Not a verdict on the provider when the list is missing or stale: an
	// unanswered question must never read as "active".
	snap, err := s.list.active()
	if err != nil {
		return err
	}
	// The whole batch is refused on one bad provider, so a partial publish never
	// leaves the sender guessing which catalogs landed.
	for _, id := range named {
		if _, ok := snap.set[id]; !ok {
			return model.NewBadReqErr("", fmt.Errorf(
				"provider %q is not an active provider in the registry", id))
		}
	}
	return nil
}

func (s *step) narrowDiscover(ctx *model.StepContext) error {
	snap, err := s.list.active()
	if err != nil {
		return err
	}
	narrowed, err := narrowFilter(s.field, s.keys, ctx.Body, snap.ids)
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

// narrowFilter restricts a discover to the given provider ids, read at field
// inside each catalog, by AND-ing an allowlist onto message.intent.filters.
// keys are the step's read keys. It refuses what it cannot place safely rather
// than guessing a rewrite.
func narrowFilter(field, keys []string, body []byte, ids []string) ([]byte, error) {
	// Safe to splice: New only accepts plain dotted names.
	idPath := "@." + strings.Join(field, ".")
	allow := idPath + ` == ""` // provider ids are never empty: matches nothing
	if len(ids) > 0 {
		tests := make([]string, len(ids))
		for i, id := range ids {
			if !providerIDPattern.MatchString(id) {
				// The registry's fault, not the sender's: a plain error (500).
				return nil, fmt.Errorf(
					"provider id %q from the registry is not a valid id, so the discover cannot be narrowed", id)
			}
			tests[i] = idPath + ` == "` + id + `"`
		}
		allow = strings.Join(tests, " || ")
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, model.NewBadReqErr("", fmt.Errorf("could not read the discover body: %w", err))
	}

	message, err := objectAt(doc, "message", "message", keys)
	if err != nil {
		return nil, err
	}
	intent, err := objectAt(message, "intent", "message.intent", keys)
	if err != nil {
		return nil, err
	}

	var expression string
	if raw, present := intent["filters"]; !present || raw == nil {
		expression = "$.catalogs[*] ? (" + allow + ")"
	} else {
		filters, ok := raw.(map[string]any)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf("message.intent.filters must be an object"))
		}
		if err := refuseLookalikeKeys(filters, keys, "message.intent.filters"); err != nil {
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
	// Only the two members discovery reads are forwarded; any other is dropped.
	intent["filters"] = map[string]any{"type": "jsonpath", "expression": expression}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("could not write the discover body: %w", err)
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// objectAt returns parent[key] as an object, creating it when absent. where
// names it in errors. Its keys are checked by refuseLookalikeKeys against read.
func objectAt(parent map[string]any, key, where string, read []string) (map[string]any, error) {
	raw, present := parent[key]
	if !present || raw == nil {
		child := map[string]any{}
		parent[key] = child
		return child, nil
	}
	child, ok := raw.(map[string]any)
	if !ok {
		return nil, model.NewBadReqErr("", fmt.Errorf("%s must be an object", where))
	}
	if err := refuseLookalikeKeys(child, read, where); err != nil {
		return nil, model.NewBadReqErr("", err)
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

// fixedKeys are the JSON keys this step reads whatever its config. The keys of
// the configured provider field are added to them.
var fixedKeys = []string{
	"message", "intent", "filters", "catalogs",
	"type", "expression", "context", "action",
}

// refuseLookalikeKeys refuses an object holding a key that differs from one
// in read (the keys this step reads) only by case or Unicode folding ("ID", "Filters",
// "catalog\u017f"). Go's struct decoding would match such a key, other readers
// would not, so the step and the service behind it could read different
// fields. A key spelled exactly is fine; so is any key nobody here reads.
func refuseLookalikeKeys(obj map[string]any, read []string, where string) error {
	for key := range obj {
		for _, name := range read {
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

// providerIDsOf returns the provider id, read at field, of every catalog in a
// publish. A catalog that names none is an error rather than a skip: an
// unnamed provider cannot be checked, so it must not slip through.
//
// Keys are read exactly, never through a struct: a struct would also match
// "ID" or a folded "catalogs", and the body is forwarded as is. Every object
// along field is checked against read.
func providerIDsOf(body []byte, field, read []string) ([]string, error) {
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
	if err := refuseLookalikeKeys(message, read, "message"); err != nil {
		return nil, err
	}
	catalogs, _ := message["catalogs"].([]any)
	if len(catalogs) == 0 {
		return nil, fmt.Errorf("the publish carries no catalogs: set message.catalogs")
	}
	dotted := strings.Join(field, ".")
	ids := make([]string, 0, len(catalogs))
	for i, raw := range catalogs {
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message.catalogs[%d] must be an object", i)
		}
		where := fmt.Sprintf("message.catalogs[%d]", i)
		if err := refuseLookalikeKeys(obj, read, where); err != nil {
			return nil, err
		}
		// Walk to the object holding the id. A missing or non-object step
		// leaves obj nil, which yields no id below.
		for _, key := range field[:len(field)-1] {
			obj, _ = obj[key].(map[string]any)
			where += "." + key
			if err := refuseLookalikeKeys(obj, read, where); err != nil {
				return nil, err
			}
		}
		id, _ := obj[field[len(field)-1]].(string)
		if id == "" {
			return nil, fmt.Errorf("catalog %d names no provider: set message.catalogs[%d].%s", i, i, dotted)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
