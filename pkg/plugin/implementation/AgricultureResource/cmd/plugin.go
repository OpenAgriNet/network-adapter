// Command plugin builds the agriculture resource step as a loadable plugin. The
// .so's filename is the id a deployment names in providerSteps.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/AgricultureResource"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// agricultureResourceProvider implements definition.ProviderStepProvider.
type agricultureResourceProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = AgricultureResource.New

// parseConfig reads the settings every provider step shares; defaults and
// refusals are left to AgricultureResource.New.
func (p agricultureResourceProvider) parseConfig(config map[string]string) (*AgricultureResource.Config, error) {
	return common.ParseStepConfig(config)
}

// New creates a new agriculture resource step instance.
func (p agricultureResourceProvider) New(ctx context.Context, cache definition.Cache, registry definition.ProviderRecordLookup, mapper definition.Mapper, config map[string]string) (definition.Step, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	cfg, err := p.parseConfig(config)
	if err != nil {
		log.Errorf(ctx, err, "Failed to parse agriculture resource configuration")
		return nil, nil, fmt.Errorf("failed to parse agriculture resource configuration: %w", err)
	}

	step, closer, err := newStepFunc(ctx, cache, registry, mapper, cfg)
	if err != nil {
		log.Errorf(ctx, err, "Failed to create agriculture resource step")
		return nil, nil, err
	}

	log.Infof(ctx, "AgricultureResource step created for %v", cfg.BindingKeys)
	return step, closer, nil
}

// Provider is the exported plugin instance.
var Provider = agricultureResourceProvider{}

// Compile-time check that Provider satisfies the interface.
var _ definition.ProviderStepProvider = Provider
