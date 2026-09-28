// The token exchanges -- oauth2 client_credentials and tokenQuery -- and the
// token each provider holds.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// providerToken returns a token to send, exchanging one if the held token has
// expired or there is none.
//
// Scheme-neutral on purpose: it holds a value and an expiry and knows nothing
// about where the token came from or where it is going. oauth2 sends the
// result in a header, tokenQuery in a query parameter, and both want exactly
// this caching.
//
// The lock spans the fetch on purpose: without it a cold start sends every
// concurrent request to the issuer at once. It costs one wait per token
// lifetime.
//
// NOTHING IS CACHED ON FAILURE, so a brief outage at the token endpoint does
// not leave this step holding a failure for a lifetime it never obtained.
func (s *Step) providerToken(ctx context.Context, auth *Authenticator) (string, error) {
	return auth.Token(ctx, s.httpClient, s.config.MaxResponseBytes)
}

// Token returns a token to send, exchanging one if the held token has expired
// or there is none. Scheme-neutral: it holds a value and an expiry and knows
// nothing about where the token came from or where it is going -- oauth2
// sends the result in a header, tokenQuery in a query parameter, and both
// want exactly this caching.
//
// The lock spans the fetch on purpose: without it a cold start sends every
// concurrent request to the issuer at once. It costs one wait per token
// lifetime.
//
// NOTHING IS CACHED ON FAILURE, so a brief outage at the token endpoint does
// not leave this held for a lifetime it never obtained.
func (a *Authenticator) Token(ctx context.Context, client *http.Client, maxResponseBytes int64) (string, error) {
	if held := a.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()

	// Re-checked: whoever held the mutex may already have exchanged one.
	if held := a.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	token, lifetime, err := a.exchange(ctx, client, maxResponseBytes)
	if err != nil {
		return "", err
	}
	a.token.Store(&cachedToken{value: token, expiry: time.Now().Add(lifetime)})
	return token, nil
}

// cachedToken is a token and the moment it stops being trusted.
type cachedToken struct {
	value  string
	expiry time.Time
}

// forgetToken drops a held token, so the next call exchanges a fresh one.
//
// Called when the provider itself rejects the credential, which is the only
// authority on whether a token is still good: an expiry held here is a claim
// about the future -- read from expires_in, or estimated by an operator in
// tokenTtl -- and the provider can disagree with it at any time.
//
// A no-op for a scheme that holds nothing, so the caller does not have to ask
// which scheme it is.
func (s *Step) forgetToken(auth *Authenticator) {
	if auth == nil {
		return
	}
	auth.Forget()
}

// Forget drops a held token, so the next call exchanges a fresh one.
//
// Called when the provider itself rejects the credential, which is the only
// authority on whether a token is still good: an expiry held here is a claim
// about the future -- read from expires_in, or estimated by an operator in
// tokenTtl -- and the provider can disagree with it at any time.
//
// A no-op for a scheme that holds nothing, so the caller does not have to ask
// which scheme it is.
func (a *Authenticator) Forget() {
	if a == nil {
		return
	}
	a.token.Store(nil)
}

// tokenResponse is the part of an OAuth2 token response this step reads.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// exchangeToken obtains a token, by whichever exchange the scheme names.
//
// Every failure carries 502, because it is the exchange with the provider's
// issuer that failed. Unclassified they would surface as 500, telling a peer
// this adapter broke when it did not.
func (s *Step) exchangeToken(ctx context.Context, auth *Authenticator) (string, time.Duration, error) {
	return auth.exchange(ctx, s.httpClient, s.config.MaxResponseBytes)
}

// exchange obtains a token, by whichever exchange the scheme names.
//
// Every failure is a *tokenExchangeErr, classified permanent or not: Step
// wraps it as 502 (the failure is the issuer's, never the caller's) and a
// scheduled caller can read it as a plain error.
func (a *Authenticator) exchange(ctx context.Context, client *http.Client, maxResponseBytes int64) (string, time.Duration, error) {
	if a.cfg.Scheme == util.AuthSchemeTokenQuery {
		return a.exchangeQueryToken(ctx, client, maxResponseBytes)
	}
	return a.exchangeOAuth2Token(ctx, client, maxResponseBytes)
}

