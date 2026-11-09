// Command plugin builds the mandi provider step as a loadable plugin.
//
// The filename of the built .so is the id a deployment names in providerSteps,
// so this package is mandi's whole public surface: a config map in, a step out.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/MandiPrice"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// mandiProvider implements definition.ProviderStepProvider.
type mandiProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = MandiPrice.New

// parseConfig turns the plugin config map into a typed Config. Every provider
// step reads the same settings the same way, so the reading is shared; what is
// defaulted and what is refused are left to MandiPrice.New.
func (p mandiProvider) parseConfig(config map[string]string) (*MandiPrice.Config, error) {
	return common.ParseStepConfig(config)
}

// New creates a new mandi provider step instance.
func (p mandiProvider) New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper, config map[string]string) (definition.Step, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	cfg, err := p.parseConfig(config)
	if err != nil {
		log.Errorf(ctx, err, "Failed to parse mandi configuration")
		return nil, nil, fmt.Errorf("failed to parse mandi configuration: %w", err)
	}

	step, closer, err := newStepFunc(ctx, registry, mapper, cfg)
	if err != nil {
		log.Errorf(ctx, err, "Failed to create mandi step")
		return nil, nil, err
	}

	log.Infof(ctx, "Mandi step created successfully")
	return step, closer, nil
}

// Provider is the exported plugin instance.
var Provider = mandiProvider{}

// Compile-time proof the provider satisfies the interface the manager asserts
// against. A mismatch is otherwise a runtime cast failure at startup.
var _ definition.ProviderStepProvider = Provider
