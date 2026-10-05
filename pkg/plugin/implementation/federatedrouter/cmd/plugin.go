// Package main provides the plugin entry point for the federated routing
// step. It is compiled as a Go plugin (.so) via
//
//	go build -buildmode=plugin -o plugins/federated.so \
//	    ./pkg/plugin/implementation/federatedrouter/cmd/plugin.go
//
// and loaded by the beckn-onix plugin manager at runtime, which looks up the
// exported Provider symbol. The .so basename is the plugin id, so pipelines
// wire the step as federated.
package main

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/federatedrouter"
)

type provider struct{}

// New builds the federated routing step from its YAML config map.
func (p provider) New(ctx context.Context, cfg map[string]string) (definition.Step, func(), error) {
	step, err := federatedrouter.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	return step, nil, nil
}

// Provider is the exported symbol that the beckn-onix plugin manager looks up.
var Provider = provider{}

// Compile-time assurance that Provider satisfies the step plugin contract.
var _ definition.StepProvider = Provider
