// Package Grievance serves the network's grievance capabilities: filing a
// grievance against a government scheme and checking on it.
//
// One package for every scheme. What differs between PMFBY and the next portal
// -- endpoints, field names, how it says "no", how it authenticates -- is the
// registry's, the mapping's and the adapter config's, so nothing here names a
// provider. Like MandiPrice, this package is a name over internal/common.
package Grievance

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Config is upstream's, unchanged. Aliased here so a domain plugin's cmd package
// need not know where the machinery lives.
type Config = common.Config

// New creates the grievance step.
//
// No prerequisites: everything a grievance call sends is in the payload.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	return common.New(ctx, registry, mapper, nil, cfg)
}
