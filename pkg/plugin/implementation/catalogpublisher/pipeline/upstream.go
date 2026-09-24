// upstream.go is the HTTP client for the `upstream:` YAML block: token
// exchange, GET and POST data calls, error classification, and guards.
// All communication with the upstream service happens here.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// maxResponseBytes caps what is read from the upstream. The largest observed
// response is master data option 7 at about 400 KB; 32 MiB leaves room for
// growth while keeping an unbounded read impossible.
const maxResponseBytes = 32 << 20

// mapperRunner is the slice of jsonmapper this package uses.
//
// An interface rather than the concrete type so a test can substitute one,
// and so each call site states which of the mapper's abilities it depends
// on. Ported from tools/publish/mandi_publish/client.go's mapperRunner.
type Mapper interface {
	Transform(ctx context.Context, ref string, d definition.Direction, in any) ([]byte, error)
	Verify(ctx context.Context, ref string, in any) error
}

// pipelineClient talks to the upstream service.
//
// One struct for every call because they share a host, a token and a
// response-size ceiling, and nothing else about them differs enough to earn a
// type each. This is the package-private counterpart of the reference tool's
// Client.
type Client struct {
	baseURL    string
	http       *http.Client
	tokenPlace string // "query" or "header" — how the token rides on requests

	// errorRules are the provider's own `upstream.errors`, deciding what one
	// failed call MEANS. Empty means the engine falls back to status alone:
	// a 2xx is success and anything else is an outage, never a quiet result.
	errorRules []ErrorRule
}

// WithErrorRules gives the client the classification its pipeline declares.
//
// Without it the engine would have to recognise "this region has no rows" by
// some upstream's literal wording, which is what it used to do -- and why a
// second provider could not say the same thing in its own words.
func (c *Client) WithErrorRules(rules []ErrorRule) *Client {
	c.errorRules = rules
	return c
}

// newPipelineClient builds a pipelineClient with a timeout that suits the
// largest call: master data option 6 is roughly 600 KB and takes seconds,
// not milliseconds.
func NewClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 120 * time.Second}}
}

// token exchanges the credentials for a token.
//
// The credentials are marshalled rather than concatenated, so a secret
// carrying a quote or a backslash cannot break out of the JSON it travels
// in.
// Token performs the token exchange the pipeline file DECLARES, rather than
// one this package knows about.
//
// Everything that varies -- the method, the path, which JSON keys carry the
// credentials, and where the token sits in the response -- comes from
// `upstream.auth`. Two upstreams spell all four differently, and a second
// pipeline must not have to edit Go to authenticate.
//
// The credentials themselves are never in the file: the body's values are
// ${inputs.…} references, and the inputs that hold them are declared
// `secret: true`, so what the file carries is the NAME of a variable.
func (c *Client) Token(ctx context.Context, auth Auth, rc *runContext) (string, error) {
	if auth.Kind != "" && auth.Kind != authTokenExchange {
		return "", fmt.Errorf("upstream.auth.kind %q is not supported; this client performs %q",
			auth.Kind, authTokenExchange)
	}
	if auth.Request.Path == "" {
		return "", fmt.Errorf("upstream.auth.request.path is empty, so there is no token endpoint to call")
	}

	// Validate and store the token placement so Get/Post can apply it.
	// Defaults to "query" when unset, matching the existing behaviour.
	carried := strings.TrimSpace(auth.Token.CarriedAs)
	if carried == "" {
		carried = "query"
	}
	if carried != "query" && carried != "header" {
		return "", fmt.Errorf("upstream.auth.token.carriedAs %q is not supported; use %q or %q",
			auth.Token.CarriedAs, "query", "header")
	}
	c.tokenPlace = carried

	body := make(map[string]string, len(auth.Request.Body))
	for field, template := range auth.Request.Body {
		value, err := rc.interpolate(template)
		if err != nil {
			return "", fmt.Errorf("upstream.auth.request.body.%s: %w", field, err)
		}
		if value == "" {
			// An empty credential is sent as a real value and rejected far
			// from here, reading as bad credentials rather than absent ones.
			return "", fmt.Errorf("upstream.auth.request.body.%s resolved to empty", field)
		}
		body[field] = value
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("token request could not be built: %w", err)
	}

	method := auth.Request.Method
	if method == "" {
		method = http.MethodPost
	}
	path, err := rc.interpolate(auth.Request.Path)
	if err != nil {
		return "", fmt.Errorf("upstream.auth.request.path: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("token request could not be built: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Not %w: Go's transport errors quote the whole URL, and a token
		// endpoint's URL is the one place a credential could appear in it.
		return "", fmt.Errorf("token endpoint could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("token response could not be read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// THE STATUS, NEVER THE BODY. A rejection from this upstream quotes the
		// request back, credentials included, and this error is printed to a
		// terminal and pasted into tickets.
		return "", fmt.Errorf("token endpoint returned %s", resp.Status)
	}

	token, err := extractToken(raw, auth.Token.At)
	if err != nil {
		return "", err
	}
	return token, nil
}

// authTokenExchange is the one auth kind this client performs: POST
// credentials, receive a token, carry it on each later request.
const authTokenExchange = "tokenExchange"

// extractToken reads the token from the response at the declared location.
//
// The form is "$.field" or "$.a.b" -- a path from the response root. It is
// deliberately a path and not a full expression: this runs on a response that
// contains a live credential, and a path cannot do anything but select.
func extractToken(body []byte, at string) (string, error) {
	path := strings.TrimSpace(at)
	if path == "" {
		return "", fmt.Errorf("upstream.auth.token.at is empty, so the token cannot be located in the response")
	}
	if !strings.HasPrefix(path, "$.") {
		return "", fmt.Errorf("upstream.auth.token.at %q must start with \"$.\"", at)
	}

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		// Never the body.
		return "", fmt.Errorf("token response is not JSON")
	}

	value := decoded
	for _, field := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("token response has no %s (%q is not an object)", at, field)
		}
		value, ok = object[field]
		if !ok {
			return "", fmt.Errorf("token response has no %s", at)
		}
	}

	token, ok := value.(string)
	if !ok || token == "" {
		return "", fmt.Errorf("token response carries no token at %s", at)
	}
	return token, nil
}

