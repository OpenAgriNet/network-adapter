// Package upstream serves a Beckn capability by calling an ordinary API that has
// never heard of Beckn.
//
// "upstream" is the registry's own word for such an API -- a Participant of type
// upstream, as against a node that speaks Beckn. This package is the machinery
// for calling one: recognise the capability, resolve the call plan, translate
// out, call, translate back.
//
// It holds nothing about any provider or any domain. What varies per capability
// comes from the registry (endpoint, method, budget, which mapping) and from the
// mapping itself (what the payload must satisfy, what to send, what to return).
// A domain package wraps this, supplying only its name and whatever prerequisite
// work a mapping cannot express.
package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/capabilitybinding"
)

// Defaults applied when the registry or the operator leaves a setting out.
const (
	// DefaultTimeout and DefaultRetryMax are the registry contract's defaults
	// for an action that leaves timeoutMs or retryMax out. Zero retries is
	// deliberate: a provider that failed is retried only where the operator
	// said so, because a retry on a non-idempotent action is a second booking.
	DefaultTimeout  = 15 * time.Second
	DefaultRetryMax = 0
	// DefaultMaxResponseBytes caps what is read from the provider. The response
	// is mapped in memory, so an unbounded one is an unbounded allocation.
	DefaultMaxResponseBytes = 4 << 20 // 4 MiB

	// MaxTimeout and MaxRetryMax bound what a registry row may ask for.
	//
	// Both come from DATA, not from this deployment's config, and neither is
	// cheap: an attempt holds a goroutine and the inbound connection for its
	// whole timeout, and http.Server's write timeout does not cancel the
	// request context. So a row reading retryMax 1000, timeoutMs 60000 pins
	// both for roughly seventeen hours, and a handful of such requests is the
	// adapter. The registry is trusted to say where a provider is; it is not a
	// reason to let one row decide how long this process is busy.
	//
	// Clamped rather than refused. A row that overreaches is a configuration
	// mistake, and failing every request for that capability is a worse answer
	// than serving it with a sane budget and saying so in the log.
	MaxTimeout  = 30 * time.Second
	MaxRetryMax = 5
)

// Auth schemes this step can present upstream. Credentials themselves are never
// configured here or held in the registry -- config names the environment
// variable to read, so a secret reaches the process through its environment and
// nothing else.
const (
	// RetryBackoffBase is the first wait between attempts, doubling from there
	// up to RetryBackoffMax. Short, because the retry budget comes from the
	// registry and an operator setting 5 retries did not ask for seconds of
	// latency -- only for the provider's brief unavailability to be ridden out.
	RetryBackoffBase = 50 * time.Millisecond
	RetryBackoffMax  = 800 * time.Millisecond

	// redactedMarker stands in for a credential in anything logged or returned.
	redactedMarker = "REDACTED"

	AuthSchemeNone   = "none"
	AuthSchemeBasic  = "basic"
	AuthSchemeHeader = "header"
	// AuthSchemeQuery puts the credential in the query string, which some
	// upstreams are built around whatever anyone thinks of it. It is the least
	// safe of the four -- a query string is logged by proxies and appears in a
	// transport error -- so the value is redacted from anything this package
	// logs or returns. See redact.
	AuthSchemeQuery = "query"

	// AuthSchemeOAuth2 exchanges a client id and secret for a short-lived
	// bearer token, and sends that. For an upstream behind an OAuth2 token
	// endpoint -- a Keycloak service account, say -- where a static token is not
	// an option: the live one this was written against lives ten hours, so a
	// value pasted into an environment variable is wrong twice a day.
	//
	// Only the client_credentials grant. There is no user to redirect and no
	// refresh token in that grant, so the other flows would be dead code.
	AuthSchemeOAuth2 = "oauth2"
)

// codeUpstreamUnavailable reports a provider that could not be reached or
// answered with a failure. It is not this adapter's fault and not the caller's.
const codeUpstreamUnavailable = "NET_DOWNSTREAM_UNAVAILABLE"

// tokenRefreshSkew is how early an oauth2 token stops being trusted. It has to
// exceed the round trip to the provider, so that a request which passes the
// expiry check cannot arrive after the token has actually died. One extra
// exchange per token lifetime is the whole cost.
const tokenRefreshSkew = 60 * time.Second

// Prerequisites are the values a capability needs that its payload does not
// carry, keyed by binding key.
//
// A mapping cannot produce them: a station id comes from a spatial lookup, a
// session token from an exchange, a market code from a table. That is real I/O,
// and no expression language should be able to do it.
//
// Whatever a function returns is handed to the mapping as _local, so the mapping
// still decides what the provider is finally asked for. A capability with no
// entry needs nothing, which is the common case.
type Prerequisites map[string]func(context.Context, any) (map[string]any, error)

// Config holds configuration parameters for the step.
type Config struct {
	// BindingKeys are the capabilities this step answers to. A request for
	// anything else passes through untouched.
	//
	// A list because a provider can serve more than one: the registry contract
	// says a provider serving two capabilities is one Participant and two
	// ProviderSchema rows. Configuring a second entry with the same plugin id
	// instead would collide in the handler's id-keyed step map, and one
	// capability would be lost with no error anywhere.
	//
	// What differs per capability -- the endpoint, the mapping, the budget --
	// comes from the registry, so one step serving several needs nothing else.
	BindingKeys []string `yaml:"bindingKeys" json:"bindingKeys"`

	// ProviderIDAt and CapabilityCodeAt override where the two halves of a
	// binding key sit in a payload. Absent means the Beckn v2 convention, which
	// is what every deployment should be using.
	//
	// This is a network convention rather than a deployment's preference --
	// every participant must agree, or two adapters disagree about what a
	// binding key is and requests silently fail to match. It is configurable
	// only so that a spec change can be tracked without waiting for a release,
	// and both must be given together.
	ProviderIDAt     string `yaml:"providerIdAt" json:"providerIdAt"`
	CapabilityCodeAt string `yaml:"capabilityCodeAt" json:"capabilityCodeAt"`

	// AuthByProvider carries one credential profile PER PROVIDER, keyed by participant
	// id -- the left half of a binding key.
	//
	// Per provider rather than per step because a step serves several binding
	// keys and the providers behind them need not authenticate alike: one may
	// take an oauth2 client id and secret, the next a token in a query
	// parameter. Everything else that differs per provider already comes from
	// the registry -- the endpoint, the path, the mapping -- so auth was the
	// only thing pinned to the step, and the only thing that made a second
	// provider of the same capability impossible to configure.
	//
	// There is deliberately NO step-wide default. A profile per provider means
	// there is never a question of which setting applies, and a provider whose
	// profile is missing is refused at startup rather than quietly falling
	// through to sending nothing.
	//
	// Built by ParseProviderAuth from the flattened config, not decoded from YAML
	// directly: what an operator writes is a nested block per provider, which
	// pkg/plugin flattens on the way in.
	AuthByProvider map[string]*AuthProfile `yaml:"-" json:"-"`

	// MaxResponseBytes caps what is read from the provider.
	MaxResponseBytes int64 `yaml:"maxResponseBytes" json:"maxResponseBytes"`
}

