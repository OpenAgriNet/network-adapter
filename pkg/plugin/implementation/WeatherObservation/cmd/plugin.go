package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/WeatherObservation"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// weatherProvider implements definition.ProviderStepProvider.
type weatherProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = WeatherObservation.New

// parseConfig turns the plugin config map into a typed Config. Every provider
// step reads the same settings the same way, so the reading is shared; what is
// defaulted and what is refused are left to WeatherObservation.New.
func (p weatherProvider) parseConfig(config map[string]string) (*WeatherObservation.Config, error) {
	return common.ParseStepConfig(config)
}

// New creates a new weather provider step instance.
func (p weatherProvider) New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper, config map[string]string) (definition.Step, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	cfg, err := p.parseConfig(config)
	if err != nil {
		log.Errorf(ctx, err, "Failed to parse weather configuration")
		return nil, nil, fmt.Errorf("failed to parse weather configuration: %w", err)
	}

	step, closer, err := newStepFunc(ctx, registry, mapper, cfg)
	if err != nil {
		log.Errorf(ctx, err, "Failed to create weather step")
		return nil, nil, err
	}

	log.Infof(ctx, "Weather step created successfully")
	return step, closer, nil
}

// Provider is the exported plugin instance.
var Provider = weatherProvider{}

// Compile-time proof the provider satisfies the interface the manager asserts
// against. A mismatch is otherwise a runtime cast failure at startup.
var _ definition.ProviderStepProvider = Provider