// asQuery renders a mapped request object as a query string.
//
// Scalars only. An object or array has no single obvious encoding --
// repeated keys, comma-joined and indexed are all in use somewhere -- so
// choosing one here would put a convention in Go that belongs in the
// mapping. The error names the field so the fix is a one-line mapping edit.
func asQuery(mapped []byte) (string, error) {
	var fields map[string]any
	if err := json.Unmarshal(mapped, &fields); err != nil {
		return "", fmt.Errorf("mapped request is not an object: %w", err)
	}
	values := url.Values{}
	for name, value := range fields {
		switch typed := value.(type) {
		case string:
			values.Set(name, typed)
		case bool:
			values.Set(name, strconv.FormatBool(typed))
		case float64:
			// 'g' with -1 precision round-trips without inventing trailing
			// zeros, so 19.9975 stays 19.9975.
			values.Set(name, strconv.FormatFloat(typed, 'g', -1, 64))
		default:
			return "", fmt.Errorf("mapped field %q is not a scalar and cannot become a query parameter", name)
		}
	}
	return values.Encode(), nil
}

// fetchGet makes one GET, applying the token as a query param or header
// depending on c.tokenPlace, and returns the body of a 2xx.
func (c *Client) fetchGet(ctx context.Context, urlPath, query, token string) ([]byte, error) {
	endpoint := c.baseURL + urlPath
	if c.tokenPlace == "header" {
		if query != "" {
			endpoint += "?" + query
		}
	} else {
		// Default: token rides in the query string, built by the mapping.
		if query != "" {
			endpoint += "?" + query
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be built: %w", err)
	}
	if c.tokenPlace == "header" && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s could not be reached", urlPath)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("GET %s: response could not be read: %w", urlPath, err)
	}
	if err := classifiedError(classify(c.errorRules, resp.StatusCode, body),
		resp.StatusCode, "GET "+urlPath); err != nil {
		return nil, err
	}
	return body, nil
}

