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
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// AuthProfile is how ONE provider's credentials are presented upstream.
//
// One per provider rather than one per step: a step serves several binding
// keys, and the providers behind them need not authenticate alike.
type AuthProfile struct {
	// The participant id this profile belongs to. Held so an error can name
	// it: with several profiles on one step, the field alone leaves an
	// operator guessing which block to look at.
	Provider string

	// One of none, basic, header, query or oauth2.
	Scheme string

	// Variable NAMES, never the values.
	UsernameEnv string
	PasswordEnv string

	// Which header to set, and the variable holding its value.
	HeaderName     string
	HeaderValueEnv string

	// The parameter to add, and the variable holding its value.
	QueryName     string
	QueryValueEnv string

	// The token endpoint. Not a credential, so it is named directly -- but it
	// is deployment-specific, so the reference config carries a placeholder.
	TokenURL string
	// Variable names. The values never appear in config, the registry or a log.
	ClientIDEnv     string
	ClientSecretEnv string
}

// authenticator is one provider's profile plus the token it holds.
//
// The token cache lives HERE rather than on the Step, which is why this type
// exists: a step-wide cache would hand the first oauth2 provider's token to the
// second, authenticating as somebody else.
type authenticator struct {
	cfg AuthProfile

	// Two mechanisms, two jobs. tokenMu serialises the EXCHANGE so a cold
	// start sends one request to the issuer. token is atomic so READERS never
	// take that mutex -- secretForms is one, reached from inside the exchange,
	// and guarding the value with tokenMu deadlocks.
	tokenMu sync.Mutex
	token   atomic.Pointer[cachedToken]
}

// validate refuses a profile whose scheme and fields disagree. Every message
// names the provider, since several profiles share one step.
func (a *AuthProfile) validate() error {
	switch a.Scheme {
	case util.AuthSchemeNone:
	case util.AuthSchemeBasic:
		if a.UsernameEnv == "" || a.PasswordEnv == "" {
			return fmt.Errorf(
				"%s: authScheme basic requires usernameEnv and passwordEnv", a.Provider)
		}
	case util.AuthSchemeHeader:
		if a.HeaderName == "" || a.HeaderValueEnv == "" {
			return fmt.Errorf(
				"%s: authScheme header requires headerName and headerValueEnv", a.Provider)
		}
	case util.AuthSchemeQuery:
		if a.QueryName == "" || a.QueryValueEnv == "" {
			return fmt.Errorf(
				"%s: authScheme query requires queryName and queryValueEnv", a.Provider)
		}
	case util.AuthSchemeOAuth2:
		if a.TokenURL == "" || a.ClientIDEnv == "" || a.ClientSecretEnv == "" {
			return fmt.Errorf(
				"%s: authScheme oauth2 requires tokenUrl, clientIdEnv and clientSecretEnv",
				a.Provider)
		}
	case "":
		return fmt.Errorf("%s: authScheme is required, "+
			"and is none where the upstream needs no credential", a.Provider)
	default:
		return fmt.Errorf(
			"%s: unknown authScheme %q: must be none, basic, header, query or oauth2",
			a.Provider, a.Scheme)
	}
	return nil
}

// ParseProviderAuth builds one credential profile per provider from a plugin's
// flattened settings.
//
// An operator writes a block per provider:
//
//	knowledge-provider:
//	  authScheme: oauth2
//	  tokenUrl: https://issuer.example/token
//
// which pkg/plugin flattens to authScheme-knowledge-provider and friends before
// any plugin sees it. This reads that form back.
//
// The split is on the FIRST dash: no setting name contains one, while a
// participant id routinely does (knowledge-provider, provider.oan.dev), so the
// field is always the part before it.
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
			// The old step-wide form. Refused rather than ignored: dropping it
			// silently leaves every provider with no credential, which reads
			// as the provider rejecting us.
			if _, isSetting := authFields[key]; isSetting {
				return nil, fmt.Errorf(
					"%q is set for the whole step; auth is per provider now, "+
						"so it belongs in a block named for the participant id", key)
			}
			continue
		}
		set, isSetting := authFields[field]
		if !isSetting {
			// Nothing else on a provider step carries a dash, so this is a
			// misspelling rather than a setting to pass through.
			return nil, fmt.Errorf("%q is not a credential setting", key)
		}
		if strings.TrimSpace(provider) == "" {
			return nil, fmt.Errorf("%q names no provider after the dash", key)
		}

		profile, seen := profiles[provider]
		if !seen {
			profile = &AuthProfile{Provider: provider}
			profiles[provider] = profile
		}
		set(profile, config[key])
	}
	return profiles, nil
}