// AuthProfile is how ONE provider's credentials are presented upstream.
//
// One per provider rather than one per step: a step serves several binding
// keys, and the providers behind them need not authenticate alike.
type AuthProfile struct {
	// Provider is the participant id this profile belongs to. Held so an error
	// can name it: with several profiles on one step, "authScheme query
	// requires queryName" would otherwise leave an operator guessing which
	// provider it meant.
	Provider string

	// Scheme is one of none, basic, header, query or oauth2. Providers differ
	// here, which is why it is configuration and not an assumption.
	Scheme string

	// UsernameEnv and PasswordEnv name the environment variables holding basic
	// credentials. They are variable NAMES, never the values.
	UsernameEnv string
	PasswordEnv string

	// HeaderName and HeaderValueEnv configure the header scheme: which header
	// to set, and which environment variable holds its value.
	HeaderName     string
	HeaderValueEnv string

	// QueryName and QueryValueEnv configure the query scheme: the parameter
	// name to add, and the environment variable holding its value. Named the
	// same way as the header pair, for the same reason -- the credential is
	// never in this config, only the name of the variable carrying it.
	QueryName     string
	QueryValueEnv string

	// TokenURL is the OAuth2 token endpoint. Not a credential, so it is named
	// here rather than through an environment variable -- but it IS
	// deployment-specific, so the reference config carries a placeholder.
	TokenURL string
	// ClientIDEnv and ClientSecretEnv name the variables holding the client
	// credentials. The values never appear in config, the registry, or a log.
	ClientIDEnv     string
	ClientSecretEnv string
}

// authenticator is one provider's profile plus the token it holds.
//
// The token cache lives HERE rather than on the Step, and that is the whole
// reason this type exists. A step-wide cache shared between two oauth2
// providers would hand the first provider's token to the second, which is
// authenticating as somebody else -- a failure no test of a single provider
// can see.
type authenticator struct {
	cfg AuthProfile

	// Two mechanisms because there are two jobs. tokenMu serialises the
	// EXCHANGE, so a cold start sends one request to the issuer rather than one
	// per concurrent caller. token is atomic so READERS never take that mutex,
	// which matters because secretForms is one of them and it is reached from
	// inside the exchange -- guarding the value with tokenMu instead deadlocked
	// on the first failing exchange, which is how this was found.
	tokenMu sync.Mutex
	token   atomic.Pointer[cachedToken]
}

// Step serves whatever capabilities a domain package configures it for. It is
// safe for concurrent use.
type Step struct {
	config        *Config
	paths         capabilitybinding.Paths
	prerequisites Prerequisites
	registry      definition.ProviderRecordLookup
	mapper        definition.Mapper
	httpClient    *http.Client

	// One authenticator per provider, keyed by participant id. Built once at
	// startup, so a request only looks one up -- and each holds its own token,
	// so two oauth2 providers cannot share one.
	auth map[string]*authenticator
}

// New creates the step.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	prerequisites Prerequisites, cfg *Config) (*Step, func() error, error) {
	if registry == nil {
		return nil, nil, errors.New("upstream: a provider record lookup is required")
	}
	if mapper == nil {
		return nil, nil, errors.New("upstream: a mapper is required")
	}
	if cfg == nil {
		cfg = &Config{}
	}
	if err := applyDefaults(cfg); err != nil {
		return nil, nil, err
	}

	paths, err := bindingPaths(cfg)
	if err != nil {
		return nil, nil, err
	}

	step := &Step{
		config:        cfg,
		paths:         paths,
		prerequisites: prerequisites,
		registry:      registry,
		mapper:        mapper,
		// Timeout is set per request from the registry's own budget, so the
		// client carries none of its own.
		httpClient: &http.Client{},
		auth:       make(map[string]*authenticator, len(cfg.AuthByProvider)),
	}
	for provider, profile := range cfg.AuthByProvider {
		step.auth[provider] = &authenticator{cfg: *profile}
	}

	closer := func() error {
		log.Debugf(ctx, "Cleaning up upstream step resources")
		step.httpClient.CloseIdleConnections()
		return nil
	}

	log.Infof(ctx, "Upstream step created for %s", strings.Join(cfg.BindingKeys, ", "))
	return step, closer, nil
}

// bindingPaths resolves where this step reads a binding key from.
//
// Both halves or neither: overriding one and leaving the other on the default
// is a half-configured deployment that would match nothing, and it would do so
// silently on every request rather than once at startup.
func bindingPaths(cfg *Config) (capabilitybinding.Paths, error) {
	if cfg.ProviderIDAt == "" && cfg.CapabilityCodeAt == "" {
		return capabilitybinding.BecknV2, nil
	}
	if cfg.ProviderIDAt == "" {
		return capabilitybinding.Paths{}, errors.New("upstream: capabilityCodeAt is set without providerIdAt")
	}
	if cfg.CapabilityCodeAt == "" {
		return capabilitybinding.Paths{}, errors.New("upstream: providerIdAt is set without capabilityCodeAt")
	}
	paths := capabilitybinding.Paths{ProviderID: cfg.ProviderIDAt, CapabilityCode: cfg.CapabilityCodeAt}
	if err := paths.Validate(); err != nil {
		return capabilitybinding.Paths{}, err
	}
	return paths, nil
}

