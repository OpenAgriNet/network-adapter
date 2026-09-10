package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// maxResponseBytes caps what is read from the upstream. The largest observed
// response is master data option 7 at about 400 KB; 32 MiB leaves room for
// growth while keeping an unbounded read impossible.
const maxResponseBytes = 32 << 20

// Client talks to Agmarknet Vistaar.
//
// One struct for all four calls because they share a host, a token and a
// response-size ceiling, and nothing else about them differs enough to earn a
// type each.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// newClient builds a Client with a timeout that suits the largest call: master
// data option 6 is roughly 600 KB and takes seconds, not milliseconds.
func newClient(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// tokenResponse is the whole of what this tool reads from the token endpoint.
//
// There is no expiry field to read -- the endpoint states none -- which is why
// the adapter's tokenQuery scheme makes an operator declare tokenTtl rather
// than inventing one. A single run holds one token.
type tokenResponse struct {
	Token string `json:"token"`
}

// Token exchanges the credentials for a token.
//
// The credentials are marshalled rather than concatenated, so a secret carrying
// a quote or a backslash cannot break out of the JSON it travels in.
func (c *Client) Token(ctx context.Context, user, secret string) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"access_name": user,
		"password":    secret,
	})
	if err != nil {
		return "", fmt.Errorf("token request could not be built: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/generate-dynamic-token-agmarknet", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("token request could not be built: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint could not be reached: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("token response could not be read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// THE STATUS, NEVER THE BODY. A rejection from this upstream quotes the
		// request back, credentials included, and this error is printed to a
		// terminal and pasted into tickets.
		return "", fmt.Errorf("token endpoint returned %s", resp.Status)
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("token response is not JSON: %w", err)
	}
	if parsed.Token == "" {
		return "", fmt.Errorf("token response carries no token")
	}
	return parsed.Token, nil
}

// mapperRunner is the slice of jsonmapper this tool uses.
//
// An interface rather than the concrete type so a test can substitute one, and
// so each call site states which of the mapper's abilities it depends on.
type mapperRunner interface {
	Transform(ctx context.Context, ref string, d definition.Direction, in any) ([]byte, error)
	Verify(ctx context.Context, ref string, in any) error
}

// State is one row of master data option 4, and the loop driver for the whole
// tool: the market-commodity mapping is fetched per state code.
type State struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// asQuery renders a mapped request object as a query string.
//
// Scalars only. An object or array has no single obvious encoding -- repeated
// keys, comma-joined and indexed are all in use somewhere -- so choosing one
// here would put a convention in Go that belongs in the mapping. The error
// names the field so the fix is a one-line mapping edit.
//
// This is a small reimplementation rather than a reuse: the adapter's version
// lives under pkg/plugin/implementation/internal/, which Go's internal rule
// puts out of reach of tools/.
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

// get makes one GET and returns the body of a 2xx.
func (c *Client) get(ctx context.Context, path, query string) ([]byte, error) {
	endpoint := c.BaseURL + path
	if query != "" {
		endpoint += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be built: %w", err)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL carries the token, and Go's transport errors quote the whole
		// URL, so the message is rebuilt from the path rather than wrapped.
		return nil, fmt.Errorf("GET %s could not be reached", path)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("GET %s: response could not be read: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned %s", path, resp.Status)
	}
	return body, nil
}

// call verifies a mapping's preconditions, runs its request half to build a
// query, makes the GET, and runs the response half over what came back.
//
// The token reaches every half under _local, the same channel the adapter uses
// for values a payload does not carry -- so no mapping file holds a credential.
//
// VERIFY RUNS FIRST, and it is the reason a mapping's `required:` block is worth
// writing: without this call the guards would parse, compile, and never
// evaluate, so a malformed state code would reach the upstream and come back as
// an empty 200 that reads as "this state has no markets".
func (c *Client) call(ctx context.Context, m mapperRunner, ref, path string, local map[string]any) ([]byte, error) {
	input := map[string]any{"_local": local}
	if err := m.Verify(ctx, ref, input); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	mapped, err := m.Transform(ctx, ref, definition.DirectionRequest, input)
	if err != nil {
		return nil, fmt.Errorf("%s: request half: %w", path, err)
	}
	query, err := asQuery(mapped)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	body, err := c.get(ctx, path, query)
	if err != nil {
		return nil, err
	}

	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("%s: upstream answered with something that is not JSON: %w", path, err)
	}
	// MUST BE AN ARRAY. All three calls this client makes answer with a JSON
	// array on success. An upstream error such as {"message":"no data found"}
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
	out, err := m.Transform(ctx, ref, definition.DirectionResponse, map[string]any{"response": answer})
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

// States fetches master data option 4.
func (c *Client) States(ctx context.Context, m mapperRunner, mappingBase, token string) ([]State, error) {
	out, err := c.call(ctx, m, mappingBase+"/master-states.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": token})
	if err != nil {
		return nil, err
	}
	var states []State
	if err := json.Unmarshal(out, &states); err != nil {
		return nil, fmt.Errorf("state list could not be read: %w", err)
	}
	return states, nil
}
