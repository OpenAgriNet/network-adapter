// Where an authenticator's credential values come from: the environment for a
// config profile, the secret store for a registry row.
package common

import (
	"context"
	"os"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

// SecretStore reads one key of one provider's entry. Implemented by
// internal/secretstore; an interface so tests need no OpenBao.
type SecretStore interface {
	Value(ctx context.Context, entry, key string) (string, error)
	// Held is what is already in memory, without fetching: for redaction.
	Held(entry, key string) (string, bool)
}

// Secrets gives credential values by name: the environment for a config
// profile, the provider's secret store entry for a registry row.
type Secrets interface {
	// Value is "" when the secret is unset or cannot be read; the reason is logged.
	Value(ctx context.Context, name string) string
	// Held is a value already in memory, without fetching: for redaction.
	Held(name string) string
}

// envSecrets reads the environment, as config profiles always have.
type envSecrets struct{}

func (envSecrets) Value(_ context.Context, name string) string { return os.Getenv(name) }
func (envSecrets) Held(name string) string                     { return os.Getenv(name) }

// storeSecrets reads one provider's entry in the secret store.
type storeSecrets struct {
	store SecretStore
	entry string
}

func (s storeSecrets) Value(ctx context.Context, name string) string {
	value, err := s.store.Value(ctx, s.entry, name)
	if err != nil {
		log.Errorf(ctx, err, "Secret %s for %s could not be read from the secret store", name, s.entry)
		return ""
	}
	return value
}

func (s storeSecrets) Held(name string) string {
	value, _ := s.store.Held(s.entry, name)
	return value
}

// secret is a credential value for this authenticator, read when it is used so
// a rotated one takes effect without a restart.
func (a *authenticator) secret(ctx context.Context, name string) string {
	if a.secrets == nil {
		return os.Getenv(name)
	}
	return a.secrets.Value(ctx, name)
}

// heldSecret is a credential value already in memory, for redaction only.
func (a *authenticator) heldSecret(name string) string {
	if a.secrets == nil {
		return os.Getenv(name)
	}
	return a.secrets.Held(name)
}