// applyDefaults fills in what was left out and rejects what cannot be defaulted.
func applyDefaults(cfg *Config) error {
	// No default. This package serves whatever a domain package configures it
	// for, so a default would have to name one provider's capability -- wrong
	// for every other domain built on it, and silently wrong rather than loudly.
	if len(cfg.BindingKeys) == 0 {
		return errors.New("upstream: bindingKeys is required: it is what this step answers to")
	}
	for _, key := range cfg.BindingKeys {
		if strings.TrimSpace(key) == "" {
			return errors.New("upstream: bindingKeys carries an empty entry")
		}
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.AuthByProvider == nil {
		cfg.AuthByProvider = map[string]*AuthProfile{}
	}

	// Both directions, because each catches a different mistake and both are
	// silent at runtime. A profile for a provider this step does not serve is a
	// typo that would apply to nothing; a served provider with no profile would
	// fall through to sending no credential and read as the provider rejecting
	// us.
	served := map[string]bool{}
	for _, key := range cfg.BindingKeys {
		served[providerIDFrom(key)] = true
	}
	for provider := range cfg.AuthByProvider {
		if !served[provider] {
			return fmt.Errorf(
				"upstream: auth is configured for %q, which is not a provider in bindingKeys (%s)",
				provider, strings.Join(cfg.BindingKeys, ", "))
		}
	}
	for provider := range served {
		profile, ok := cfg.AuthByProvider[provider]
		if !ok {
			return fmt.Errorf(
				"upstream: %q is served but has no auth block; every provider declares its own, "+
					"using authScheme none where the upstream needs no credential", provider)
		}
		profile.Provider = provider
		if err := profile.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate refuses a profile whose scheme and fields disagree. Every message
// names the provider: with several profiles on one step, the field alone would
// leave an operator guessing which block to look at.
func (a *AuthProfile) validate() error {
	switch a.Scheme {
	case AuthSchemeNone:
	case AuthSchemeBasic:
		if a.UsernameEnv == "" || a.PasswordEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme basic requires usernameEnv and passwordEnv", a.Provider)
		}
	case AuthSchemeHeader:
		if a.HeaderName == "" || a.HeaderValueEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme header requires headerName and headerValueEnv", a.Provider)
		}
	case AuthSchemeQuery:
		if a.QueryName == "" || a.QueryValueEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme query requires queryName and queryValueEnv", a.Provider)
		}
	case AuthSchemeOAuth2:
		if a.TokenURL == "" || a.ClientIDEnv == "" || a.ClientSecretEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme oauth2 requires tokenUrl, clientIdEnv and clientSecretEnv",
				a.Provider)
		}
	case "":
		return fmt.Errorf("upstream: %s: authScheme is required, "+
			"and is none where the upstream needs no credential", a.Provider)
	default:
		return fmt.Errorf(
			"upstream: %s: unknown authScheme %q: must be none, basic, header, query or oauth2",
			a.Provider, a.Scheme)
	}
	return nil
}

// ParseProviderAuth builds one credential profile per provider from a plugin's
// flattened settings.
//
// What an operator writes is a block per provider:
//
//	knowledge-provider:
//	  authScheme: oauth2
//	  tokenUrl: https://issuer.example/token
//
// which pkg/plugin flattens to authScheme-knowledge-provider and
// tokenUrl-knowledge-provider before any plugin sees it. This reads that form
// back into profiles.
//
// Shared rather than repeated in each capability plugin: all of them copied
// the same field list out of the same map, so a scheme added in one place had
// to be remembered in three.
//
// The split is on the FIRST dash, which is unambiguous because no setting name
// contains one while a participant id routinely does -- knowledge-provider,
// provider.oan.dev. So the field is always the part before it.
func ParseProviderAuth(config map[string]string) (map[string]*AuthProfile, error) {
	profiles := map[string]*AuthProfile{}

	// Sorted so a config with two mistakes reports the same one every run.
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		field, provider, dashed := strings.Cut(key, "-")
		if !dashed {
			// A bare auth field is the old step-wide form. Refused rather than
			// ignored: silently dropping it leaves every provider on no
			// credential at all, which reads as the provider rejecting us.
			if authFields[key] {
				return nil, fmt.Errorf(
					"upstream: %q is set for the whole step; auth is per provider now, "+
						"so it belongs in a block named for the participant id", key)
			}
			continue
		}
		if !authFields[field] {
			// Not an auth setting, and nothing else on a provider step carries
			// a dash -- so this is a misspelled field rather than something to
			// pass through.
			return nil, fmt.Errorf("upstream: %q is not a credential setting", key)
		}
		if strings.TrimSpace(provider) == "" {
			return nil, fmt.Errorf("upstream: %q names no provider after the dash", key)
		}

		profile, seen := profiles[provider]
		if !seen {
			profile = &AuthProfile{Provider: provider}
			profiles[provider] = profile
		}
		value := config[key]
		switch field {
		case "authScheme":
			profile.Scheme = value
		case "usernameEnv":
			profile.UsernameEnv = value
		case "passwordEnv":
			profile.PasswordEnv = value
		case "headerName":
			profile.HeaderName = value
		case "headerValueEnv":
			profile.HeaderValueEnv = value
		case "queryName":
			profile.QueryName = value
		case "queryValueEnv":
			profile.QueryValueEnv = value
		case "tokenUrl":
			profile.TokenURL = value
		case "clientIdEnv":
			profile.ClientIDEnv = value
		case "clientSecretEnv":
			profile.ClientSecretEnv = value
		}
	}
	return profiles, nil
}

// authFields is the closed set of per-provider credential settings. Closed on
// purpose: it is what makes the split on the first dash decidable, and what
// turns a misspelled field into a startup error rather than a setting that
// quietly does nothing.
var authFields = map[string]bool{
	"authScheme":      true,
	"usernameEnv":     true,
	"passwordEnv":     true,
	"headerName":      true,
	"headerValueEnv":  true,
	"queryName":       true,
	"queryValueEnv":   true,
	"tokenUrl":        true,
	"clientIdEnv":     true,
	"clientSecretEnv": true,
}

// providerIDFrom returns the provider half of a binding key. The format is
// "<participantId>|<capabilityCode>", and a participant id carries dashes and
// dots but never a pipe, so the first one separates them.
func providerIDFrom(bindingKey string) string {
	provider, _, _ := strings.Cut(bindingKey, "|")
	return strings.TrimSpace(provider)
}

// Run serves the request when it is for this step's capability, and does
// nothing when it is not.
//
// Doing nothing is the dispatch mechanism: several provider steps sit in one
// pipeline and each recognises its own work, so adding a provider is one more
// entry rather than a change to a routing table.
func (s *Step) Run(ctx *model.StepContext) error {
	binding, err := capabilitybinding.From(s.paths, ctx.Body)
	if errors.Is(err, capabilitybinding.ErrNoBinding) {
		return nil
	}
	if err != nil {
		// Everything From refuses is a statement about the payload: unreadable
		// JSON, or a request naming more than one call. Unclassified it becomes
		// a 500, which says this adapter broke and leaves the reason in a log
		// the caller cannot read.
		return model.NewBadReqErr("", err)
	}
	if !s.serves(binding.Key()) {
		log.Debugf(ctx, "upstream: %s is not one of this step's capabilities, passing through", binding.Key())
		return nil
	}

	plan, err := s.registry.ProviderRecord(ctx, binding.Key())
	if err != nil {
		// A definite "no such binding" is the caller naming something that is
		// not there, so 404 -- the same reasoning the no-route path uses to
		// refuse an unrecognised capability rather than ACK it. A registry that
		// could not be consulted is different and stays a 500: unclassified,
		// because it is this adapter that failed.
		if errors.Is(err, definition.ErrProviderRecordNotFound) {
			// %w, not %v: the sentinel has to stay unwrappable, or anything
			// upstream testing errors.Is against it silently stops matching.
			return model.NewNotFoundErr("", fmt.Errorf(
				"upstream: the registry publishes no active binding for %s: %w", binding.Key(), err))
		}
		return fmt.Errorf("upstream: no call plan for %s: %w", binding.Key(), err)
	}

	return s.serve(ctx, plan)
}

