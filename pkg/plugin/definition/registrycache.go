package definition

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrRegistryRecordNotFound reports that no active record, or more than one, matched.
var ErrRegistryRecordNotFound = errors.New("registry record not found")

// RegistryCache reads whole registry records of any entity, keeping the active
// ones in the cache.
type RegistryCache interface {
	// Get returns the one active record of entity whose field equals value (or,
	// for a list field, contains it), exactly as the registry sent it, or
	// ErrRegistryRecordNotFound.
	Get(ctx context.Context, entity, field, value string) (json.RawMessage, error)
	// List returns every active record of entity whose fields match: each field
	// equals the value or, for a list field, contains it. An empty match returns
	// every active record.
	List(ctx context.Context, entity string, match map[string]string) ([]json.RawMessage, error)
}

// RegistryCacheUser is a registry that can read its records through a
// RegistryCache instead of asking the registry each time.
type RegistryCacheUser interface {
	UseRegistryCache(RegistryCache)
}

// RegistryCacheProvider initializes a RegistryCache over the shared cache.
type RegistryCacheProvider interface {
	New(context.Context, Cache, map[string]string) (RegistryCache, func() error, error)
}
