// Package common serves a Beckn capability by calling an ordinary API that has
// never heard of Beckn: recognise the capability, resolve the call plan,
// translate out, call, translate back.
//
// Common to the three capability plugins and to nothing else. Each of them --
// WeatherObservation, MandiPrice, KnowledgeAdvisory -- is a name, a set of
// binding keys and a credential profile over this machinery. Something shared
// by fewer than all three belongs in its own package, not here on the strength
// of the name.
//
// Nothing about any provider or domain is held here. What varies per capability
// comes from the registry (endpoint, method, budget, which mapping) and from
// the mapping itself. "Upstream" is the registry's word for the API being
// called, and stays the word for it throughout.
package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/httputil"
)

// Prerequisites are the values a capability needs that its payload does not
// carry, keyed by binding key.
//
// A mapping cannot produce them: a station id comes from a spatial lookup, a
// market code from a table. That is real I/O, and no expression language should
// do it. What a function returns reaches the mapping as _local, so the mapping
// still decides what the provider is asked for. Most capabilities need none.
type Prerequisites map[string]func(context.Context, any) (map[string]any, error)

// Config holds configuration parameters for the step.
type Config struct {
	// The capabilities this step answers to. A request for anything else
	// passes through untouched.
	//
	// A list because a provider can serve several, and a second config entry
	// with the same plugin id would collide in the handler's step map -- losing
	// a capability with no error anywhere. What differs per capability comes
	// from the registry, so one step serving several needs nothing more.
	BindingKeys []string `yaml:"bindingKeys" json:"bindingKeys"`

	// Override where the two halves of a binding key sit in a payload. Absent
	// means the Beckn v2 convention, which every deployment should use.
	//
	// A network convention, not a preference: every participant must agree or
	// requests silently fail to match. Configurable only so a spec change can
	// be tracked without waiting for a release. Both or neither.
	ProviderIDAt     string `yaml:"providerIdAt" json:"providerIdAt"`
	CapabilityCodeAt string `yaml:"capabilityCodeAt" json:"capabilityCodeAt"`

	// One credential profile per provider, keyed by participant id -- the left
	// half of a binding key.
	//
	// Per provider because a step serves several binding keys and the providers
	// behind them need not authenticate alike: one may take oauth2 client
	// credentials, the next a token in a query parameter. Everything else that
	// differs per provider already comes from the registry, so auth was the one
	// thing pinned to the step.
	//
	// NO step-wide default: a missing profile is refused at startup rather than
	// quietly falling through to sending nothing.
	//
	// Built by ParseProviderAuth, not decoded from YAML -- an operator writes a
	// nested block per provider, which pkg/plugin flattens on the way in.
	AuthByProvider map[string]*AuthProfile `yaml:"-" json:"-"`

	// MaxResponseBytes caps what is read from the provider.
	MaxResponseBytes int64 `yaml:"maxResponseBytes" json:"maxResponseBytes"`
}

// Step serves whatever capabilities a domain package configures it for. It is
// safe for concurrent use.
type Step struct {
	config        *Config
	paths         Paths
	prerequisites Prerequisites
	registry      definition.ProviderRecordLookup
	mapper        definition.Mapper
	httpClient    *http.Client

	// One per provider, built at startup so a request only looks one up. Each
	// holds its own token, so two oauth2 providers cannot share one.
	auth map[string]*authenticator
}

// New creates the step.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	prerequisites Prerequisites, cfg *Config) (*Step, func() error, error) {
	if registry == nil {
		return nil, nil, errors.New("a provider record lookup is required")
	}
	if mapper == nil {
		return nil, nil, errors.New("a mapper is required")
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
		// The timeout is per request, from the registry's budget.
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
// Both halves or neither: one overridden and one defaulted would match nothing,
// silently, on every request.
func bindingPaths(cfg *Config) (Paths, error) {
	if cfg.ProviderIDAt == "" && cfg.CapabilityCodeAt == "" {
		return BecknV2, nil
	}
	if cfg.ProviderIDAt == "" {
		return Paths{}, errors.New("capabilityCodeAt is set without providerIdAt")
	}
	if cfg.CapabilityCodeAt == "" {
		return Paths{}, errors.New("providerIdAt is set without capabilityCodeAt")
	}
	paths := Paths{ProviderID: cfg.ProviderIDAt, CapabilityCode: cfg.CapabilityCodeAt}
	if err := paths.Validate(); err != nil {
		return Paths{}, err
	}
	return paths, nil
}

// applyDefaults fills in what was left out and rejects what cannot be defaulted.
func applyDefaults(cfg *Config) error {
	// No default: this package serves whatever a domain package points it at,
	// so any default would name one provider's capability and be silently
	// wrong for every other.
	if len(cfg.BindingKeys) == 0 {
		return errors.New("bindingKeys is required: it is what this step answers to")
	}
	for _, key := range cfg.BindingKeys {
		if strings.TrimSpace(key) == "" {
			return errors.New("bindingKeys carries an empty entry")
		}
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = httputil.DefaultMaxResponseBytes
	}
	if cfg.AuthByProvider == nil {
		cfg.AuthByProvider = map[string]*AuthProfile{}
	}

	// Both directions, because each catches a different silent mistake: a
	// profile for an unserved provider is a typo applying to nothing, and a
	// served provider with no profile would send no credential at all.
	served := map[string]bool{}
	for _, key := range cfg.BindingKeys {
		served[providerIDFrom(key)] = true
	}
	for provider := range cfg.AuthByProvider {
		if !served[provider] {
			return fmt.Errorf(
				"auth is configured for %q, which is not a provider in bindingKeys (%s)",
				provider, strings.Join(cfg.BindingKeys, ", "))
		}
	}
	for provider := range served {
		profile, ok := cfg.AuthByProvider[provider]
		if !ok {
			return fmt.Errorf(
				"%q is served but has no auth block; every provider declares its own, "+
					"using authScheme none where the upstream needs no credential", provider)
		}
		profile.Provider = provider
		if err := profile.validate(); err != nil {
			return err
		}
	}
	return nil
}