// authFields maps each per-provider setting to the field it fills.
//
// ONE list, not two. The name and the assignment used to sit apart -- a switch
// in ParseProviderAuth and a set of valid names here -- and the two had to stay
// in sync by hand. Forgetting the switch was the silent direction: the setting
// parsed, validated, and was discarded, leaving a provider on a credential the
// operator thought they had configured.
//
// Being the whole vocabulary, this is also what makes the dash split decidable
// and turns a misspelled setting into a startup error.
var authFields = map[string]func(*AuthProfile, string){
	"authScheme":      func(p *AuthProfile, v string) { p.Scheme = v },
	"usernameEnv":     func(p *AuthProfile, v string) { p.UsernameEnv = v },
	"passwordEnv":     func(p *AuthProfile, v string) { p.PasswordEnv = v },
	"headerName":      func(p *AuthProfile, v string) { p.HeaderName = v },
	"headerValueEnv":  func(p *AuthProfile, v string) { p.HeaderValueEnv = v },
	"queryName":       func(p *AuthProfile, v string) { p.QueryName = v },
	"queryValueEnv":   func(p *AuthProfile, v string) { p.QueryValueEnv = v },
	"tokenUrl":        func(p *AuthProfile, v string) { p.TokenURL = v },
	"clientIdEnv":     func(p *AuthProfile, v string) { p.ClientIDEnv = v },
	"clientSecretEnv": func(p *AuthProfile, v string) { p.ClientSecretEnv = v },
}

// providerIDFrom returns the provider half of "<participantId>|<capabilityCode>".
// A participant id carries dashes and dots but never a pipe.
func providerIDFrom(bindingKey string) string {
	provider, _, _ := strings.Cut(bindingKey, "|")
	return strings.TrimSpace(provider)
}

// missingCredential reports an unset credential without naming the variable on
// the wire.
//
// This error is signed and returned to a network peer, and the variable name is
// somebody else's deployment detail -- the caller cannot set it and the fix is
// the operator's. So the name goes to the log and the wire gets the scheme.
func (s *Step) missingCredential(ctx context.Context, provider, scheme, envNames string) error {
	err := fmt.Errorf("the %s credential for %s is not configured", scheme, provider)
	log.Errorf(ctx, err, "%s auth is configured for %s but %s is not set",
		scheme, provider, envNames)
	// Marked here rather than at the call site in attempt, which cannot tell
	// this apart from an oauth2 exchange that failed on the network. An unset
	// environment variable is configuration: retrying reports an operator's
	// missing variable as the provider being down.
	return util.DoNotRetry(err)
}

// authenticate presents this provider's credentials, read from the environment
// at call time so a rotated secret takes effect without a restart.
func (s *Step) authenticate(auth *authenticator, req *http.Request) error {
	cfg := auth.cfg
	switch cfg.Scheme {
	case util.AuthSchemeBasic:
		username, password := os.Getenv(cfg.UsernameEnv), os.Getenv(cfg.PasswordEnv)
		if username == "" || password == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "basic",
				cfg.UsernameEnv+" and "+cfg.PasswordEnv)
		}
		req.SetBasicAuth(username, password)
	case util.AuthSchemeHeader:
		value := os.Getenv(cfg.HeaderValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "header", cfg.HeaderValueEnv)
		}
		req.Header.Set(cfg.HeaderName, value)
	case util.AuthSchemeQuery:
		value := os.Getenv(cfg.QueryValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "query", cfg.QueryValueEnv)
		}
		// Set, not Add: a second copy of the parameter is an ambiguity, and
		// which one an upstream reads is its own business.
		query := req.URL.Query()
		query.Set(cfg.QueryName, value)
		req.URL.RawQuery = query.Encode()
	case util.AuthSchemeOAuth2:
		token, err := s.bearerToken(req.Context(), auth)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}
