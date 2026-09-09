// The oauth2 client_credentials exchange and the token each provider holds.
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

// bearerToken returns a token to send, exchanging one if the held token has
// expired or there is none.
//
// The lock spans the fetch on purpose: without it a cold start sends every
// concurrent request to the issuer at once. It costs one wait per token
// lifetime.
//
// NOTHING IS CACHED ON FAILURE, so a brief outage at the token endpoint does
// not leave this step holding a failure for a lifetime it never obtained.
func (s *Step) bearerToken(ctx context.Context, auth *authenticator) (string, error) {
	if held := auth.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	auth.tokenMu.Lock()
	defer auth.tokenMu.Unlock()

	// Re-checked: whoever held the mutex may already have exchanged one.
	if held := auth.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	token, lifetime, err := s.exchangeToken(ctx, auth)
	if err != nil {
		return "", err
	}
	auth.token.Store(&cachedToken{value: token, expiry: time.Now().Add(lifetime)})
	return token, nil
}

// cachedToken is a token and the moment it stops being trusted.
type cachedToken struct {
	value  string
	expiry time.Time
}

// tokenResponse is the part of an OAuth2 token response this step reads.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// exchangeToken performs the client_credentials grant.
//
// Every failure carries 502, because it is the exchange with the provider's
// issuer that failed. Unclassified they would surface as 500, telling a peer
// this adapter broke when it did not.
func (s *Step) exchangeToken(ctx context.Context, auth *authenticator) (string, time.Duration, error) {
	cfg := auth.cfg
	clientID, clientSecret := os.Getenv(cfg.ClientIDEnv), os.Getenv(cfg.ClientSecretEnv)
	if clientID == "" || clientSecret == "" {
		return "", 0, s.missingCredential(ctx, cfg.Provider, "oauth2",
			cfg.ClientIDEnv+" and "+cfg.ClientSecretEnv)
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		// A tokenUrl that will not parse is configuration.
		return "", 0, s.permanentTokenErr(
			fmt.Errorf("token request could not be built: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token endpoint %s could not be reached: %w",
			cfg.TokenURL, err))
	}
	defer resp.Body.Close()

	// Bounded like any other upstream read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token response could not be read: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status, not the body: a failure body routinely quotes the
		// request back.
		log.Warnf(ctx, "token endpoint %s returned %s: %s",
			cfg.TokenURL, resp.Status, s.redactString(util.Explain(body)))
		err := fmt.Errorf("token endpoint returned %s", resp.Status)
		// The same rule the provider's own status gets: 5xx and 429 ask to be
		// tried again, and every other 4xx is a statement about the request --
		// here, usually that the client credentials are wrong.
		if resp.StatusCode < http.StatusInternalServerError &&
			resp.StatusCode != http.StatusTooManyRequests {
			return "", 0, s.permanentTokenErr(err)
		}
		return "", 0, s.tokenErr(err)
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A 2xx that is not JSON is the issuer misbehaving in a way another
		// attempt will not change.
		return "", 0, s.permanentTokenErr(fmt.Errorf("token response from %s is not JSON: %w",
			cfg.TokenURL, err))
	}
	if parsed.AccessToken == "" {
		return "", 0, s.permanentTokenErr(
			fmt.Errorf("token response from %s carries no access_token", cfg.TokenURL))
	}
	lifetime, ok := tokenLifetime(parsed.ExpiresIn, util.TokenRefreshSkew)
	if !ok {
		return "", 0, s.permanentTokenErr(fmt.Errorf(
			"token response from %s carries no usable expires_in, so its lifetime is unknown",
			cfg.TokenURL))
	}
	return parsed.AccessToken, lifetime, nil
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