// exchangeQueryToken POSTs a JSON body of configured field names and reads the
// token out of a configured response field.
//
// The lifetime comes from config, not from the response: this endpoint says
// nothing about how long its token lives. That is the whole reason tokenTtl is
// required rather than defaulted -- see AuthProfile.
func (a *Authenticator) exchangeQueryToken(ctx context.Context, client *http.Client, maxResponseBytes int64) (string, time.Duration, error) {
	cfg := a.cfg
	user, secret := os.Getenv(cfg.TokenUserEnv), os.Getenv(cfg.TokenSecretEnv)
	if user == "" || secret == "" {
		return "", 0, &missingCredentialErr{provider: cfg.Provider, scheme: util.AuthSchemeTokenQuery,
			envNames: cfg.TokenUserEnv + " and " + cfg.TokenSecretEnv}
	}

	// Marshalled rather than concatenated, so a credential containing a quote
	// or a backslash cannot break out of the JSON it travels in.
	payload, err := json.Marshal(map[string]string{
		cfg.TokenUserField:  user,
		cfg.TokenSecretName: secret,
	})
	if err != nil {
		return "", 0, permanentTokenExchangeErr(
			fmt.Errorf("token request body could not be built: %w", err))
	}

	body, err := a.postForToken(ctx, client, maxResponseBytes, payload, "application/json")
	if err != nil {
		return "", 0, err
	}

	// Decoded into a map because the field name is configured: a struct tag
	// cannot be written for a key that is not known until config is read.
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", 0, permanentTokenExchangeErr(fmt.Errorf("token response from %s is not JSON: %w",
			cfg.TokenURL, err))
	}
	token, ok := fields[cfg.TokenResponseField].(string)
	if !ok || token == "" {
		return "", 0, permanentTokenExchangeErr(fmt.Errorf(
			"token response from %s carries no %s", cfg.TokenURL, cfg.TokenResponseField))
	}

	// The skew is subtracted here for the same reason oauth2 subtracts it from
	// expires_in: a request that passed the expiry check must not arrive after
	// the token died. validate has already refused a ttl at or below it.
	return token, cfg.TokenTTL - util.TokenRefreshSkew, nil
}

