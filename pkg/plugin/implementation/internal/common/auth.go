// Credentials: one profile per provider, how a profile is read from config,
// and how it is presented on a request.
package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// One of none, basic, header, query, body, oauth2, tokenQuery or tokenHeader.
	Scheme string

	// Variable NAMES, never the values.
	UsernameEnv string
	PasswordEnv string

	// Which header to set, and the variable holding its value.
	HeaderName     string
	HeaderValueEnv string

	// A second fixed header, sent beside whatever the scheme sends rather than
	// instead of it. See docs/adr/0006-a-binding-may-carry-its-own-credential.md
	ExtraHeaderName     string
	ExtraHeaderValueEnv string

	// tokenHeader only. Put before the token, joined with a space, e.g. Bearer.
	TokenHeaderPrefix string

	// The parameter to add, and the variable holding its value.
	QueryName     string
	QueryValueEnv string

	// The JSON body field to set, and the variable holding its value.
	BodyField    string
	BodyValueEnv string

	// Non-2xx statuses this provider uses for a real answer, such as SATHI's 409
	// for "no stock". Their body goes to the response mapping instead of failing.
	AnswerStatusesRaw string
	AnswerStatuses    []int

	// The token endpoint. Not a credential, so it is named directly -- but it
	// is deployment-specific, so the reference config carries a placeholder.
	TokenURL string
	// Variable names. The values never appear in config, the registry or a log.
	ClientIDEnv     string
	ClientSecretEnv string

	// tokenQuery only. The JSON keys the token endpoint expects, and the
	// variables holding what goes under them.
	//
	// The KEYS are configured because they are one provider's spelling rather
	// than a standard: the live one this was written against wants
	// access_name and password, and the next will want something else.
	TokenUserField  string
	TokenUserEnv    string
	TokenSecretName string
	TokenSecretEnv  string

	// The fixed part of the login request, as a JSON object (e.g. a GraphQL
	// query); the secret is added into it, a dotted field meaning one level down.
	TokenRequestBody string

	// Where the token sits in the response body, as a dotted path.
	TokenResponseField string

	// How long a token may be held, for a response that states no expiry. Too
	// long costs one rejected call: the rejection drops the token.
	TokenTTLRaw string
	TokenTTL    time.Duration

	// Where the response states the token's expiry, as a dotted path to Unix
	// seconds or an RFC 3339 time. Set this or tokenTtl: the issuer's own
	// expiry beats an estimate.
	TokenExpiryField string
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
	case util.AuthSchemeBody:
		if a.BodyField == "" || a.BodyValueEnv == "" {
			return fmt.Errorf(
				"%s: authScheme body requires bodyField and bodyValueEnv", a.Provider)
		}
	case util.AuthSchemeOAuth2:
		if a.TokenURL == "" || a.ClientIDEnv == "" || a.ClientSecretEnv == "" {
			return fmt.Errorf(
				"%s: authScheme oauth2 requires tokenUrl, clientIdEnv and clientSecretEnv",
				a.Provider)
		}
	case util.AuthSchemeTokenQuery, util.AuthSchemeTokenHeader:
		// Every one of these is required, and none can be defaulted: the JSON
		// keys, the response key and the parameter or header name are all this
		// provider's spelling, and guessing any of them sends a malformed
		// request whose rejection says nothing about the cause.
		if a.TokenURL == "" || a.TokenSecretName == "" || a.TokenSecretEnv == "" ||
			a.TokenResponseField == "" {
			return fmt.Errorf("%s: authScheme %s requires tokenUrl, "+
				"tokenSecretField, tokenSecretEnv and tokenResponseField", a.Provider, a.Scheme)
		}
		if err := a.validateTokenUser(); err != nil {
			return err
		}
		if err := a.validateTokenRequestBody(); err != nil {
			return err
		}
		if a.Scheme == util.AuthSchemeTokenQuery && a.QueryName == "" {
			return fmt.Errorf("%s: authScheme tokenQuery requires queryName", a.Provider)
		}
		if a.Scheme == util.AuthSchemeTokenHeader && a.HeaderName == "" {
			return fmt.Errorf("%s: authScheme tokenHeader requires headerName", a.Provider)
		}
		// The lifetime comes from exactly one of the two: a field the provider
		// sets on each token, or a fixed duration configured here.
		switch {
		case a.TokenExpiryField != "" && a.TokenTTLRaw != "":
			// Two lifetimes would leave an operator guessing which one holds.
			return fmt.Errorf("%s: set tokenTtl or tokenExpiryField, not both", a.Provider)

		case a.TokenExpiryField != "":
			// The provider states it per token, so there is nothing to parse.

		case a.TokenTTLRaw != "":
			ttl, err := time.ParseDuration(a.TokenTTLRaw)
			if err != nil {
				return fmt.Errorf("%s: tokenTtl %q is not a duration: %w",
					a.Provider, a.TokenTTLRaw, err)
			}
			// Below the skew every token is already expired when it arrives,
			// so each call would exchange one and then discard it -- two round
			// trips per request and a token endpoint hit at the request rate.
			if ttl <= util.TokenRefreshSkew {
				return fmt.Errorf("%s: tokenTtl %s must exceed the %s refresh skew, "+
					"or every token is expired before it is used",
					a.Provider, ttl, util.TokenRefreshSkew)
			}
			a.TokenTTL = ttl

		default:
			return fmt.Errorf("%s: authScheme %s requires tokenTtl or tokenExpiryField, "+
				"or the token's lifetime is unknown", a.Provider, a.Scheme)
		}
	case "":
		return fmt.Errorf("%s: authScheme is required, "+
			"and is none where the upstream needs no credential", a.Provider)
	default:
		return fmt.Errorf(
			"%s: unknown authScheme %q: must be none, basic, header, query, body, "+
				"oauth2, tokenQuery or tokenHeader", a.Provider, a.Scheme)
	}
	// Independent of the scheme, so checked outside the switch. One without
	// the other is a half-written block: a name with no variable sends an
	// empty header, and a variable with no name sends nothing at all.
	if (a.ExtraHeaderName == "") != (a.ExtraHeaderValueEnv == "") {
		return fmt.Errorf(
			"%s: extraHeaderName and extraHeaderValueEnv are set together or not at all",
			a.Provider)
	}
	if a.ExtraHeaderName != "" && strings.EqualFold(a.ExtraHeaderName, a.HeaderName) {
		return fmt.Errorf(
			"%s: extraHeaderName %q is the header the scheme already sets, so one would overwrite the other",
			a.Provider, a.ExtraHeaderName)
	}
	if a.AnswerStatusesRaw != "" {
		statuses, err := parseAnswerStatuses(a.Provider, a.AnswerStatusesRaw)
		if err != nil {
			return err
		}
		a.AnswerStatuses = statuses
	}
	return nil
}

