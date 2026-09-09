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
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Config carries everything common.Config does, plus this capability's own
// search concurrency -- which common.Config has no field for, because how
// many calls one payload becomes is not something that package knows about at
// all. A flat struct with common.Config's fields repeated rather than an
// alias (which this package used to be, and MandiPrice and WeatherObservation
// still are) or an embedded common.Config (which would break every existing
// flat struct literal, `&Config{BindingKeys: ..., AuthScheme: ...}`, since
// Go's composite literal syntax does not promote an embedded struct's fields
// the way a selector expression does).
type Config struct {
	BindingKeys      []string `yaml:"bindingKeys" json:"bindingKeys"`
	ProviderIDAt     string   `yaml:"providerIdAt" json:"providerIdAt"`
	CapabilityCodeAt string   `yaml:"capabilityCodeAt" json:"capabilityCodeAt"`
	MaxResponseBytes int64    `yaml:"maxResponseBytes" json:"maxResponseBytes"`

	// AuthByProvider carries one credential profile per provider, keyed by
	// participant id. Passed through to the inner step, which is where the
	// schemes are defined and validated -- see common.AuthProfile.
	AuthByProvider map[string]*common.AuthProfile `yaml:"-" json:"-"`

	// SearchConcurrency is how many of a multi-type search's calls may be in
	// flight at once. See search.go's DefaultSearchConcurrency and
	// MaxFacilityTypes for what absent and too-large mean.
	SearchConcurrency int `yaml:"searchConcurrency" json:"searchConcurrency"`
}

// New creates the agriculture facility step.
//
// Which capabilities it answers to is configuration, with no default: a package
// serving a family cannot guess which of them a deployment has providers for.
//
// Two steps, one returned. The inner one is internal/upstream's, which serves
// one payload with one call and knows nothing about facility types. The outer
// one is this package's own (see search.go): it splits a multi-type search
// into one single-type payload per type, runs the inner step over each of them
// concurrently, and merges the answers. Everything POCRA-specific about that
// is in the outer step, which is why the inner one is the same step
// MandiPrice and WeatherObservation use unchanged.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	if cfg == nil {
		cfg = &Config{}
	}

	upstreamCfg := &common.Config{
		BindingKeys:      cfg.BindingKeys,
		ProviderIDAt:     cfg.ProviderIDAt,
		CapabilityCodeAt: cfg.CapabilityCodeAt,
		MaxResponseBytes: cfg.MaxResponseBytes,
		AuthByProvider:   cfg.AuthByProvider,
	}

	one, closer, err := common.New(ctx, registry, mapper, prerequisites, upstreamCfg)
	if err != nil {
		return nil, nil, err
	}

	// The same paths the inner step resolved from the same config, so both
	// answer "is this payload mine?" identically. Resolved through upstream
	// rather than duplicated here: a second reading of the same two config
	// fields could drift from the first.
	paths, err := common.BindingPaths(upstreamCfg)
	if err != nil {
		return nil, nil, err
	}

	return &Step{
		inner:       one,
		paths:       paths,
		bindingKeys: cfg.BindingKeys,
		concurrency: searchConcurrency(cfg.SearchConcurrency),
	}, closer, nil
}
