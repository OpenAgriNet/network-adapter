// Command plugin builds the grievance provider step as a loadable plugin. The
// .so's filename is the id a deployment names in providerSteps.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/Grievance"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// grievanceProvider implements definition.ProviderStepProvider.
type grievanceProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = Grievance.New

// parseConfig turns the plugin config map into a typed Config. Every provider
// step reads the same settings the same way, so the reading is shared; what is
// defaulted and what is refused are left to Grievance.New.
func (p grievanceProvider) parseConfig(config map[string]string) (*Grievance.Config, error) {
	// Envelope settings first: the credential parser refuses settings it does
	// not know.
	envelopes, rest, err := Grievance.ParseEnvelopes(config)
	if err != nil {
		return nil, err
	}
	cfg, err := common.ParseStepConfig(rest)
	if err != nil {
		return nil, err
	}
	cfg.Envelopes = envelopes.Envelopes
	cfg.HostedElsewhere = envelopes.HostedElsewhere
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

// Provider is the exported plugin instance.
var Provider = grievanceProvider{}

// Compile-time check that Provider satisfies the interface.
var _ definition.ProviderStepProvider = Provider