// resolve runs whatever prerequisite work this capability needs, and returns the
// values for the mapping to read under _local.
//
// Empty rather than nil when there is nothing: a mapping referring to _local on
// a capability that resolves nothing should read a missing field, not fail.
func (s *Step) resolve(ctx context.Context, bindingKey string, beckn any) (map[string]any, error) {
	prerequisite, needed := s.prerequisites[bindingKey]
	if !needed {
		return map[string]any{}, nil
	}
	local, err := prerequisite(ctx, beckn)
	if err != nil {
		return nil, fmt.Errorf("upstream: %s could not resolve what it needs before the call: %w", bindingKey, err)
	}
	if local == nil {
		return map[string]any{}, nil
	}
	return local, nil
}

// serves reports whether a binding key is one this step answers to.
//
// A slice rather than a set: a step serves a handful of capabilities at most, so
// the scan costs less than the map would, and the config order is preserved in
// the log line above.
func (s *Step) serves(key string) bool {
	return slices.Contains(s.config.BindingKeys, key)
}

// serve runs the exchange this step exists for: resolve, map out, call, map back.
func (s *Step) serve(ctx *model.StepContext, plan *model.ProviderRecord) error {
	action := extractAction(ctx.Body)
	call, served := plan.Actions[action]
	if !served {
		// The capability publishes no endpoint for this action, so it does not
		// serve it. Refused here rather than after a call to whichever endpoint
		// happened to be on the record -- naming what it does serve turns a
		// registry mistake into a one-line fix.
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: %s does not serve action %q; it serves %s",
			plan.BindingKey, action, strings.Join(plan.ServedActions(), ", ")))
	}

	beckn, err := decodeBody(ctx.Body)
	if err != nil {
		return err
	}

	// What this provider requires of a payload is declared by its mapping, not
	// by this step. A capability with a different rule is a different mapping
	// file rather than a different build -- and the rule sits beside the
	// extraction it guards.
	if err := s.mapper.Verify(ctx, call.Mappings, map[string]any{"beckn": beckn}); err != nil {
		return err
	}

	// Whatever this capability needs that its payload does not carry. Empty for
	// most: the mapping reads the payload directly and needs nothing resolved.
	local, err := s.resolve(ctx, plan.BindingKey, beckn)
	if err != nil {
		return err
	}

	upstreamRequest, err := s.buildRequest(ctx, call, beckn, local)
	if err != nil {
		return err
	}

	// Which credential this provider takes. Resolved from the binding key, so
	// one step serving several providers authenticates each as its own.
	// Startup guarantees a profile per served provider; this guards the case
	// where a record arrives for a key the config never declared.
	auth, configured := s.auth[providerIDFrom(plan.BindingKey)]
	if !configured {
		return fmt.Errorf("upstream: no credential is configured for %s", plan.BindingKey)
	}

	upstreamResponse, err := s.call(ctx, auth, plan.BaseURL, call, upstreamRequest)
	if err != nil {
		return err
	}

	answer, err := decodeBody(upstreamResponse)
	if err != nil {
		return fmt.Errorf("upstream: provider answered with something that is not JSON: %w", err)
	}

	// The same mapping reference as the request, other half: one file carries
	// both directions for this action.
	//
	// The mapping is handed what each party sent, plus whatever prerequisites
	// resolved, under _local. Empty when there are none.
	becknResponse, err := s.mapper.Transform(ctx, call.Mappings, definition.DirectionResponse, map[string]any{
		"beckn":    beckn,
		"_local":   local,
		"response": answer,
	})
	if err != nil {
		return err
	}
	if len(becknResponse) == 0 {
		// Either the file has no response half, or its transform matched nothing
		// in this answer. Both leave no Beckn response to return, and returning
		// the provider's own shape instead would be worse than failing. The
		// message says what was observed rather than guessing which it was.
		return fmt.Errorf("upstream: the response half of %s produced nothing, so %s cannot be answered",
			call.Mappings, plan.BindingKey)
	}

	ctx.ResponseBody = becknResponse
	log.Infof(ctx, "upstream: served %s in %d bytes", plan.BindingKey, len(becknResponse))
	return nil
}

// buildRequest produces what the provider is sent.
//
// Whatever the mapping produces IS the request: a body for a method that takes
// one, query parameters for a method that does not. Nothing is substituted when
// it produces nothing, so an empty request half means an empty request.
//
// This step used to extract a point from the payload and fall back to sending
// that. It meant the choice of which payload fields reach the provider lived in
// Go, so adding a parameter -- a date range, say -- was a rebuild. Now it is a
// mapping edit and nothing else.
func (s *Step) buildRequest(ctx context.Context, call model.ActionPlan, beckn any, local map[string]any) ([]byte, error) {
	mapped, err := s.mapper.Transform(ctx, call.Mappings, definition.DirectionRequest, map[string]any{
		"beckn":  beckn,
		"_local": local,
	})
	if err != nil {
		return nil, err
	}
	if len(mapped) == 0 {
		log.Debugf(ctx, "upstream: the request half of %s produced nothing; sending an empty request", call.Mappings)
	}
	return mapped, nil
}

