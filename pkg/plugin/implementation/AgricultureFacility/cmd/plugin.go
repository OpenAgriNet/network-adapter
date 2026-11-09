// Command plugin builds the agriculture facility provider step as a loadable
// plugin.
//
// The filename of the built .so is the id a deployment names in providerSteps,
// so this package is AgricultureFacility's whole public surface: a config map
// in, a step out.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/AgricultureFacility"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// agriFacilityProvider implements definition.ProviderStepProvider.
type agriFacilityProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = AgricultureFacility.New

// parseConfig turns the plugin config map into a typed Config. The settings
// every provider step shares are read by common.ParseStepConfig and copied
// across, because this package's Config is a flat struct rather than an alias
// (see AgricultureFacility.Config for why). Only the two settings that are this
// capability's own are read here.
func (p agriFacilityProvider) parseConfig(config map[string]string) (*AgricultureFacility.Config, error) {
	shared, err := common.ParseStepConfig(config)
	if err != nil {
		return nil, err
	}
	cfg := &AgricultureFacility.Config{
		BindingKeys:        shared.BindingKeys,
		ProviderIDAt:       shared.ProviderIDAt,
		CapabilityCodeAt:   shared.CapabilityCodeAt,
		MaxResponseBytes:   shared.MaxResponseBytes,
		AnswersUnaddressed: shared.AnswersUnaddressed,
		ResponseStatus:     shared.ResponseStatus,
		AuthByProvider:     shared.AuthByProvider,
		// Absent means the Beckn v2 convention. See
		// AgricultureFacility.DefaultFacilityTypesAt.
		FacilityTypesAt: config["facilityTypesAt"],
	}

	if raw := config["searchConcurrency"]; raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid searchConcurrency value '%s': %w", raw, err)
		}
		if value <= 0 {
			return nil, fmt.Errorf("searchConcurrency must be positive, got %d", value)
		}
		cfg.SearchConcurrency = value
	}

	return cfg, nil
}

// New creates a new agriculture facility provider step instance.
func (p agriFacilityProvider) New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper, config map[string]string) (definition.Step, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	cfg, err := p.parseConfig(config)
	if err != nil {
		log.Errorf(ctx, err, "Failed to parse agriculture facility configuration")
		return nil, nil, fmt.Errorf("failed to parse agriculture facility configuration: %w", err)
	}

	step, closer, err := newStepFunc(ctx, registry, mapper, cfg)
	if err != nil {
		log.Errorf(ctx, err, "Failed to create agriculture facility step")
		return nil, nil, err
	}

	log.Infof(ctx, "Agriculture facility step created successfully")
	return step, closer, nil
}

// Provider is the exported plugin instance.
var Provider = agriFacilityProvider{}

// Compile-time proof the provider satisfies the interface the manager asserts
// against. A mismatch is otherwise a runtime cast failure at startup.
var _ definition.ProviderStepProvider = Provider
