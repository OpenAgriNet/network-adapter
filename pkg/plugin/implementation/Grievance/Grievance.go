// Package Grievance serves the network's grievance capabilities: filing a
// grievance against a government scheme and checking on it. Provider details
// live in the registry, the mappings and the adapter config, not here.
package Grievance

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Config is the shared step config.
type Config = common.Config

// New creates the grievance step. It has no prerequisites.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	return common.New(ctx, registry, mapper, nil, cfg)
}