// exchangeOAuth2Token performs the client_credentials grant.
func (a *Authenticator) exchangeOAuth2Token(ctx context.Context, client *http.Client, maxResponseBytes int64) (string, time.Duration, error) {
	cfg := a.cfg
	clientID, clientSecret := os.Getenv(cfg.ClientIDEnv), os.Getenv(cfg.ClientSecretEnv)
	if clientID == "" || clientSecret == "" {
		return "", 0, &missingCredentialErr{provider: cfg.Provider, scheme: "oauth2",
			envNames: cfg.ClientIDEnv + " and " + cfg.ClientSecretEnv}
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	body, err := a.postForToken(ctx, client, maxResponseBytes, []byte(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return "", 0, err
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, permanentTokenExchangeErr(fmt.Errorf("token response from %s is not JSON: %w",
			cfg.TokenURL, err))
	}
	if parsed.AccessToken == "" {
		return "", 0, permanentTokenExchangeErr(
			fmt.Errorf("token response from %s carries no access_token", cfg.TokenURL))
	}
	lifetime, ok := tokenLifetime(parsed.ExpiresIn, util.TokenRefreshSkew)
	if !ok {
		return "", 0, permanentTokenExchangeErr(fmt.Errorf(
			"token response from %s carries no usable expires_in, so its lifetime is unknown",
			cfg.TokenURL))
	}
	return parsed.AccessToken, lifetime, nil
}

// postForToken sends the exchange and returns the body of a 2xx, classifying
// every failure the same way for both schemes.
//
// Shared deliberately: the retry decision is the interesting part -- an
// unreachable endpoint, an unreadable response, a 5xx and a 429 are worth
// another attempt, and a 4xx is configuration that will fail identically next
// time. Two copies of that would drift.
func (a *Authenticator) postForToken(ctx context.Context, client *http.Client, maxResponseBytes int64,
	payload []byte, contentType string) ([]byte, error) {
	cfg := a.cfg
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL,
		strings.NewReader(string(payload)))
	if err != nil {
		// A tokenUrl that will not parse is configuration.
		return nil, permanentTokenExchangeErr(
			fmt.Errorf("token request could not be built: %w", err))
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := client.Do(req)
	if err != nil {
		return nil, retryableTokenExchangeErr(fmt.Errorf("token endpoint %s could not be reached: %w",
			cfg.TokenURL, err))
	}
	defer resp.Body.Close()

	// Bounded like any other upstream read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, retryableTokenExchangeErr(fmt.Errorf("token response could not be read: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status, not the body: a failure body routinely quotes the
		// request back. Redacted with what THIS authenticator alone could
		// leak -- a caller with several providers (Step) is expected to widen
		// this if it wants cross-provider coverage in this one log line.
		text := util.Explain(body)
		for _, secret := range a.SecretForms() {
			text = strings.ReplaceAll(text, secret, util.RedactedMarker)
		}
		log.Warnf(ctx, "token endpoint %s returned %s: %s", cfg.TokenURL, resp.Status, text)
		err := fmt.Errorf("token endpoint returned %s", resp.Status)
		// The same rule the provider's own status gets: 5xx and 429 ask to be
		// tried again, and every other 4xx is a statement about the request --
		// here, usually that the credentials are wrong.
		if resp.StatusCode < http.StatusInternalServerError &&
			resp.StatusCode != http.StatusTooManyRequests {
			return nil, permanentTokenExchangeErr(err)
		}
		return nil, retryableTokenExchangeErr(err)
	}
	return body, nil
}

// permanentTokenExchangeErr and retryableTokenExchangeErr classify a failed
// exchange for whichever caller wraps it: Step maps permanent to a
// non-retried 502, and not-permanent to a retried one; a scheduled caller can
// read Error() and try again on its own clock regardless.
func permanentTokenExchangeErr(err error) error {
	return &tokenExchangeErr{cause: err, permanent: true}
}

func retryableTokenExchangeErr(err error) error {
	return &tokenExchangeErr{cause: err}
}

// tokenErr classifies a failed exchange. Always 502: the failure is the
// issuer's, never the caller's.
//
// Retryable unless the caller says otherwise. The exchange is network I/O to a
// third party, so the same 5xx and 429 that earn a retry against the provider
// earn one here; permanent causes are marked with permanentTokenErr instead.
func (s *Step) tokenErr(err error) error {
	return model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable,
		fmt.Errorf("oauth2 token exchange failed: %w", err))
}

// permanentTokenErr is tokenErr for a cause no further attempt would change: a
// token URL that cannot be built, a 4xx that is not 429, or a response the
// issuer will keep sending in the same shape.
func (s *Step) permanentTokenErr(err error) error {
	return util.DoNotRetry(s.tokenErr(err))
}

// tokenLifetime turns expires_in into how long the token may be held, or
// reports that it may not be held at all.
//
// Read from the response, never from config: the issuer owns this number, and a
// configured copy is wrong the moment a realm is retuned -- silently, with
// every request 401ing.
//
// skew is subtracted so a request that passed the check cannot arrive after
// expiry. The false return says "we were not told", which is not a lifetime:
// no expires_in is refused rather than cached for a guessed period.
func tokenLifetime(expiresIn int, skew time.Duration) (time.Duration, bool) {
	if expiresIn <= 0 {
		return 0, false
	}
	lifetime := time.Duration(expiresIn) * time.Second
	// A lifetime at or under the skew would put the expiry in the past, making
	// every request fetch a fresh token. Half is still early enough.
	if lifetime <= skew {
		return lifetime / 2, true
	}
	return lifetime - skew, true
}