// extractAction reads the Beckn action a request is for.
func extractAction(body []byte) string {
	var payload struct {
		Context struct {
			Action string `json:"action"`
		} `json:"context"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Context.Action
}

// decodeBody turns raw JSON into the generic value a mapping reads.
func decodeBody(body []byte) (any, error) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("upstream: could not read JSON: %w", err)
	}
	return decoded, nil
}

// call makes the upstream request described by the plan, retrying within its
// budget.
// budget resolves how long one attempt may take and how many retries follow
// it, applying the registry's values within this deployment's ceilings.
//
// Pure and separate from call so both bounds can be asserted without a server
// that sleeps for the timeout it is testing.
//
// retryMax counts retries, not attempts, so the call itself is always made
// once. An absent retryMax and an explicit 0 are the same instruction.
func budget(call model.ActionPlan) (time.Duration, int) {
	timeout := DefaultTimeout
	if call.TimeoutMs > 0 {
		timeout = time.Duration(call.TimeoutMs) * time.Millisecond
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}

	retries := DefaultRetryMax
	if call.RetryMax > 0 {
		retries = call.RetryMax
	}
	if retries > MaxRetryMax {
		retries = MaxRetryMax
	}
	return timeout, retries
}

func (s *Step) call(ctx context.Context, auth *authenticator, baseURL string, call model.ActionPlan, mapped []byte) ([]byte, error) {
	endpoint, err := buildEndpoint(baseURL, call, mapped)
	if err != nil {
		return nil, err
	}

	timeout, retries := budget(call)
	if d := time.Duration(call.TimeoutMs) * time.Millisecond; d > timeout {
		log.Warnf(ctx, "upstream: registry asks for a %v timeout; using the %v ceiling", d, timeout)
	}
	if call.RetryMax > retries {
		log.Warnf(ctx, "upstream: registry asks for %d retries; using the %d ceiling",
			call.RetryMax, retries)
	}
	attempts := retries + 1

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// A caller that has gone away is not worth another attempt, and neither
		// is a budget already spent. Checked before the call rather than after,
		// so a cancelled request costs nothing.
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				lastErr = err
			}
			break
		}

		body, err := s.attempt(ctx, auth, call, endpoint, mapped, timeout)
		if err == nil {
			return body, nil
		}
		lastErr = s.redact(err)
		log.Warnf(ctx, "upstream: attempt %d/%d failed: %v", attempt, attempts, lastErr)

		// Only some failures are worth repeating. A 4xx, a request this step
		// could not build and a credential it could not read will fail
		// identically however many times they are tried -- and retrying the
		// credential case is the worst of them, because it reports an
		// operator's missing environment variable as the provider being down.
		if isPermanent(err) {
			break
		}
		if attempt < attempts {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				break
			}
		}
	}
	return nil, model.NewCodedErr(http.StatusBadGateway, codeUpstreamUnavailable,
		fmt.Errorf("upstream: provider did not answer after %d attempts: %w", attempts, lastErr))
}

// permanentErr marks a failure no retry can fix. Kept unexported and detected
// with errors.As, so a caller of this package sees only the underlying error.
type permanentErr struct{ error }

func (p permanentErr) Unwrap() error { return p.error }

// doNotRetry marks err as not worth repeating.
func doNotRetry(err error) error { return permanentErr{err} }

// isPermanent reports whether err is one no further attempt would change.
func isPermanent(err error) bool {
	var permanent permanentErr
	return errors.As(err, &permanent)
}

// backoff is how long to wait before the next attempt.
//
// Exponential from a short base and capped, because the provider being briefly
// busy is the case worth waiting out; anything longer is a timeout's job. With
// no wait at all a retryMax of 5 spends its whole budget inside a couple of
// milliseconds, which is not a retry so much as the same failure six times.
func backoff(attempt int) time.Duration {
	if attempt <= 1 {
		return RetryBackoffBase
	}
	// Doubled in a loop that stops at the ceiling rather than shifted and then
	// clamped. `RetryBackoffBase << (attempt - 1)` overflows int64 once the
	// shift reaches 38 at a 50ms base, and the wrapped value is NEGATIVE -- so
	// it passes the `> RetryBackoffMax` check, is returned, and a sleep on a
	// negative duration returns immediately. The retry loop then spins as fast
	// as the provider can refuse. Stopping at the ceiling cannot overflow,
	// because it never doubles a value already at or past it.
	wait := RetryBackoffBase
	for i := 1; i < attempt && wait < RetryBackoffMax; i++ {
		wait *= 2
	}
	if wait > RetryBackoffMax {
		return RetryBackoffMax
	}
	return wait
}

// sleep waits, or reports that the context ended first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attempt makes one upstream request.
func (s *Step) attempt(ctx context.Context, auth *authenticator, call model.ActionPlan, endpoint string, mapped []byte, timeout time.Duration) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	method := canonicalMethod(call.Method)
	req, err := http.NewRequestWithContext(attemptCtx, method, endpoint, requestBody(method, mapped))
	if err != nil {
		return nil, doNotRetry(fmt.Errorf("could not build the request: %w", err))
	}
	if hasBody(method) {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := s.authenticate(auth, req); err != nil {
		// A missing or unreadable credential is configuration, not weather.
		return nil, doNotRetry(err)
	}

	// The URL as it actually went on the wire, credential removed. At info
	// rather than debug because this is the line that answers "what did we ask,
	// and what came back" -- the question every provider problem starts with.
	requested := s.redactString(req.URL.String())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read the response: %w", err)
	}
	log.Infof(ctx, "upstream: %s %s -> %s, %d bytes", method, requested, resp.Status, len(body))
	if int64(len(body)) > s.config.MaxResponseBytes {
		// Asking again will not make the answer smaller.
		return nil, doNotRetry(fmt.Errorf("response exceeds the %d byte limit", s.config.MaxResponseBytes))
	}
	// Any 2xx, not 200 alone. A provider is entitled to answer 202 for work it
	// accepted, 204 for nothing to report, or 201 for something it created, and
	// treating those as failures would refuse a perfectly good exchange. 3xx
	// does not reach here: the client follows redirects.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The body is logged, not returned. It goes into a 502 that is signed
		// and sent to the network caller, and what a provider puts in a failure
		// body is its own business -- a stack trace, an internal hostname, a
		// database error. The status is the caller's business and stays; the
		// body is the operator's, and the log is where the operator looks.
		// Redacted on the way to the log too. A provider that rejects a
		// request often quotes it back, credential and all -- so the body is
		// exactly where a query-string token turns up, and moving it from the
		// error to the log would only move the leak.
		log.Warnf(ctx, "upstream: provider returned %s for %s %s: %s",
			resp.Status, method, requested, s.redactString(explain(body)))
		err := fmt.Errorf("provider returned %s", resp.Status)
		// 5xx and 429 are the provider asking to be tried again. Every other
		// 4xx is a statement about the request, which will not improve.
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			return nil, doNotRetry(err)
		}
		return nil, err
	}
	return body, nil
}

// explainLimit is how much of a failed response is quoted. Enough for a
// provider's own message, short enough not to put a page of HTML in a log line
// or a NACK.
const explainLimit = 300

// explain renders a failed response body for a human.
//
// The body was already read and then thrown away, so a provider's own account
// of what was wrong -- Agmarknet says "no data" in the body of a 400 -- never
// reached anyone. The status alone says a call failed and nothing about why,
// which is the first thing an operator needs and the thing that makes a real
// provider's behaviour observable at all.
func explain(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(no body)"
	}
	// Collapse whitespace: a provider that answers with indented JSON or an
	// HTML error page should not spread one failure over forty log lines.
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > explainLimit {
		return text[:explainLimit] + "... (truncated)"
	}
	return text
}

// authenticate presents this provider's credentials, read from the environment
// at call time so a rotated secret takes effect without a restart.
// missingCredential reports an unset credential without naming the variable on
// the wire.
//
// The variable name is deployment configuration, and this error is wrapped into
// a 502 that is signed and returned to a network peer. Telling a peer that
// MANDI_TOKEN is what this deployment reads describes the inside of somebody
// else's stack for no benefit to the caller -- the caller cannot set it, and
// the fix is entirely the operator's. So the name goes to the log, where the
// operator is, and the wire gets the scheme that failed.
func (s *Step) missingCredential(ctx context.Context, provider, scheme, envNames string) error {
	err := fmt.Errorf("upstream: the %s credential for %s is not configured", scheme, provider)
	log.Errorf(ctx, err, "upstream: %s auth is configured for %s but %s is not set",
		scheme, provider, envNames)
	return err
}

func (s *Step) authenticate(auth *authenticator, req *http.Request) error {
	cfg := auth.cfg
	switch cfg.Scheme {
	case AuthSchemeBasic:
		username, password := os.Getenv(cfg.UsernameEnv), os.Getenv(cfg.PasswordEnv)
		if username == "" || password == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "basic",
				cfg.UsernameEnv+" and "+cfg.PasswordEnv)
		}
		req.SetBasicAuth(username, password)
	case AuthSchemeHeader:
		value := os.Getenv(cfg.HeaderValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "header", cfg.HeaderValueEnv)
		}
		req.Header.Set(cfg.HeaderName, value)
	case AuthSchemeQuery:
		value := os.Getenv(cfg.QueryValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "query", cfg.QueryValueEnv)
		}
		// Set rather than Add: a second copy of the parameter is not a
		// credential, it is an ambiguity, and which one an upstream reads is
		// its own business.
		query := req.URL.Query()
		query.Set(cfg.QueryName, value)
		req.URL.RawQuery = query.Encode()
	case AuthSchemeOAuth2:
		token, err := s.bearerToken(req.Context(), auth)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// redact removes a query-string credential from an error's text.
//
// Go's transport errors quote the whole URL -- `Get "http://host/p?token=..."
// dial tcp: ...` -- so without this, one unreachable host writes the credential
// into the log at warn level. Nothing else in this package puts a URL in a
// message, which is why this is the only place it is needed.
//
// A plain string replacement, because the value is what leaks and the value is
// what we hold. Parsing the error to find it would assume a shape net/http does
// not promise.
func (s *Step) redact(err error) error {
	if err == nil {
		return nil
	}
	text := s.redactString(err.Error())
	if text == err.Error() {
		return err
	}
	return redactedErr{text: text, err: err}
}

// redactedErr reports a redacted message while keeping the original reachable
// for errors.Is and errors.As.
//
// errors.New(text) was the obvious thing and it broke the chain: the redacted
// value is what gets %w-wrapped into the final 502, so under a query-string
// scheme -- and only then, since nothing else redacts -- errors.Is(err,
// context.DeadlineExceeded) silently stopped matching. Retry classification
// was never affected, because isPermanent tests the error before redaction,
// which is why nothing failed visibly.
//
// fmt.Errorf("%s: %w", text, err) would have restored the chain and undone the
// redaction with it: %w formats the original, credential included. Reporting
// the redacted text from Error() and the original from Unwrap() keeps both.
//
// The original's text is reachable through errors.Unwrap, which is a
// deliberate act by a caller who wants the cause -- and %v, %s and %w on the
// value itself all go through Error() and stay redacted.
type redactedErr struct {
	text string
	err  error
}

func (e redactedErr) Error() string { return e.text }
func (e redactedErr) Unwrap() error { return e.err }

// bearerToken returns a token to send, exchanging one if what we hold has
// expired or if we hold none.
//
// The lock spans the fetch on purpose. Without it a cold start sends every
// concurrent request to the token endpoint, and the provider's issuer sees a
// burst of identical exchanges. Serialising them costs one wait per token
// lifetime and nothing after that, which is a better trade than a second
// caching layer.
//
// NOTHING IS CACHED ON FAILURE. A transient outage at the token endpoint must
// not leave this step holding a failure for the lifetime it never obtained --
// the next request tries again.
func (s *Step) bearerToken(ctx context.Context, auth *authenticator) (string, error) {
	if held := auth.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	auth.tokenMu.Lock()
	defer auth.tokenMu.Unlock()

	// Re-checked after acquiring: while this caller waited, whoever held the
	// mutex may already have exchanged a fresh token.
	if held := auth.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	token, lifetime, err := s.exchangeToken(ctx, auth)
	if err != nil {
		return "", err
	}
	auth.token.Store(&cachedToken{value: token, expiry: time.Now().Add(lifetime)})
	return token, nil
}

// cachedToken is a token and the moment it stops being trusted.
type cachedToken struct {
	value  string
	expiry time.Time
}

// tokenResponse is the part of an OAuth2 token response this step reads.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// exchangeToken performs the client_credentials grant.
//
// Every failure here is the UPSTREAM exchange failing, so all of them carry
// 502. Unclassified they would surface as a 500, which tells a network peer
// this adapter broke when in fact the provider's issuer did.
func (s *Step) exchangeToken(ctx context.Context, auth *authenticator) (string, time.Duration, error) {
	cfg := auth.cfg
	clientID, clientSecret := os.Getenv(cfg.ClientIDEnv), os.Getenv(cfg.ClientSecretEnv)
	if clientID == "" || clientSecret == "" {
		return "", 0, s.missingCredential(ctx, cfg.Provider, "oauth2",
			cfg.ClientIDEnv+" and "+cfg.ClientSecretEnv)
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token request could not be built: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token endpoint %s could not be reached: %w",
			cfg.TokenURL, err))
	}
	defer resp.Body.Close()

	// Bounded like any other upstream read: a token response is small, and an
	// unbounded read here would be a hole in the same wall.
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token response could not be read: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status, not the body: what an issuer puts in a failure body is its
		// own business, and it routinely quotes the request back.
		log.Warnf(ctx, "upstream: token endpoint %s returned %s: %s",
			cfg.TokenURL, resp.Status, s.redactString(explain(body)))
		return "", 0, s.tokenErr(fmt.Errorf("token endpoint returned %s", resp.Status))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token response from %s is not JSON: %w",
			cfg.TokenURL, err))
	}
	if parsed.AccessToken == "" {
		return "", 0, s.tokenErr(fmt.Errorf("token response from %s carries no access_token",
			cfg.TokenURL))
	}
	lifetime, ok := tokenLifetime(parsed.ExpiresIn, tokenRefreshSkew)
	if !ok {
		return "", 0, s.tokenErr(fmt.Errorf(
			"token response from %s carries no usable expires_in, so its lifetime is unknown",
			cfg.TokenURL))
	}
	return parsed.AccessToken, lifetime, nil
}

