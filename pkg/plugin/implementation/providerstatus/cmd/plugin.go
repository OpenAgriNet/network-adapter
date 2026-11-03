package main

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/providerstatus"
)

type provider struct{}

func (p provider) New(ctx context.Context, registry definition.RegistryLookup, config map[string]string) (definition.Step, func() error, error) {
	step, err := providerstatus.New(registry)
	return step, nil, err
}

var Provider = provider{}