// parseAnswerStatuses reads a comma-separated list of 4xx statuses. 5xx stays a
// failure so it is still retried, and 2xx is already an answer.
func parseAnswerStatuses(provider, raw string) ([]int, error) {
	var statuses []int
	for _, part := range strings.Split(raw, ",") {
		status, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || status < 400 || status > 499 {
			return nil, fmt.Errorf("%s: answerStatuses %q must be a comma-separated list of 4xx statuses",
				provider, raw)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// isAnswer reports whether this provider uses status for a real answer.
func (a *AuthProfile) isAnswer(status int) bool {
	return slices.Contains(a.AnswerStatuses, status)
}

// validateTokenUser keeps the user optional only for a login with a fixed request
// body, such as a refresh-token login; a plain login must name its user.
func (a *AuthProfile) validateTokenUser() error {
	hasField, hasEnv := a.TokenUserField != "", a.TokenUserEnv != ""
	if hasField != hasEnv || (!hasField && a.TokenRequestBody == "") {
		return fmt.Errorf("%s: authScheme %s needs tokenUserField and tokenUserEnv together; "+
			"only a login with tokenRequestBody may leave both out", a.Provider, a.Scheme)
	}
	return nil
}

// validateTokenRequestBody refuses a body that is not a JSON object, at startup
// rather than on the first call.
func (a *AuthProfile) validateTokenRequestBody() error {
	if a.TokenRequestBody == "" {
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(a.TokenRequestBody), &body); err != nil || body == nil {
		return fmt.Errorf("%s: tokenRequestBody must be a JSON object", a.Provider)
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
	"authScheme":     func(p *AuthProfile, v string) { p.Scheme = v },
	"usernameEnv":    func(p *AuthProfile, v string) { p.UsernameEnv = v },
	"passwordEnv":    func(p *AuthProfile, v string) { p.PasswordEnv = v },
	"headerName":     func(p *AuthProfile, v string) { p.HeaderName = v },
	"headerValueEnv": func(p *AuthProfile, v string) { p.HeaderValueEnv = v },

	"extraHeaderName":     func(p *AuthProfile, v string) { p.ExtraHeaderName = v },
	"extraHeaderValueEnv": func(p *AuthProfile, v string) { p.ExtraHeaderValueEnv = v },
	"queryName":           func(p *AuthProfile, v string) { p.QueryName = v },
	"queryValueEnv":       func(p *AuthProfile, v string) { p.QueryValueEnv = v },
	"bodyField":           func(p *AuthProfile, v string) { p.BodyField = v },
	"bodyValueEnv":        func(p *AuthProfile, v string) { p.BodyValueEnv = v },
	"answerStatuses":      func(p *AuthProfile, v string) { p.AnswerStatusesRaw = v },
	"tokenUrl":            func(p *AuthProfile, v string) { p.TokenURL = v },
	"clientIdEnv":         func(p *AuthProfile, v string) { p.ClientIDEnv = v },
	"clientSecretEnv":     func(p *AuthProfile, v string) { p.ClientSecretEnv = v },

	"tokenUserField":     func(p *AuthProfile, v string) { p.TokenUserField = v },
	"tokenUserEnv":       func(p *AuthProfile, v string) { p.TokenUserEnv = v },
	"tokenSecretField":   func(p *AuthProfile, v string) { p.TokenSecretName = v },
	"tokenSecretEnv":     func(p *AuthProfile, v string) { p.TokenSecretEnv = v },
	"tokenResponseField": func(p *AuthProfile, v string) { p.TokenResponseField = v },
	// Kept as written and turned into a duration by validate, so a bad value is
	// refused at startup naming the provider rather than read as zero -- which
	// would mean re-exchanging a token on every single call.
	"tokenTtl":          func(p *AuthProfile, v string) { p.TokenTTLRaw = v },
	"tokenExpiryField":  func(p *AuthProfile, v string) { p.TokenExpiryField = v },
	"tokenRequestBody":  func(p *AuthProfile, v string) { p.TokenRequestBody = v },
	"tokenHeaderPrefix": func(p *AuthProfile, v string) { p.TokenHeaderPrefix = v },
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

// withBodyCredential returns the mapped body with the body scheme's credential
// added. Other schemes get the body back unchanged.
func (s *Step) withBodyCredential(ctx context.Context, auth *authenticator, method string, mapped []byte) ([]byte, error) {
	cfg := auth.cfg
	if cfg.Scheme != util.AuthSchemeBody {
		return mapped, nil
	}
	if !util.HasBody(method) {
		return nil, util.DoNotRetry(fmt.Errorf(
			"%s: authScheme body needs a request body, and %s sends none", cfg.Provider, method))
	}
	value := os.Getenv(cfg.BodyValueEnv)
	if value == "" {
		return nil, s.missingCredential(ctx, cfg.Provider, "body", cfg.BodyValueEnv)
	}
	return setBodyField(cfg.Provider, mapped, cfg.BodyField, value)
}

// setBodyField sets one field on a JSON object body, keeping every other field.
func setBodyField(provider string, body []byte, field, value string) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, util.DoNotRetry(fmt.Errorf(
				"%s: authScheme body needs the mapped request to be a JSON object: %w", provider, err))
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, util.DoNotRetry(fmt.Errorf("%s: could not encode the body credential: %w", provider, err))
	}
	fields[field] = encoded
	return json.Marshal(fields)
}

// authenticate presents this provider's credentials, read from the environment
// at call time so a rotated secret takes effect without a restart, plus the
// farmer's session if this call carries one.
func (s *Step) authenticate(auth *authenticator, session sessionCredential, req *http.Request) error {
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
		token, err := s.providerToken(req.Context(), auth)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)

	case util.AuthSchemeTokenHeader:
		// Exchanged like tokenQuery, sent in the header behind the optional prefix.
		token, err := s.providerToken(req.Context(), auth)
		if err != nil {
			return err
		}
		req.Header.Set(cfg.HeaderName, strings.TrimSpace(cfg.TokenHeaderPrefix+" "+token))

	case util.AuthSchemeTokenQuery:
		// Exchanged like oauth2, placed like query: the same held token, put
		// where this provider reads it from.
		token, err := s.providerToken(req.Context(), auth)
		if err != nil {
			return err
		}
		query := req.URL.Query()
		query.Set(cfg.QueryName, token)
		req.URL.RawQuery = query.Encode()
	}

	// A second credential this deployment holds. PMFBY's insurance API wants
	// both an exchanged token and a fixed password on one request, and a
	// scheme sends exactly one thing.
	if cfg.ExtraHeaderName != "" {
		value := os.Getenv(cfg.ExtraHeaderValueEnv)
		if value == "" {
			return s.missingCredential(req.Context(), cfg.Provider, "extraHeader",
				cfg.ExtraHeaderValueEnv)
		}
		req.Header.Set(cfg.ExtraHeaderName, value)
	}

	// The farmer's session, set AFTER the switch so it rides beside the
	// deployment's own credential rather than instead of it. AIF checks two
	// things on the same request: the service apiKey that says which
	// integrator is calling, and the bearer that says which farmer consented.
	// A scheme cannot express that, because a binding has exactly one.
	if session.Name != "" && session.Value != "" {
		req.Header.Set(session.Name, session.Value)
	}
	return nil
}