// tokenErr classifies a failed exchange. Always 502: the exchange is with the
// provider's issuer, so its failure is upstream's, never the caller's.
func (s *Step) tokenErr(err error) error {
	return model.NewCodedErr(http.StatusBadGateway, codeUpstreamUnavailable,
		fmt.Errorf("upstream: oauth2 token exchange failed: %w", err))
}

// tokenLifetime turns a token response's expires_in into how long we may hold
// that token, or reports that we may not hold it at all.
//
// The issuer owns this number, so it is read from the response and never from
// our config: a configured copy is a second version of the same fact, and it
// is wrong the moment a realm's token lifetime is retuned -- silently, with
// every request 401ing until someone edits a file.
//
// skew is subtracted so a request that passes the check cannot arrive at the
// provider after expiry. It has to exceed the round trip, and costs one extra
// refresh per token lifetime.
//
// Two answers rather than one duration, because "we were not told" is not a
// lifetime. A response with no expires_in is refused rather than cached for a
// guessed period or re-fetched on every single request.
func tokenLifetime(expiresIn int, skew time.Duration) (time.Duration, bool) {
	if expiresIn <= 0 {
		return 0, false
	}
	lifetime := time.Duration(expiresIn) * time.Second
	// A lifetime at or under the skew would put the expiry in the past, and
	// then every request fetches a fresh token. Half is still early enough to
	// refresh before the real expiry.
	if lifetime <= skew {
		return lifetime / 2, true
	}
	return lifetime - skew, true
}

