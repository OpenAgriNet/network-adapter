// Credentials: one profile per provider, how a profile is read from config,
// and how it is presented on a request.
package common

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

// AuthProfile is how ONE provider's credentials are presented upstream.
//
// One per provider rather than one per step: a step serves several binding
// keys, and the providers behind them need not authenticate alike.
type AuthProfile struct {
	// Provider is the participant id this profile belongs to. Held so an error
	// can name it: with several profiles on one step, "authScheme query
	// requires queryName" would otherwise leave an operator guessing which
	// provider it meant.
	Provider string

	// Scheme is one of none, basic, header, query or oauth2. Providers differ
	// here, which is why it is configuration and not an assumption.
	Scheme string

	// UsernameEnv and PasswordEnv name the environment variables holding basic
	// credentials. They are variable NAMES, never the values.
	UsernameEnv string
	PasswordEnv string

	// HeaderName and HeaderValueEnv configure the header scheme: which header
	// to set, and which environment variable holds its value.
	HeaderName     string
	HeaderValueEnv string

	// QueryName and QueryValueEnv configure the query scheme: the parameter
	// name to add, and the environment variable holding its value. Named the
	// same way as the header pair, for the same reason -- the credential is
	// never in this config, only the name of the variable carrying it.
	QueryName     string
	QueryValueEnv string

	// TokenURL is the OAuth2 token endpoint. Not a credential, so it is named
	// here rather than through an environment variable -- but it IS
	// deployment-specific, so the reference config carries a placeholder.
	TokenURL string
	// ClientIDEnv and ClientSecretEnv name the variables holding the client
	// credentials. The values never appear in config, the registry, or a log.
	ClientIDEnv     string
	ClientSecretEnv string
}

// authenticator is one provider's profile plus the token it holds.
//
// The token cache lives HERE rather than on the Step, and that is the whole
// reason this type exists. A step-wide cache shared between two oauth2
// providers would hand the first provider's token to the second, which is
// authenticating as somebody else -- a failure no test of a single provider
// can see.
type authenticator struct {
	cfg AuthProfile

	// Two mechanisms because there are two jobs. tokenMu serialises the
	// EXCHANGE, so a cold start sends one request to the issuer rather than one
	// per concurrent caller. token is atomic so READERS never take that mutex,
	// which matters because secretForms is one of them and it is reached from
	// inside the exchange -- guarding the value with tokenMu instead deadlocked
	// on the first failing exchange, which is how this was found.
	tokenMu sync.Mutex
	token   atomic.Pointer[cachedToken]
}

// validate refuses a profile whose scheme and fields disagree. Every message
// names the provider: with several profiles on one step, the field alone would
// leave an operator guessing which block to look at.
func (a *AuthProfile) validate() error {
	switch a.Scheme {
	case AuthSchemeNone:
	case AuthSchemeBasic:
		if a.UsernameEnv == "" || a.PasswordEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme basic requires usernameEnv and passwordEnv", a.Provider)
		}
	case AuthSchemeHeader:
		if a.HeaderName == "" || a.HeaderValueEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme header requires headerName and headerValueEnv", a.Provider)
		}
	case AuthSchemeQuery:
		if a.QueryName == "" || a.QueryValueEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme query requires queryName and queryValueEnv", a.Provider)
		}
	case AuthSchemeOAuth2:
		if a.TokenURL == "" || a.ClientIDEnv == "" || a.ClientSecretEnv == "" {
			return fmt.Errorf(
				"upstream: %s: authScheme oauth2 requires tokenUrl, clientIdEnv and clientSecretEnv",
				a.Provider)
		}
	case "":
		return fmt.Errorf("upstream: %s: authScheme is required, "+
			"and is none where the upstream needs no credential", a.Provider)
	default:
		return fmt.Errorf(
			"upstream: %s: unknown authScheme %q: must be none, basic, header, query or oauth2",
			a.Provider, a.Scheme)
	}
	return nil
}

