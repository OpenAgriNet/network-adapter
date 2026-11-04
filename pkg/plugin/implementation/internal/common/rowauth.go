// Credentials taken from a provider's registry row instead of the step config,
// so a new provider needs only its row.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// rowAuthenticators holds one authenticator per binding whose row carries auth.
// Kept while the row is unchanged, so a token it fetched is reused.
type rowAuthenticators struct {
	mu        sync.Mutex
	byBinding map[string]rowAuthenticator
}

type rowAuthenticator struct {
	fingerprint string
	auth        *authenticator
}

// authFor is the credential for plan: its row's auth block when it has one,
// else the step config's profile for its provider.
func (s *Step) authFor(ctx context.Context, plan *model.ProviderRecord) (*authenticator, error) {
	if plan.Auth == nil {
		return s.configAuth(plan)
	}
	return s.rowAuth.forRow(ctx, plan, s.config.SecretStore)
}

func (s *Step) configAuth(plan *model.ProviderRecord) (*authenticator, error) {
	auth, configured := s.auth[providerIDFrom(plan.BindingKey)]
	if !configured {
		return nil, fmt.Errorf("no credential is configured for %s", plan.BindingKey)
	}
	return auth, nil
}

// forRow returns the binding's authenticator, building a new one only when the
// row's auth block differs from the one it was built from.
func (r *rowAuthenticators) forRow(ctx context.Context, plan *model.ProviderRecord, store SecretStore) (*authenticator, error) {
	fingerprint, err := json.Marshal(plan.Auth)
	if err != nil {
		return nil, fmt.Errorf("%s: its registry row's auth block cannot be read: %w", plan.BindingKey, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	held, seen := r.byBinding[plan.BindingKey]
	if seen && held.fingerprint == string(fingerprint) {
		return held.auth, nil
	}

	profile, err := profileFromRow(providerIDFrom(plan.BindingKey), plan.Auth)
	if err != nil {
		log.Errorf(ctx, err, "Refusing %s: the auth block in its registry row is not usable", plan.BindingKey)
		return nil, fmt.Errorf("%s: the auth block in its registry row is not usable: %w", plan.BindingKey, err)
	}
	auth := &authenticator{cfg: *profile, secrets: rowSecrets(ctx, plan, store)}
	if r.byBinding == nil {
		r.byBinding = map[string]rowAuthenticator{}
	}
	r.byBinding[plan.BindingKey] = rowAuthenticator{fingerprint: string(fingerprint), auth: auth}

	if seen {
		log.Infof(ctx, "%s: auth changed in its registry row, now scheme %s; any token held is dropped",
			plan.BindingKey, profile.Scheme)
	} else {
		log.Infof(ctx, "%s: auth taken from its registry row, scheme %s", plan.BindingKey, profile.Scheme)
	}
	return auth, nil
}

// rowSecrets is where a row's credentials are read: the provider's own entry in
// the secret store, or the environment when no store is configured.
func rowSecrets(ctx context.Context, plan *model.ProviderRecord, store SecretStore) secretSource {
	if store == nil {
		log.Infof(ctx, "%s: no secret store is configured, so its credentials are read from the environment", plan.BindingKey)
		return envSecrets{}
	}
	return storeSecrets{store: store, entry: providerIDFrom(plan.BindingKey)}
}

// all lists every authenticator built so far, for redaction.
func (r *rowAuthenticators) all() []*authenticator {
	r.mu.Lock()
	defer r.mu.Unlock()
	auths := make([]*authenticator, 0, len(r.byBinding))
	for _, held := range r.byBinding {
		auths = append(auths, held.auth)
	}
	return auths
}

// profileFromRow reads a row's auth block through the same parser and checks as
// the step config, so a row is refused for exactly what a config would be.
func profileFromRow(provider string, auth map[string]any) (*AuthProfile, error) {
	keys, err := authKeysFromRow(provider, auth)
	if err != nil {
		return nil, err
	}
	profiles, err := ParseProviderAuth(keys)
	if err != nil {
		return nil, err
	}
	profile := profiles[provider]
	if err := profile.validate(); err != nil {
		return nil, err
	}
	return profile, nil
}

// authKeysFromRow writes a row's auth block in the step config's flat form. Only
// the block scheme names is read: a switched row can still hold the old one.
func authKeysFromRow(provider string, auth map[string]any) (map[string]string, error) {
	scheme, _ := auth["scheme"].(string)
	if scheme == "" {
		return nil, fmt.Errorf("%s: the auth block has no scheme", provider)
	}
	keys := map[string]string{"authScheme-" + provider: scheme}
	raw, present := auth[scheme]
	if scheme == util.AuthSchemeNone || !present {
		return keys, nil
	}

	block, isBlock := raw.(map[string]any)
	if !isBlock {
		return nil, fmt.Errorf("%s: auth.%s is not an object", provider, scheme)
	}
	for field, value := range block {
		text, isText := value.(string)
		if !isText {
			return nil, fmt.Errorf("%s: auth.%s.%s is not text", provider, scheme, field)
		}
		keys[configName(field)+"-"+provider] = text
	}
	return keys, nil
}

// configName is a row field's name in the step config, where a Ref is an Env.
func configName(field string) string {
	if name, isRef := strings.CutSuffix(field, "Ref"); isRef {
		return name + "Env"
	}
	return field
}
