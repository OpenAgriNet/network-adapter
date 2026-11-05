package main

import (
	"context"
	"errors"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/nonbecknrouter"
)

// RouterProvider provides instances of the non-Beckn router.
type RouterProvider struct{}

// New builds a passthrough router from the plugin config.
func (rp RouterProvider) New(ctx context.Context, config map[string]string) (definition.Router, func() error, error) {
	if ctx == nil {
		return nil, nil, errors.New("context cannot be nil")
	}
	return nonbecknrouter.New(ctx, &nonbecknrouter.Config{RoutingConfig: config["routingConfig"]})
}

// Provider is the exported symbol that the plugin manager will look for.
var Provider = RouterProvider{}