// ParseProviderAuth builds one credential profile per provider from a plugin's
// flattened settings.
//
// What an operator writes is a block per provider:
//
//	knowledge-provider:
//	  authScheme: oauth2
//	  tokenUrl: https://issuer.example/token
//
// which pkg/plugin flattens to authScheme-knowledge-provider and
// tokenUrl-knowledge-provider before any plugin sees it. This reads that form
// back into profiles.
//
// Shared rather than repeated in each capability plugin: all of them copied
// the same field list out of the same map, so a scheme added in one place had
// to be remembered in three.
//
// The split is on the FIRST dash, which is unambiguous because no setting name
// contains one while a participant id routinely does -- knowledge-provider,
// provider.oan.dev. So the field is always the part before it.
func ParseProviderAuth(config map[string]string) (map[string]*AuthProfile, error) {
	profiles := map[string]*AuthProfile{}

	// Sorted so a config with two mistakes reports the same one every run.
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		field, provider, dashed := strings.Cut(key, "-")
		if !dashed {
			// A bare auth field is the old step-wide form. Refused rather than
			// ignored: silently dropping it leaves every provider on no
			// credential at all, which reads as the provider rejecting us.
			if authFields[key] {
				return nil, fmt.Errorf(
					"upstream: %q is set for the whole step; auth is per provider now, "+
						"so it belongs in a block named for the participant id", key)
			}
			continue
		}
		if !authFields[field] {
			// Not an auth setting, and nothing else on a provider step carries
			// a dash -- so this is a misspelled field rather than something to
			// pass through.
			return nil, fmt.Errorf("upstream: %q is not a credential setting", key)
		}
		if strings.TrimSpace(provider) == "" {
			return nil, fmt.Errorf("upstream: %q names no provider after the dash", key)
		}

		profile, seen := profiles[provider]
		if !seen {
			profile = &AuthProfile{Provider: provider}
			profiles[provider] = profile
		}
		value := config[key]
		switch field {
		case "authScheme":
			profile.Scheme = value
		case "usernameEnv":
			profile.UsernameEnv = value
		case "passwordEnv":
			profile.PasswordEnv = value
		case "headerName":
			profile.HeaderName = value
		case "headerValueEnv":
			profile.HeaderValueEnv = value
		case "queryName":
			profile.QueryName = value
		case "queryValueEnv":
			profile.QueryValueEnv = value
		case "tokenUrl":
			profile.TokenURL = value
		case "clientIdEnv":
			profile.ClientIDEnv = value
		case "clientSecretEnv":
			profile.ClientSecretEnv = value
		}
	}
	return profiles, nil
}

// authFields is the closed set of per-provider credential settings. Closed on
// purpose: it is what makes the split on the first dash decidable, and what
// turns a misspelled field into a startup error rather than a setting that
// quietly does nothing.
var authFields = map[string]bool{
	"authScheme":      true,
	"usernameEnv":     true,
	"passwordEnv":     true,
	"headerName":      true,
	"headerValueEnv":  true,
	"queryName":       true,
	"queryValueEnv":   true,
	"tokenUrl":        true,
	"clientIdEnv":     true,
	"clientSecretEnv": true,
}

// providerIDFrom returns the provider half of a binding key. The format is
// "<participantId>|<capabilityCode>", and a participant id carries dashes and
// dots but never a pipe, so the first one separates them.
func providerIDFrom(bindingKey string) string {
	provider, _, _ := strings.Cut(bindingKey, "|")
	return strings.TrimSpace(provider)
}

// authenticate presents this provider's credentials, read from the environment
// at call time so a rotated secret takes effect without a restart.
// missingCredential reports an unset credential without naming the variable on
// the wire.
//
// The variable name is deployment configuration, and this error is wrapped into
// a 502 that is signed and returned to a network peer. Telling a peer that
// MANDI_TOKEN is what this deployment reads describes the inside of somebody
// else's stack for no benefit to the caller -- the caller cannot set it, and
// the fix is entirely the operator's. So the name goes to the log, where the
// operator is, and the wire gets the scheme that failed.
func (s *Step) missingCredential(ctx context.Context, provider, scheme, envNames string) error {
	err := fmt.Errorf("upstream: the %s credential for %s is not configured", scheme, provider)
	log.Errorf(ctx, err, "upstream: %s auth is configured for %s but %s is not set",
		scheme, provider, envNames)
	return err
}

func (s *Step) authenticate(auth *authenticator, req *http.Request) error {
	cfg := auth.cfg
	switch cfg.Scheme {
	case AuthSchemeBasic:
		username, password := os.Getenv(cfg.UsernameEnv), os.Getenv(cfg.PasswordEnv)
		if username == "" || password == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "basic",
				cfg.UsernameEnv+" and "+cfg.PasswordEnv)
		}
		req.SetBasicAuth(username, password)
	case AuthSchemeHeader:
		value := os.Getenv(cfg.HeaderValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "header", cfg.HeaderValueEnv)
		}
		req.Header.Set(cfg.HeaderName, value)
	case AuthSchemeQuery:
		value := os.Getenv(cfg.QueryValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "query", cfg.QueryValueEnv)
		}
		// Set rather than Add: a second copy of the parameter is not a
		// credential, it is an ambiguity, and which one an upstream reads is
		// its own business.
		query := req.URL.Query()
		query.Set(cfg.QueryName, value)
		req.URL.RawQuery = query.Encode()
	case AuthSchemeOAuth2:
		token, err := s.bearerToken(req.Context(), auth)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}
