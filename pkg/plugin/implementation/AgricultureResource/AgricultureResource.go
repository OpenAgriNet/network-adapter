// Package AgricultureResource serves capabilities published as
// openagrinet:AgricultureResource, such as SATHI's seed stock.
package AgricultureResource

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Config is the shared provider step config.
type Config = common.Config

// New creates the agriculture resource step.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	return common.New(ctx, registry, mapper, prerequisites, cfg)
}
