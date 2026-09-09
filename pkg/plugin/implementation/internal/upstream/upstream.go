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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/capabilitybinding"
)

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