// fetchPost makes one POST with a JSON body and returns the body of a 2xx.
func (c *Client) fetchPost(ctx context.Context, urlPath string, bodyPayload []byte, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+urlPath, bytes.NewReader(bodyPayload))
	if err != nil {
		return nil, fmt.Errorf("POST %s request could not be built: %w", urlPath, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.tokenPlace == "header" && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if c.tokenPlace == "query" && token != "" {
		q := req.URL.Query()
		q.Set("token", token)
		req.URL.RawQuery = q.Encode()
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s could not be reached", urlPath)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("POST %s: response could not be read: %w", urlPath, err)
	}
	if err := classifiedError(classify(c.errorRules, resp.StatusCode, body),
		resp.StatusCode, "POST "+urlPath); err != nil {
		return nil, err
	}
	return body, nil
}

// httpGet verifies a mapping's preconditions, runs its request half to build
// a query, makes the GET, and runs the response half over what came back.
//
// The token reaches every half under _local, the same channel the adapter
// uses for values a payload does not carry -- so no mapping file holds a
// credential.
//
// VERIFY RUNS FIRST, and it is the reason a mapping's `required:` block is
// worth writing: without this call the guards would parse, compile, and
// never evaluate, so a malformed state code would reach the upstream and
// come back as an empty 200 that reads as "this state has no markets".
func (c *Client) Get(ctx context.Context, m Mapper, mappingRef, path string, local map[string]any) ([]byte, error) {
	input := map[string]any{"_local": local}
	if err := m.Verify(ctx, mappingRef, input); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	mapped, err := m.Transform(ctx, mappingRef, definition.DirectionRequest, input)
	if err != nil {
		return nil, fmt.Errorf("%s: request half: %w", path, err)
	}
	query, err := asQuery(mapped)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	token := ""
	if t, ok := local["token"].(string); ok {
		token = t
	}
	body, err := c.fetchGet(ctx, path, query, token)
	if err != nil {
		return nil, err
	}

	return c.runResponseMapping(ctx, m, mappingRef, path, body)
}

// Post builds a JSON body from the mapping's request half, POSTs it to path,
// and runs the response half over the answer.
//
// Situation: the upstream requires a POST to fetch or submit data
//
//	(e.g. search endpoints, filtering APIs, data submission).
//
// Scenario:  The mapping's request half shapes the body; the token is
//
//	applied as a header or query param per carriedAs.
func (c *Client) Post(ctx context.Context, m Mapper, mappingRef, path string, local map[string]any) ([]byte, error) {
	input := map[string]any{"_local": local}
	if err := m.Verify(ctx, mappingRef, input); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	bodyPayload, err := m.Transform(ctx, mappingRef, definition.DirectionRequest, input)
	if err != nil {
		return nil, fmt.Errorf("%s: request half: %w", path, err)
	}

	token := ""
	if t, ok := local["token"].(string); ok {
		token = t
	}
	rawBody, err := c.fetchPost(ctx, path, bodyPayload, token)
	if err != nil {
		return nil, err
	}

	return c.runResponseMapping(ctx, m, mappingRef, path, rawBody)
}

// runResponseMapping decodes a raw response body and runs the mapping's
// response half over it. Shared between Get and Post.
func (c *Client) runResponseMapping(ctx context.Context, m Mapper, mappingRef, path string, body []byte) ([]byte, error) {
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("%s: upstream answered with something that is not JSON: %w", path, err)
	}
	// MUST BE AN ARRAY. All calls this client makes answer with a JSON array
	// on success. An upstream error such as {"message":"no data found"}
	// decodes to an object, and JSONata's $map over an object yields one
	// element whose every field is undefined -- which Go then decodes as a
	// zero-valued struct (marketId: 0 and so on): a phantom row that would
	// flow into the output and become a published resource. Refuse here,
	// before the response half runs, rather than downstream where it looks
	// like real data. The body itself is never in this message: an upstream
	// error body can quote the request back, and the request carries the
	// token in its query string.
	if _, ok := answer.([]any); !ok {
		return nil, fmt.Errorf("%s: upstream answered with a JSON %s where an array was expected", path, jsonShape(answer))
	}
	out, err := m.Transform(ctx, mappingRef, definition.DirectionResponse, map[string]any{"response": answer})
	if err != nil {
		return nil, fmt.Errorf("%s: response half: %w", path, err)
	}
	return out, nil
}

// jsonShape names the JSON type of a value decoded by encoding/json, for an
// error message -- never the value itself, which for an upstream error body
// may carry the request (and its token) back.
func jsonShape(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// ErrNoUpstreamData reports that the upstream answered "no rows", not that
// the call went wrong.
//
// The service says this with an HTTP 400 and {"success":false,"message":"No
// data available."} -- measured 2026-09-11, when 27 of 36 states answered
// that way for the market-commodity mapping while every one of their markets
// was present in the master data. A malformed request looks different
// ({"error":"Option must be between 1 and 6"} for option=7), which is what
// makes this body safe to read as semantic rather than as a rejection.
// Recording it as a failure previously turned those 27 states into false
// outages, which is why callers must be able to tell "no data" apart from
// "broken" instead of collapsing both into one generic error.
var ErrNoUpstreamData = errors.New("upstream reports no data for this request")
