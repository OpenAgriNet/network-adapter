// Package WeatherObservation serves the network's weather capabilities.
//
// One package per capability, named for the capability it serves, so which
// plugin owns a payload is readable from its binding key without a lookup:
// openagrinet:WeatherObservation is this one's, openagrinet:MandiPrice is not.
// Which keys it answers to is still configuration -- a deployment can point it
// at a related pack such as openagrinet:WeatherAdvisory -- but the name says
// what it was built against.
//
// Almost nothing lives here. Recognising a capability, resolving the call plan,
// authenticating, calling with the registry's budget and translating in both
// directions are all internal/common's, because none of them differ by domain.
// What this package owns is its name, and prerequisites -- the work a mapping
// cannot express, which is domain knowledge by definition.
package WeatherObservation

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Config is upstream's, unchanged. Aliased here so a domain plugin's cmd package
// need not know where the machinery lives.
type Config = common.Config

// New creates the weather step.
//
// Which capabilities it answers to is configuration, with no default: a package
// serving a family cannot guess which of them a deployment has providers for.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	return common.New(ctx, registry, mapper, prerequisites, cfg)
}
