// upstream.go is the HTTP client for the `upstream:` YAML block: token
// exchange, GET and POST data calls, and error classification.
// All communication with the upstream service happens here.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// maxResponseBytes caps what is read from the upstream. The largest observed
// response is master data option 7 at about 400 KB; 32 MiB leaves room for
// growth while keeping an unbounded read impossible.
const maxResponseBytes = 32 << 20

// Mapper is the slice of jsonmapper this package uses.
//
// An interface rather than the concrete type so a test can substitute one,
// and so each call site states which of the mapper's abilities it depends
// on.
type Mapper interface {
	Transform(ctx context.Context, ref string, d definition.Direction, in any) ([]byte, error)
	Verify(ctx context.Context, ref string, in any) error
}

// Client talks to the upstream service.
//
// One struct for every call because they share a host, a token and a
// response-size ceiling, and nothing else about them differs enough to earn a
// type each.
type Client struct {
	baseURL    string
	http       *http.Client
	credential Credential // what every request carries; see applyCredential
	log        *slog.Logger

	// allowCleartext carries the file's deliberate acceptance of an http
	// upstream. See checkUpstreamScheme.
	allowCleartext bool

	// policyOnce applies the redirect policy to whatever http.Client this
	// one ends up holding, including one a caller substituted.
	policyOnce sync.Once

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

// WithCleartextAllowed carries the file's deliberate acceptance of an http
// upstream into the client.
func (c *Client) WithCleartextAllowed(allowed bool) *Client {
	c.allowCleartext = allowed
	return c
}

// NewClient builds a Client with a timeout that suits the largest call:
// master data option 6 is roughly 600 KB and takes seconds, not
// milliseconds.
func NewClient(baseURL string) *Client {
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: 120 * time.Second}}
}

// WithLogger sets where each call's line goes. nil means slog.Default().
func (c *Client) WithLogger(log *slog.Logger) *Client {
	c.log = log
	return c
}

func (c *Client) logger() *slog.Logger {
	if c.log == nil {
		return slog.Default()
	}
	return c.log
}

type logItemKey struct{}

// withLogItem tags a context with the forEach item a call is made for, so the
// call's log line says which state stalled.
func withLogItem(ctx context.Context, item string) context.Context {
	return context.WithValue(ctx, logItemKey{}, item)
}