// redactString removes the configured credential from any text about to be
// logged or returned -- an error, a provider's response body, or the URL that
// was requested.
//
// Logging those is deliberate: they say what was asked of whom and what came
// back, which is the first thing anyone wants when a provider misbehaves. This
// is what makes that safe to do at info and warn level.
//
// EVERY scheme, not just query. This used to return early unless the scheme was
// query, on the reasoning that only a query credential reaches a URL -- true of
// the URL, and wrong about the body. A provider quoting the request it rejected
// is the ordinary shape of a 401 or 403 body, an API gateway echoing the
// Authorization header is routine, and a wrong-credential 4xx is not retried,
// so it lands in the log once per request for as long as the credential is
// wrong. basic is the scheme the reference config ships.
func (s *Step) redactString(text string) string {
	for _, secret := range s.secretForms() {
		text = strings.ReplaceAll(text, secret, redactedMarker)
	}
	return text
}

// secretForms returns every form the configured credential can appear in,
// longest first so a value that contains another is replaced before its
// substring turns the longer one into a partial redaction.
//
// Per scheme, because the schemes leak differently and redacting the value we
// hold is not enough on its own:
//
//   - basic wraps the pair: SetBasicAuth sends base64(user:pass), so the
//     password alone does not appear on the wire and replacing it misses the
//     echoed header entirely.
//   - query escapes: authenticate goes through url.Values.Encode, so a base64
//     token carrying "+", "/" or "=" appears as "a%2Bb%2Fc%3D". Escaping what
//     we hold is exact -- same function Encode used, so the two agree by
//     construction rather than by a guess about which characters matter.
//   - header sends the value as-is.
//
// The raw form is kept alongside the wrapped one in both cases: an error built
// from the config rather than from the request still quotes the credential
// unwrapped.
func (s *Step) secretForms() []string {
	// EVERY profile, not the one being served.
	//
	// Redaction is about what could appear in a piece of text, not about which
	// provider a request happened to be for. An error or a log line built while
	// serving one provider can quote another's credential -- a shared client
	// echoing a header, an issuer naming the wrong caller -- and scoping this
	// to the active profile would let that reach the log unredacted. There are
	// a handful of profiles and this runs on a failure path, so walking all of
	// them costs nothing worth measuring.
	var forms []string
	for _, auth := range s.auth {
		forms = append(forms, auth.secretForms()...)
	}
	// Sorted across the merged set rather than per profile: one provider's
	// short token can be a substring of another's, and replacing the short one
	// first would leave the longer half-redacted.
	return longestFirst(forms)
}

