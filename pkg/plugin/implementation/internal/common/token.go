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
)

// bearerToken returns a token to send, exchanging one if what we hold has
// expired or if we hold none.
//
// The lock spans the fetch on purpose. Without it a cold start sends every
// concurrent request to the token endpoint, and the provider's issuer sees a
// burst of identical exchanges. Serialising them costs one wait per token
// lifetime and nothing after that, which is a better trade than a second
// caching layer.
//
// NOTHING IS CACHED ON FAILURE. A transient outage at the token endpoint must
// not leave this step holding a failure for the lifetime it never obtained --
// the next request tries again.
func (s *Step) bearerToken(ctx context.Context, auth *authenticator) (string, error) {
	if held := auth.token.Load(); held != nil && time.Now().Before(held.expiry) {
		return held.value, nil
	}

	auth.tokenMu.Lock()
	defer auth.tokenMu.Unlock()

	// Re-checked after acquiring: while this caller waited, whoever held the
	// mutex may already have exchanged a fresh token.
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
// Every failure here is the UPSTREAM exchange failing, so all of them carry
// 502. Unclassified they would surface as a 500, which tells a network peer
// this adapter broke when in fact the provider's issuer did.
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
		return "", 0, s.tokenErr(fmt.Errorf("token request could not be built: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token endpoint %s could not be reached: %w",
			cfg.TokenURL, err))
	}
	defer resp.Body.Close()

	// Bounded like any other upstream read: a token response is small, and an
	// unbounded read here would be a hole in the same wall.
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.config.MaxResponseBytes+1))
	if err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token response could not be read: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The status, not the body: what an issuer puts in a failure body is its
		// own business, and it routinely quotes the request back.
		log.Warnf(ctx, "upstream: token endpoint %s returned %s: %s",
			cfg.TokenURL, resp.Status, s.redactString(explain(body)))
		return "", 0, s.tokenErr(fmt.Errorf("token endpoint returned %s", resp.Status))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, s.tokenErr(fmt.Errorf("token response from %s is not JSON: %w",
			cfg.TokenURL, err))
	}
	if parsed.AccessToken == "" {
		return "", 0, s.tokenErr(fmt.Errorf("token response from %s carries no access_token",
			cfg.TokenURL))
	}
	lifetime, ok := tokenLifetime(parsed.ExpiresIn, tokenRefreshSkew)
	if !ok {
		return "", 0, s.tokenErr(fmt.Errorf(
			"token response from %s carries no usable expires_in, so its lifetime is unknown",
			cfg.TokenURL))
	}
	return parsed.AccessToken, lifetime, nil
}

// tokenErr classifies a failed exchange. Always 502: the exchange is with the
// provider's issuer, so its failure is upstream's, never the caller's.
func (s *Step) tokenErr(err error) error {
	return model.NewCodedErr(http.StatusBadGateway, codeUpstreamUnavailable,
		fmt.Errorf("upstream: oauth2 token exchange failed: %w", err))
}

// tokenLifetime turns a token response's expires_in into how long we may hold
// that token, or reports that we may not hold it at all.
//
// The issuer owns this number, so it is read from the response and never from
// our config: a configured copy is a second version of the same fact, and it
// is wrong the moment a realm's token lifetime is retuned -- silently, with
// every request 401ing until someone edits a file.
//
// skew is subtracted so a request that passes the check cannot arrive at the
// provider after expiry. It has to exceed the round trip, and costs one extra
// refresh per token lifetime.
//
// Two answers rather than one duration, because "we were not told" is not a
// lifetime. A response with no expires_in is refused rather than cached for a
// guessed period or re-fetched on every single request.
func tokenLifetime(expiresIn int, skew time.Duration) (time.Duration, bool) {
	if expiresIn <= 0 {
		return 0, false
	}
	lifetime := time.Duration(expiresIn) * time.Second
	// A lifetime at or under the skew would put the expiry in the past, and
	// then every request fetches a fresh token. Half is still early enough to
	// refresh before the real expiry.
	if lifetime <= skew {
		return lifetime / 2, true
	}
	return lifetime - skew, true
}