// do is the one place a request leaves this package, so the redirect policy,
// the credential and the call's log line cannot be forgotten at a call site.
// It reads (bounded) and closes the body.
//
// carryCredential is false only for the token exchange, which carries the
// credentials in its body and must not carry the token it is replacing.
//
// A nil response means the call never completed; a response with an error
// means it was answered but the body could not be read.
//
// The log line carries the path WITHOUT its query: for this class of
// upstream the query is where the token rides.
func (c *Client) do(req *http.Request, carryCredential bool) (*http.Response, []byte, error) {
	c.policyOnce.Do(func() {
		if c.http.CheckRedirect == nil {
			c.http.CheckRedirect = util.RefuseOffHostRedirect
		}
	})
	if carryCredential {
		c.applyCredential(req)
	}

	began := time.Now()
	resp, err := c.http.Do(req)
	fields := []any{"method", req.Method, "path", req.URL.Path}
	if item, ok := req.Context().Value(logItemKey{}).(string); ok && item != "" {
		fields = append(fields, "item", item)
	}
	if err != nil {
		c.logger().InfoContext(req.Context(), "upstream call", append(fields,
			"status", 0, "duration", time.Since(began).Round(time.Millisecond).String(), "bytes", 0)...)
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	c.logger().InfoContext(req.Context(), "upstream call", append(fields,
		"status", resp.StatusCode, "duration", time.Since(began).Round(time.Millisecond).String(),
		"bytes", len(body))...)
	return resp, body, readErr
}

// ErrRedirectRefused is the reason a call failed when the upstream redirected
// somewhere this client will not carry a credential. The guard is shared with
// the domain plugins' client (util.RefuseOffHostRedirect), and so is this.
var ErrRedirectRefused = util.ErrRedirectRefused

// unreachable is the error a failed round trip becomes.
//
// Go's transport errors quote the whole URL, and for this class of upstream
// the URL is where the token rides -- so a transport failure is reported
// WITHOUT its cause. A refused redirect is our own error and names only a
// scheme and a host, so it is passed through.
func unreachable(call string, err error) error {
	// Taken out of the *url.Error rather than wrapped through it: that
	// wrapper's Error() prints the URL, token and all.
	var wrapped *url.Error
	if errors.As(err, &wrapped) && errors.Is(wrapped.Err, ErrRedirectRefused) {
		return fmt.Errorf("%s: %w", call, wrapped.Err)
	}
	if errors.Is(err, ErrRedirectRefused) {
		return fmt.Errorf("%s: %w", call, ErrRedirectRefused)
	}
	// Neither carries the URL -- "context canceled"/"context deadline
	// exceeded" are fixed, credential-free strings -- so both are safe to
	// keep classifiable with %w. Losing them here is how a shutdown or a
	// timeout used to read as an ordinary upstream failure: it burned a
	// sweep's attempt budget on its own way out, and could give up on a
	// firing that never got a real try.
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", call, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", call, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s could not be reached", call)
}

// exchangeToken performs the token exchange the pipeline file DECLARES,
// rather than one this package knows about. It is the tokenExchange
// Authenticator's round trip (see auth.go).
//
// The credentials are marshalled rather than concatenated, so a secret
// carrying a quote or a backslash cannot break out of the JSON it travels
// in.
//
// Everything that varies -- the method, the path, which JSON keys carry the
// credentials, and where the token sits in the response -- comes from
// `upstream.auth`. Two upstreams spell all four differently, and a second
// pipeline must not have to edit Go to authenticate.
//
// The credentials themselves are never in the file: the body's values are
// ${inputs.…} references, and the inputs that hold them are declared
// `secret: true`, so what the file carries is the NAME of a variable.
func (c *Client) exchangeToken(ctx context.Context, auth Auth, rc *runContext) (string, error) {
	// Checked HERE rather than at the first data call, because this is the
	// request that carries the credentials themselves.
	if err := checkUpstreamScheme(c.baseURL, c.allowCleartext); err != nil {
		return "", err
	}

	if auth.Request.Path == "" {
		return "", fmt.Errorf("upstream.auth.request.path is empty, so there is no token endpoint to call")
	}

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
	if err := checkCallPath("token endpoint", path); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("token request could not be built: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, raw, err := c.do(req, false)
	if resp == nil {
		// Not %w: Go's transport errors quote the whole URL, and a token
		// endpoint's URL is the one place a credential could appear in it.
		return "", unreachable("token endpoint", err)
	}
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

// authTokenExchange is the default auth kind: POST credentials, receive a
// token, carry it on each later request.
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

// checkCallPath refuses a path that could change the host it is appended to.
// The client builds every request as baseURL + path, and the credential rides
// on it: "@evil.example/x" after https://host is a request to evil.example.
// A path starting with "/" stays on the host the deployment configured.
func checkCallPath(call, path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%s: path %q must start with \"/\"; anything else appended to the upstream "+
			"address can change the host the credential is sent to", call, path)
	}
	return nil
}

// fetchGet makes one GET, applying the client's credential (see
// applyCredential), and returns the body of a 2xx.
func (c *Client) fetchGet(ctx context.Context, urlPath, query string) ([]byte, error) {
	if err := checkCallPath("GET", urlPath); err != nil {
		return nil, err
	}
	// The query is the mapping's, whole: on the query-carried path the
	// mapping is what puts the token in it, under the file's own name.
	endpoint := c.baseURL + urlPath
	if query != "" {
		endpoint += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be built: %w", err)
	}

	resp, body, err := c.do(req, true)
	if resp == nil {
		return nil, unreachable("GET "+urlPath, err)
	}
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
func (c *Client) fetchPost(ctx context.Context, urlPath string, bodyPayload []byte) ([]byte, error) {
	if err := checkCallPath("POST", urlPath); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+urlPath, bytes.NewReader(bodyPayload))
	if err != nil {
		return nil, fmt.Errorf("POST %s request could not be built: %w", urlPath, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, body, err := c.do(req, true)
	if resp == nil {
		return nil, unreachable("POST "+urlPath, err)
	}
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

	body, err := c.fetchGet(ctx, path, query)
	if err != nil {
		return nil, err
	}

	return c.runResponseMapping(ctx, m, mappingRef, path, body, local)
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

	rawBody, err := c.fetchPost(ctx, path, bodyPayload)
	if err != nil {
		return nil, err
	}

	return c.runResponseMapping(ctx, m, mappingRef, path, rawBody, local)
}

// runResponseMapping decodes a raw response body and runs the mapping's
// response half over it. Shared between Get and Post.
//
// The response half sees the request's _local too, less the credential (see
// responseLocal): a per-item call often answers without the item's own codes
// -- the price call reports a market's name, not its code -- so the mapping
// needs the item to say which record the answer belongs to.
func (c *Client) runResponseMapping(ctx context.Context, m Mapper, mappingRef, path string, body []byte, local map[string]any) ([]byte, error) {
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
	out, err := m.Transform(ctx, mappingRef, definition.DirectionResponse, map[string]any{
		"response": answer,
		"_local":   c.responseLocal(local),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: response half: %w", path, err)
	}
	return out, nil
}

// responseLocal is local without any value that carries the credential.
//
// A response mapping's output becomes records, and records become published
// catalogs, so the token must not be within its reach -- not as `token`, and
// not inside another value such as "Bearer <token>". Matched on the value,
// not the key, because the key is whatever the pipeline file named it.
func (c *Client) responseLocal(local map[string]any) map[string]any {
	out := make(map[string]any, len(local))
	secret := c.credential.Value
	for key, value := range local {
		if text, isText := value.(string); isText && secret != "" && strings.Contains(text, secret) {
			continue
		}
		out[key] = value
	}
	return out
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

// checkUpstreamScheme refuses to send credentials in cleartext.
//
// A pipeline once defaulted its upstream to a plain-HTTP address at a bare IP,
// so a deployment that did not set the env var POSTed its credentials
// unencrypted and then carried the token in every query string after that.
// Removing that default fixes one file; this fixes any file.
//
// Loopback is exempt: a developer's fake upstream and this package's own tests
// run on http://127.0.0.1, and refusing those would make the rule something
// people work around rather than something that protects them.
func checkUpstreamScheme(baseURL string, allowCleartext bool) error {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return fmt.Errorf("upstream address could not be parsed")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && (isLoopback(parsed.Hostname()) || allowCleartext) {
		return nil
	}
	if parsed.Scheme == "" {
		return fmt.Errorf("the upstream address names no scheme; it must be https " +
			"(the credential exchange and every token afterwards travel over it)")
	}
	return fmt.Errorf("the upstream address uses %s; credentials and the token it returns would "+
		"travel in cleartext. Use https, or -- if this upstream offers no TLS -- say so "+
		"deliberately with `upstream.allowCleartext: true`, which is recorded in the pipeline "+
		"file and warned about on every run", parsed.Scheme)
}

// isLoopback reports whether a host is this machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
