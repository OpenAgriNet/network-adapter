package pipeline

// auth.go is how a pipeline authenticates to its upstream, as a strategy the
// file picks with upstream.auth.kind rather than one flow this package knows.
//
// Three kinds, and deliberately no more until a pipeline needs one:
//
//	tokenExchange  POST credentials, read a token from the answer (default)
//	none           nothing to send
//	apiKey         a value from an input, placed per carriedAs/name
//
// Whatever the kind, ${auth.token} resolves to the credential's value, so a
// mapping that places the token itself does not care how it was obtained.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

const (
	authNone   = "none"
	authAPIKey = "apiKey"

	carriedAsQuery  = "query"
	carriedAsHeader = "header"
)

// Credential is what an Authenticator produced and how it rides on requests.
type Credential struct {
	Value     string // "" for kind none
	CarriedAs string // "header" | "query"
	Name      string // e.g. "Authorization", "token", "x-api-key"
	Prefix    string // e.g. "Bearer " or ""
}

// Authenticator obtains the credential a run carries.
type Authenticator interface {
	// RequiredInputs are the inputs that must resolve non-empty before the
	// run may call the upstream.
	RequiredInputs() []string
	// Prepare obtains the credential. It is called once per run, and again
	// on a reauth.
	Prepare(ctx context.Context, c *Client, rc *runContext) (Credential, error)
}

// authenticatorFor picks the strategy the file names.
func authenticatorFor(a Auth) (Authenticator, error) {
	switch strings.TrimSpace(a.Kind) {
	case "", authTokenExchange:
		return tokenExchange{auth: a}, nil
	case authNone:
		return noAuth{}, nil
	case authAPIKey:
		return apiKey{token: a.Token}, nil
	default:
		return nil, fmt.Errorf("upstream.auth.kind %q is not supported; use %q, %q or %q",
			a.Kind, authTokenExchange, authNone, authAPIKey)
	}
}

// authKind is the kind as the run reports it: unset reads as the default.
func authKind(a Auth) string {
	if k := strings.TrimSpace(a.Kind); k != "" {
		return k
	}
	return authTokenExchange
}

type tokenExchange struct{ auth Auth }

func (tokenExchange) RequiredInputs() []string { return []string{inputTokenUser, inputTokenSecret} }

func (t tokenExchange) Prepare(ctx context.Context, c *Client, rc *runContext) (Credential, error) {
	carried, name, err := placement(t.auth.Token, carriedAsQuery, "token")
	if err != nil {
		return Credential{}, err
	}
	token, err := c.exchangeToken(ctx, t.auth, rc)
	if err != nil {
		return Credential{}, err
	}
	cred := Credential{Value: token, CarriedAs: carried, Name: name}
	if carried == carriedAsHeader {
		// Today's header placement: a bearer token in Authorization,
		// whatever token.name says -- name is the QUERY parameter's name.
		cred.Name, cred.Prefix = "Authorization", "Bearer "
	}
	return cred, nil
}

type noAuth struct{}

func (noAuth) RequiredInputs() []string { return nil }

func (noAuth) Prepare(context.Context, *Client, *runContext) (Credential, error) {
	return Credential{}, nil
}

type apiKey struct{ token AuthTokenSpec }

func (apiKey) RequiredInputs() []string { return nil }

func (k apiKey) Prepare(_ context.Context, c *Client, rc *runContext) (Credential, error) {
	// Scheme first, like the exchange: the key is a credential too.
	if err := checkUpstreamScheme(c.baseURL, c.allowCleartext); err != nil {
		return Credential{}, err
	}
	if strings.TrimSpace(k.token.Value) == "" {
		return Credential{}, fmt.Errorf("upstream.auth.token.value is empty; an apiKey needs an ${inputs.…} reference")
	}
	value, err := rc.interpolate(k.token.Value)
	if err != nil {
		return Credential{}, fmt.Errorf("upstream.auth.token.value: %w", err)
	}
	if value == "" {
		return Credential{}, fmt.Errorf("upstream.auth.token.value resolved to empty")
	}
	carried, name, err := placement(k.token, "", "")
	if err != nil {
		return Credential{}, err
	}
	if carried == "" || name == "" {
		return Credential{}, fmt.Errorf("an apiKey needs upstream.auth.token.carriedAs and token.name")
	}
	return Credential{Value: value, CarriedAs: carried, Name: name}, nil
}

// placement validates carriedAs and name, applying defaults.
func placement(token AuthTokenSpec, defaultCarried, defaultName string) (string, string, error) {
	carried := strings.TrimSpace(token.CarriedAs)
	if carried == "" {
		carried = defaultCarried
	}
	if carried != "" && carried != carriedAsQuery && carried != carriedAsHeader {
		return "", "", fmt.Errorf("upstream.auth.token.carriedAs %q is not supported; use %q or %q",
			token.CarriedAs, carriedAsQuery, carriedAsHeader)
	}
	name := strings.TrimSpace(token.Name)
	if name == "" {
		name = defaultName
	}
	return carried, name, nil
}

// WithCredential sets the credential every later request carries.
func (c *Client) WithCredential(cred Credential) *Client {
	c.credential = cred
	return c
}

// applyCredential places the credential on one request.
//
// Header credentials go on every request. A query credential goes on POST
// only: on GET the MAPPING builds the whole query and places the token under
// the file's own name (today's behaviour), and adding it here too would send
// it twice.
func (c *Client) applyCredential(req *http.Request) {
	cred := c.credential
	if cred.Value == "" {
		return
	}
	switch cred.CarriedAs {
	case carriedAsHeader:
		req.Header.Set(cred.Name, cred.Prefix+cred.Value)
	case carriedAsQuery:
		if req.Method == http.MethodGet {
			return
		}
		q := req.URL.Query()
		q.Set(cred.Name, cred.Value)
		req.URL.RawQuery = q.Encode()
	}
}
