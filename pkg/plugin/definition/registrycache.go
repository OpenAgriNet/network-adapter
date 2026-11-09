package definition

import (
	"context"
	"encoding/json"
)

// RegistryCache keeps active registry records of any entity in the cache.
type RegistryCache interface {
	// List returns the cached active records of entity whose fields match, or an
	// error when none are cached.
	List(ctx context.Context, entity string, match map[string]string) ([]json.RawMessage, error)
	// Refresh reloads entity's records soon, when a reader found them stale.
	Refresh(entity string)
}

// RegistryCacheUser is a registry that can read its records through a RegistryCache.
type RegistryCacheUser interface {
	UseRegistryCache(RegistryCache)
}

// RegistryCacheProvider initializes a RegistryCache over the shared cache.
type RegistryCacheProvider interface {
	New(context.Context, Cache, map[string]string) (RegistryCache, func() error, error)
}
