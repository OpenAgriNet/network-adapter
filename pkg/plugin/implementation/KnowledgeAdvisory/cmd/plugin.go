// Command plugin builds the knowledge advisory provider step as a loadable
// plugin.
//
// The filename of the built .so is the id a deployment names in providerSteps,
// so this package is KnowledgeAdvisory's whole public surface: a config map in,
// a step out.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/KnowledgeAdvisory"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// knowledgeAdvisoryProvider implements definition.ProviderStepProvider.
type knowledgeAdvisoryProvider struct{}

// newStepFunc creates a new step. Indirected for tests.
var newStepFunc = KnowledgeAdvisory.New

// parseConfig turns the plugin config map into a typed Config. Every provider
// step reads the same settings the same way, so the reading is shared; what is
// defaulted and what is refused are left to KnowledgeAdvisory.New.
func (p knowledgeAdvisoryProvider) parseConfig(config map[string]string) (*KnowledgeAdvisory.Config, error) {
	return common.ParseStepConfig(config)
}

// New creates a new knowledge advisory provider step instance.
func (p knowledgeAdvisoryProvider) New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper, config map[string]string) (definition.Step, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}

	cfg, err := p.parseConfig(config)
	if err != nil {
		log.Errorf(ctx, err, "Failed to parse knowledge advisory configuration")
		return nil, nil, fmt.Errorf("failed to parse knowledge advisory configuration: %w", err)
	}

	step, closer, err := newStepFunc(ctx, registry, mapper, cfg)
	if err != nil {
		log.Errorf(ctx, err, "Failed to create knowledge advisory step")
		return nil, nil, err
	}

	log.Infof(ctx, "Knowledge advisory step created successfully")
	return step, closer, nil
}

// Provider is the exported plugin instance.
var Provider = knowledgeAdvisoryProvider{}

// Compile-time proof the provider satisfies the interface the manager asserts
// against. A mismatch is otherwise a runtime cast failure at startup.
var _ definition.ProviderStepProvider = Provider