// secretForms returns every form this profile's credential can appear in.
//
// Per scheme, because the schemes leak differently and redacting the value we
// hold is not enough on its own:
//
//   - basic wraps the pair: SetBasicAuth sends base64(user:pass), so the
//     password alone does not appear on the wire and replacing it misses the
//     echoed header entirely.
//   - query escapes: authenticate goes through url.Values.Encode, so a base64
//     token carrying "+", "/" or "=" appears as "a%2Bb%2Fc%3D". Escaping what
//     we hold is exact -- same function Encode used, so the two agree by
//     construction rather than by a guess about which characters matter.
//   - header sends the value as-is.
//
// The raw form is kept alongside the wrapped one in both cases: an error built
// from the config rather than from the request still quotes the credential
// unwrapped.
func (a *authenticator) secretForms() []string {
	switch a.cfg.Scheme {
	case AuthSchemeBasic:
		username, password := os.Getenv(a.cfg.UsernameEnv), os.Getenv(a.cfg.PasswordEnv)
		if password == "" {
			return nil
		}
		forms := []string{password}
		if username != "" {
			// The wire form, which is what a gateway echoes back.
			forms = append(forms,
				base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
		}
		// The username is deliberately NOT redacted. It identifies rather than
		// authenticates, and it is routinely a short common word -- redacting
		// "user" or "admin" would eat unrelated text and cost the operator the
		// log line they came for. The pair and the password are the secrets.
		return forms
	case AuthSchemeHeader:
		value := os.Getenv(a.cfg.HeaderValueEnv)
		if value == "" {
			return nil
		}
		return []string{value}
	case AuthSchemeOAuth2:
		// Both halves: the client secret we send to the issuer, and the token
		// it gave back. The token is the one that reaches the provider, so it
		// is the one an echoing 401 body quotes.
		var forms []string
		if secret := os.Getenv(a.cfg.ClientSecretEnv); secret != "" {
			forms = append(forms, secret)
		}
		// Read without tokenMu: this is reached from inside the exchange, which
		// holds it.
		if held := a.token.Load(); held != nil && held.value != "" {
			forms = append(forms, held.value)
		}
		// The client id is deliberately NOT redacted: it identifies, it does
		// not authenticate, and it is what makes a log line useful.
		return forms
	case AuthSchemeQuery:
		value := os.Getenv(a.cfg.QueryValueEnv)
		if value == "" {
			return nil
		}
		forms := []string{value}
		if encoded := url.QueryEscape(value); encoded != value {
			forms = append(forms, encoded)
		}
		return forms
	}
	return nil
}

// longestFirst orders replacement candidates so a longer form is substituted
// before any shorter one it contains.
func longestFirst(forms []string) []string {
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// buildEndpoint joins the plan's base URL and path, carrying the mapped request
// as query parameters when the method takes no body.
func buildEndpoint(baseURL string, call model.ActionPlan, mapped []byte) (string, error) {
	if err := verifyBaseURL(baseURL); err != nil {
		return "", err
	}
	if err := verifyPath(call.Path); err != nil {
		return "", err
	}

	// baseUrl cannot end in a slash and path must begin with one, so exactly one
	// separator appears between them. The trim is belt and braces: the registry
	// refuses a trailing slash on baseUrl, and this keeps a row that predates
	// that from producing a doubled one.
	endpoint := strings.TrimSuffix(baseURL, "/") + call.Path
	if hasBody(call.Method) {
		return endpoint, nil
	}

	query, err := asQuery(mapped)
	if err != nil {
		return "", err
	}
	if query == "" {
		return endpoint, nil
	}
	if strings.Contains(endpoint, "?") {
		return endpoint + "&" + query, nil
	}
	return endpoint + "?" + query, nil
}

// verifyPath refuses a published path nobody could have meant.
//
// The registry constrains this, but it is a separate deployable that may not be
// updated in step, so a row that slipped through has to fail here with something
// an operator can act on rather than as a provider's 404 three hops away.
//
// An empty segment is the case worth catching: "//get-daily" is never
// deliberate, and plenty of servers answer it differently from "/get-daily". A
// trailing slash is deliberately left alone -- "/api/" and "/api" are a
// distinction some APIs genuinely make, so stripping it would silently change
// the URL the operator published.
func verifyPath(path string) error {
	if path == "" {
		return model.NewBadReqErr("", errors.New("upstream: the registry publishes no path for this action"))
	}
	if !strings.HasPrefix(path, "/") {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: path %q does not begin with a slash, so it cannot be joined to a base url", path))
	}
	if strings.Contains(path, "//") {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: path %q has an empty segment; write it with single slashes", path))
	}
	// A dot segment is refused rather than resolved. The registry says which
	// path answers an action, and a row that climbs out of it is either a
	// mistake or an attempt to reach something the row does not name -- and
	// net/url would quietly resolve it either way, so the request that left
	// would not be the request the row described.
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." || segment == "." {
			return model.NewBadReqErr("", fmt.Errorf(
				"upstream: path %q contains the %q segment; publish the path it resolves to instead",
				path, segment))
		}
	}
	// A fragment is never sent, so a row carrying one describes a request that
	// cannot be made. Refused here rather than silently dropped by the
	// transport, which would make the row look honoured.
	if strings.Contains(path, "#") {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: path %q contains a fragment, which is never sent to a server", path))
	}
	return nil
}

// verifyBaseURL checks the participant's base url before it is joined to a
// path, so a row that cannot produce a request says so as a bad request rather
// than as the provider being unreachable.
//
// Without this, `baseUrl: "registry:8081"` -- a scheme left off -- failed
// inside http.NewRequestWithContext and arrived as a 502 "provider did not
// answer after 1 attempts: could not build the request". That names the
// provider for an error in the row describing it, and it is retried on the way
// there. jsonmapper has always checked its own reference this way; this is the
// same check on the other url the registry publishes.
func verifyBaseURL(baseURL string) error {
	if baseURL == "" {
		return model.NewBadReqErr("", errors.New("upstream: the registry publishes no base url for this provider"))
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return model.NewBadReqErr("", fmt.Errorf("upstream: invalid base url %q: %w", baseURL, err))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return model.NewBadReqErr("", fmt.Errorf(
			"upstream: base url %q must be http or https", baseURL))
	}
	if parsed.Host == "" {
		return model.NewBadReqErr("", fmt.Errorf("upstream: base url %q names no host", baseURL))
	}
	return nil
}

// asQuery renders a mapped request as query parameters.
//
// A method with no body still needs the mapping's output somewhere, and the
// query string is the only place it can go. Only scalars are carried: a nested
// value has no single obvious encoding, and inventing one here would put a
// convention in Go that belongs in the mapping.
func asQuery(mapped []byte) (string, error) {
	if len(bytes.TrimSpace(mapped)) == 0 {
		return "", nil
	}
	var fields map[string]any
	if err := json.Unmarshal(mapped, &fields); err != nil {
		return "", fmt.Errorf("upstream: mapped request is not an object, so it cannot become a query: %w", err)
	}

	values := url.Values{}
	for name, value := range fields {
		rendered, ok := renderScalar(value)
		if !ok {
			return "", fmt.Errorf("upstream: mapped field %q is not a scalar and cannot become a query parameter", name)
		}
		values.Set(name, rendered)
	}
	return values.Encode(), nil
}

// renderScalar renders a JSON scalar as a query parameter value.
func renderScalar(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		// 'g' with -1 precision round-trips without inventing trailing zeros, so
		// 19.9975 stays 19.9975 rather than becoming 19.997500.
		return strconv.FormatFloat(typed, 'g', -1, 64), true
	default:
		return "", false
	}
}

// requestBody returns the body to send, which is none for methods that take none.
func requestBody(method string, mapped []byte) io.Reader {
	if !hasBody(method) {
		return nil
	}
	return bytes.NewReader(mapped)
}

// hasBody reports whether a method carries a request body.
func hasBody(method string) bool {
	switch canonicalMethod(method) {
	case http.MethodGet, http.MethodHead, http.MethodDelete, "":
		return false
	default:
		return true
	}
}

// canonicalMethod returns a known HTTP method in the spelling the RFC gives
// it, and anything else unchanged.
//
// hasBody used to upper-case privately, which made the method look
// case-insensitive when it is not: NewRequestWithContext transmits it verbatim,
// so a registry row reading `method: "post"` sent `post /path HTTP/1.1`. The
// body was attached correctly -- hasBody had normalised -- but nginx and most
// gateways answer 405 to a lowercase method, which classifies permanent and
// surfaces as a 502 "provider did not answer". A row that is right in every
// respect but its capitalisation is a bad way to spend an afternoon.
//
// Only known methods are rewritten. Upper-casing everything would be a new
// restriction on an upstream entitled to a method this list has not heard of,
// and net/http already refuses one that is not a valid token.
//
// An empty method is left empty: net/http documents "" as GET and substitutes
// it, and hasBody agrees that it carries no body, so the two are already
// consistent and inventing a value here would only hide where it comes from.
func canonicalMethod(method string) string {
	upper := strings.ToUpper(method)
	switch upper {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return upper
	}
	return method
}
