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

// New reads:
//   - url (required): the registry base, e.g. http://registry:8081/api/v1
//   - ttl: Go duration, e.g. "24h"; absent or 0 disables caching
//   - preload: entities loaded up front, comma separated, e.g.
//     "Participant,ProviderSchema"
func (registryCacheProvider) New(ctx context.Context, cache definition.Cache, cfg map[string]string) (definition.RegistryCache, func() error, error) {
	config := registrycache.Config{URL: cfg["url"]}
	if raw := cfg["ttl"]; raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl < 0 {
			return nil, nil, fmt.Errorf("invalid ttl %q", raw)
		}
		config.TTL = ttl
	}
	for _, entity := range strings.Split(cfg["preload"], ",") {
		if entity = strings.TrimSpace(entity); entity == "" {
			continue
		}
		if strings.ContainsAny(entity, ":/ ") {
			return nil, nil, fmt.Errorf("invalid preload entity %q, want an entity name such as Participant", entity)
		}
		config.Preload = append(config.Preload, entity)
	}

	store, err := registrycache.New(cache, config)
	if err != nil {
		return nil, nil, err
	}
	stop := store.Start(context.WithoutCancel(ctx))
	return store, func() error { stop(); return nil }, nil
}

var Provider = registryCacheProvider{}
