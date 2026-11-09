package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/registrycache"
)

type registryCacheProvider struct{}

// New reads url, ttl (e.g. "24h") and preload (e.g. "Participant,ProviderSchema"),
// all required.
func (registryCacheProvider) New(ctx context.Context, cache definition.Cache, cfg map[string]string) (definition.RegistryCache, func() error, error) {
	config := registrycache.Config{URL: cfg["url"]}
	ttl, err := time.ParseDuration(cfg["ttl"])
	if err != nil || ttl <= 0 {
		return nil, nil, fmt.Errorf("invalid ttl %q, want a positive duration such as 24h", cfg["ttl"])
	}
	config.TTL = ttl
	for _, entity := range strings.Split(cfg["preload"], ",") {
		if entity = strings.TrimSpace(entity); entity == "" {
			continue
		}
		if strings.ContainsAny(entity, ":/ ") {
			return nil, nil, fmt.Errorf("invalid preload entity %q, want an entity name such as Participant", entity)
		}
		config.Preload = append(config.Preload, entity)
	}
	if len(config.Preload) == 0 {
		return nil, nil, fmt.Errorf("preload is required, e.g. Participant,ProviderSchema")
	}

	store, err := registrycache.New(cache, config)
	if err != nil {
		return nil, nil, err
	}
	stop := store.Start(context.WithoutCancel(ctx))
	return store, func() error { stop(); return nil }, nil
}

var Provider = registryCacheProvider{}
