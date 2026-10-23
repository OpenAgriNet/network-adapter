// Command plugin builds the grievance provider step as a loadable plugin. The
// .so's filename is the id a deployment names in providerSteps.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/Grievance"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// grievanceProvider implements definition.ProviderStepProvider.
type grievanceProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = Grievance.New

// parseConfig turns the plugin config map into a typed Config. Defaults and
// validation are left to Grievance.New.
func (p grievanceProvider) parseConfig(config map[string]string) (*Grievance.Config, error) {
	cfg := &Grievance.Config{
		BindingKeys:      splitList(config["bindingKeys"]),
		ProviderIDAt:     config["providerIdAt"],
		CapabilityCodeAt: config["capabilityCodeAt"],
	}

	if raw, exists := config["maxResponseBytes"]; exists && raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid maxResponseBytes value '%s': %w", raw, err)
		}
		if value <= 0 {
			return nil, fmt.Errorf("maxResponseBytes must be positive, got %d", value)
		}
		cfg.MaxResponseBytes = value
	}

	// One credential profile per provider.
	auth, err := common.ParseProviderAuth(config)
	if err != nil {
		return nil, err
	}
	cfg.AuthByProvider = auth

	return cfg, nil
}

// New creates a new grievance provider step instance.
func (p grievanceProvider) New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper, config map[string]string) (definition.Step, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	cfg, err := p.parseConfig(config)
	if err != nil {
		log.Errorf(ctx, err, "Failed to parse grievance configuration")
		return nil, nil, fmt.Errorf("failed to parse grievance configuration: %w", err)
	}

	step, closer, err := newStepFunc(ctx, registry, mapper, cfg)
	if err != nil {
		log.Errorf(ctx, err, "Failed to create grievance step")
		return nil, nil, err
	}

	log.Infof(ctx, "Grievance step created successfully")
	return step, closer, nil
}

// splitList reads a comma-separated config value, trimming spaces and dropping
// blanks.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// Provider is the exported plugin instance.
var Provider = grievanceProvider{}

// Compile-time check that Provider satisfies the interface.
var _ definition.ProviderStepProvider = Provider
