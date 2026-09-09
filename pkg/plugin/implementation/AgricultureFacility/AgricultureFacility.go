// Package AgricultureFacility serves the openagrinet:AgricultureFacility
// capability.
//
// One package per capability, named for the capability it serves, so which
// plugin owns one is readable from its binding key.
//
// Named for the capability and not for POCRA, deliberately. A provider is a
// registry row, and more than one could serve this same capability -- a second
// state aggregator would be another row and another mapping, not another
// package.
//
// Almost nothing lives here, and that is the point. Recognising a capability,
// resolving the call plan, authenticating, calling with the registry's budget
// and translating in both directions are all internal/upstream's, because none
// of them differ by domain. What this package owns is its name, and
// prerequisites -- the work a mapping cannot express, which is domain knowledge
// by definition.
//
// The upstream this was written against is POCRA's aggregator, whose search
// takes a category code and a point, both of which an AgricultureFacility
// payload carries. So the package is a name and nothing else: see
// prerequisites.go for why that is worth stating.
package AgricultureFacility

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/upstream"
)

// Config carries everything upstream.Config does, plus this capability's own
// fan-out concurrency -- which upstream.Config no longer has a field for, now
// that upstream has no opinion on fan-out concurrency at all. A flat struct
// with upstream.Config's fields repeated rather than an alias (which this
// package used to be, and MandiPrice and WeatherObservation still are) or an
// embedded upstream.Config (which would break every existing flat struct
// literal, `&Config{BindingKeys: ..., AuthScheme: ...}`, since Go's composite
// literal syntax does not promote an embedded struct's fields the way a
// selector expression does).
type Config struct {
	BindingKeys      []string `yaml:"bindingKeys" json:"bindingKeys"`
	ProviderIDAt     string   `yaml:"providerIdAt" json:"providerIdAt"`
	CapabilityCodeAt string   `yaml:"capabilityCodeAt" json:"capabilityCodeAt"`
	AuthScheme       string   `yaml:"authScheme" json:"authScheme"`
	UsernameEnv      string   `yaml:"usernameEnv" json:"usernameEnv"`
	PasswordEnv      string   `yaml:"passwordEnv" json:"passwordEnv"`
	HeaderName       string   `yaml:"headerName" json:"headerName"`
	HeaderValueEnv   string   `yaml:"headerValueEnv" json:"headerValueEnv"`
	QueryName        string   `yaml:"queryName" json:"queryName"`
	QueryValueEnv    string   `yaml:"queryValueEnv" json:"queryValueEnv"`
	MaxResponseBytes int64    `yaml:"maxResponseBytes" json:"maxResponseBytes"`

	// FanOutConcurrency is how many of a fan-out's calls may be in flight at
	// once. See fanout.go's DefaultFanOutConcurrency and MaxFanOut for what
	// absent and too-large mean.
	FanOutConcurrency int `yaml:"fanOutConcurrency" json:"fanOutConcurrency"`
}

// New creates the agriculture facility step.
//
// Which capabilities it answers to is configuration, with no default: a package
// serving a family cannot guess which of them a deployment has providers for.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	concurrency := applyFanOutDefaults(cfg.FanOutConcurrency)

	upstreamCfg := &upstream.Config{
		BindingKeys:      cfg.BindingKeys,
		ProviderIDAt:     cfg.ProviderIDAt,
		CapabilityCodeAt: cfg.CapabilityCodeAt,
		AuthScheme:       cfg.AuthScheme,
		UsernameEnv:      cfg.UsernameEnv,
		PasswordEnv:      cfg.PasswordEnv,
		HeaderName:       cfg.HeaderName,
		HeaderValueEnv:   cfg.HeaderValueEnv,
		QueryName:        cfg.QueryName,
		QueryValueEnv:    cfg.QueryValueEnv,
		MaxResponseBytes: cfg.MaxResponseBytes,
	}

	return upstream.NewWithFanOut(ctx, registry, mapper, prerequisites,
		gatherFacilities(concurrency), upstreamCfg)
}
