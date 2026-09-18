package router

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"

	"gopkg.in/yaml.v3"
)

// Config holds the configuration for the Router plugin.
type Config struct {
	RoutingConfig string `json:"routingConfig"`
}

// RoutingConfig represents the structure of the routing configuration file.
type routingConfig struct {
	RoutingRules []routingRule `yaml:"routingRules"`
}

// Router implements Router interface.
type Router struct {
	rules map[string]map[string]map[string]*model.Route // domain -> version -> endpoint -> route
}

// RoutingRule represents a single routing rule.
type routingRule struct {
	Domain     string   `yaml:"domain"`
	Version    string   `yaml:"version"`
	TargetType string   `yaml:"targetType"` // "url", "urls" (fan-out), "publisher", "bpp"/"receiver", or "bap"/"sender"
	Target     target   `yaml:"target,omitempty"`
	Endpoints  []string `yaml:"endpoints"`
	// MergeFieldPath is the full dot path, from the envelope root, to the
	// array a fan-out merges across targets -- "message.catalogs" for
	// discover, "message.contract.commitments" for select. Required for
	// targetType "urls", meaningless for anything else.
	MergeFieldPath string `yaml:"mergeFieldPath,omitempty"`
}

// Target contains destination-specific details.
type target struct {
	URL           string   `yaml:"url,omitempty"`           // URL for "url" or gateway endpoint for "bpp"/"bap"
	URLs          []string `yaml:"urls,omitempty"`          // Targets for "urls" (fan-out)
	PublisherID   string   `yaml:"publisherId,omitempty"`   // For "msgq" type
	ExcludeAction bool     `yaml:"excludeAction,omitempty"` // For "url"/"urls" to exclude appending action to URL path
}

// TargetType defines possible target destinations.
// The legacy values "bpp" and "bap" are retained for backward compatibility;
// new configs should use "receiver" and "sender" respectively.
const (
	targetTypeURL       = "url"       // Route to a single URL
	targetTypeURLs      = "urls"      // Fan-out: call every URL, merge the replies
	targetTypePublisher = "publisher" // Route to a publisher
	targetTypeBPP       = "bpp"       // Route to a BPP endpoint (legacy; prefer "receiver")
	targetTypeBAP       = "bap"       // Route to a BAP endpoint (legacy; prefer "sender")
	targetTypeReceiver  = "receiver"  // Route to receiver (BPP) endpoint — Beckn spec v2 name
	targetTypeSender    = "sender"    // Route to sender (BAP) endpoint — Beckn spec v2 name
)

// New initializes a new Router instance with the provided configuration.
// It loads and validates the routing rules from the specified YAML file.
// Returns an error if the configuration is invalid or the rules cannot be loaded.
func New(ctx context.Context, config *Config) (*Router, func() error, error) {
	// Check if config is nil
	if config == nil {
		return nil, nil, fmt.Errorf("config cannot be nil")
	}
	router := &Router{
		rules: make(map[string]map[string]map[string]*model.Route),
	}

	// Load rules at bootup
	if err := router.loadRules(config.RoutingConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to load routing rules: %w", err)
	}
	return router, nil, nil
}

// LoadRules reads and parses routing rules from the YAML configuration file.
func (r *Router) loadRules(configPath string) error {
	if configPath == "" {
		return fmt.Errorf("routingConfig path is empty")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("error reading config file at %s: %w", configPath, err)
	}
	var config routingConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("error parsing YAML: %w", err)
	}

	// Validate rules
	if err := validateRules(config.RoutingRules); err != nil {
		return fmt.Errorf("invalid routing rules: %w", err)
	}
	// Build the optimized rule map
	for _, rule := range config.RoutingRules {
		// For v2.x.x, warn if domain is provided and normalize to wildcard "*"
		domain := rule.Domain
		if isV2Version(rule.Version) {
			if domain != "" {
				fmt.Printf("WARNING: Domain field '%s' is not needed for version %s and will be ignored. Consider removing it from your config.\n", domain, rule.Version)
			}
			domain = "*"
		}

		// Initialize domain map if not exists
		if _, ok := r.rules[domain]; !ok {
			r.rules[domain] = make(map[string]map[string]*model.Route)
		}

		// Initialize version map if not exists
		if _, ok := r.rules[domain][rule.Version]; !ok {
			r.rules[domain][rule.Version] = make(map[string]*model.Route)
		}

		// Add all endpoints for this rule
		for _, endpoint := range rule.Endpoints {
			var route *model.Route
			switch rule.TargetType {
			case targetTypePublisher:
				route = &model.Route{
					TargetType:  rule.TargetType,
					PublisherID: rule.Target.PublisherID,
				}
			case targetTypeURL:
				parsedURL, err := url.Parse(rule.Target.URL)
				if err != nil {
					return fmt.Errorf("invalid URL in rule: %w", err)
				}
				if !rule.Target.ExcludeAction {
					parsedURL.Path = joinPath(parsedURL, endpoint)
				}
				route = &model.Route{
					TargetType: rule.TargetType,
					URL:        parsedURL,
				}
			case targetTypeURLs:
				parsed := make([]*url.URL, 0, len(rule.Target.URLs))
				for _, t := range rule.Target.URLs {
					parsedURL, err := url.Parse(t)
					if err != nil {
						return fmt.Errorf("invalid URL in rule: %w", err)
					}
					if !rule.Target.ExcludeAction {
						parsedURL.Path = joinPath(parsedURL, endpoint)
					}
					parsed = append(parsed, parsedURL)
				}
				route = &model.Route{
					TargetType:     rule.TargetType,
					URLs:           parsed,
					MergeFieldPath: rule.MergeFieldPath,
				}
			case targetTypeBPP, targetTypeBAP, targetTypeReceiver, targetTypeSender:
				var parsedURL *url.URL
				if rule.Target.URL != "" {
					parsedURL, err = url.Parse(rule.Target.URL)
					if err != nil {
						return fmt.Errorf("invalid URL in rule: %w", err)
					}
					parsedURL.Path = joinPath(parsedURL, endpoint)
				}
				route = &model.Route{
					TargetType: rule.TargetType,
					URL:        parsedURL,
				}
			}
			// Check for conflicting v2 rules
			if isV2Version(rule.Version) {
				if _, exists := r.rules[domain][rule.Version][endpoint]; exists {
					return fmt.Errorf("duplicate endpoint '%s' found for version %s. For v2.x.x, domain is ignored, so you can only define each endpoint once per version. Please remove the duplicate rule", endpoint, rule.Version)
				}
			}
			r.rules[domain][rule.Version][endpoint] = route
		}
	}

	return nil
}

// validateRules performs basic validation on the loaded routing rules.
func validateRules(rules []routingRule) error {
	for _, rule := range rules {
		// Ensure version and TargetType are present
		if rule.Version == "" || rule.TargetType == "" {
			return fmt.Errorf("invalid rule: version and targetType are required")
		}

		// Domain is required only for v1.x.x
		if !isV2Version(rule.Version) && rule.Domain == "" {
			return fmt.Errorf("invalid rule: domain is required for version %s", rule.Version)
		}

		// Validate based on TargetType
		switch rule.TargetType {
		case targetTypeURL:
			if rule.Target.URL == "" {
				return fmt.Errorf("invalid rule: target.url is required for targetType 'url'")
			}
			if _, err := url.Parse(rule.Target.URL); err != nil {
				return fmt.Errorf("invalid URL - %s: %w", rule.Target.URL, err)
			}
			if len(rule.Target.URLs) > 0 {
				return fmt.Errorf("invalid rule: target.urls is not supported for targetType 'url' -- use targetType 'urls' for fan-out")
			}
			if rule.MergeFieldPath != "" {
				return fmt.Errorf("invalid rule: mergeFieldPath is only meaningful for targetType 'urls'")
			}
		case targetTypeURLs:
			if rule.Target.URL != "" {
				return fmt.Errorf("invalid rule: target.url is not supported for targetType 'urls' -- use target.urls")
			}
			if len(rule.Target.URLs) < 2 {
				return fmt.Errorf("invalid rule: targetType 'urls' requires at least 2 targets -- use targetType 'url' for a single target")
			}
			for _, t := range rule.Target.URLs {
				if strings.TrimSpace(t) == "" {
					return fmt.Errorf("invalid rule: urls holds an empty entry for targetType 'urls'")
				}
				if _, err := url.Parse(t); err != nil {
					return fmt.Errorf("invalid URL - %s: %w", t, err)
				}
			}
			// Fan-out merges one array from every target's reply -- the
			// router serves every action, so it cannot guess where that
			// array lives.
			if rule.MergeFieldPath == "" {
				return fmt.Errorf("invalid rule: mergeFieldPath is required for targetType 'urls' -- e.g. mergeFieldPath: message.catalogs, or mergeFieldPath: message.contract.commitments for select")
			}
			// The shape a legal path must have (rooted at "message") is
			// Beckn-envelope knowledge this router doesn't otherwise
			// need -- model.ValidMergeFieldPath is the shared answer, so
			// this plugin isn't the one asserting what a reply looks
			// like. A path failing it is a typo that would otherwise
			// fail silently at request time -- every target looking like
			// it "carries nothing" and the caller getting a confusing
			// NACK instead of a clear load-time error.
			if !model.ValidMergeFieldPath(rule.MergeFieldPath) {
				return fmt.Errorf("invalid rule: mergeFieldPath %q must start with \"message.\" -- it names a path under the response's message, not the envelope root", rule.MergeFieldPath)
			}
		case targetTypePublisher:
			if rule.Target.PublisherID == "" {
				return fmt.Errorf("invalid rule: publisherID is required for targetType 'publisher'")
			}
		case targetTypeBPP, targetTypeBAP, targetTypeReceiver, targetTypeSender:
			if rule.Target.URL != "" {
				if _, err := url.Parse(rule.Target.URL); err != nil {
					return fmt.Errorf("invalid URL - %s defined in routing config for target type %s: %w", rule.Target.URL, rule.TargetType, err)
				}
			}
			// Fan-out (target.urls, several destinations merged into one
			// reply) only exists for targetType 'urls' -- loadRules' branch
			// for this type never reads it, so a rule setting it here would
			// parse clean and silently route to nothing instead of erroring.
			if len(rule.Target.URLs) > 0 {
				return fmt.Errorf("invalid rule: target.urls is not supported for targetType %q -- fan-out is only available for targetType 'urls'", rule.TargetType)
			}
			continue
		default:
			return fmt.Errorf("invalid rule: unknown targetType '%s'", rule.TargetType)
		}
	}
	return nil
}

// getContextString returns the value for a context field, checking each key in
// order and returning the first non-empty string found. Supports snake_case
// (legacy), camelCase (Beckn spec v1 preferred), and the new Beckn spec v2
// camelCase names (e.g. senderUri, receiverUri) transparently.
func getContextString(ctx map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := ctx[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// Route determines the routing destination based on the request context.
// reqURL.Path holds the Beckn endpoint action already stripped of the module
// base path by the step layer (e.g. "search" or "catalog/subscription").
// reqURL.RawQuery is forwarded verbatim to the upstream target URL.
func (r *Router) Route(ctx context.Context, reqURL *url.URL, body []byte) (*model.Route, error) {
	if reqURL == nil {
		return nil, fmt.Errorf("reqURL must not be nil")
	}
	endpoint := reqURL.Path
	rawQuery := reqURL.RawQuery
	// Bodyless requests (GET/DELETE) carry no JSON body; domain, version, and
	// BAP/BPP URIs are not available. Route using the v2 config version.
	if len(body) == 0 {
		return r.routeBodyless(endpoint, rawQuery)
	}

	// Decode the body and extract its context field using the same
	// classification reqmapper (#867) and reqpreprocessor (#868) rely on for
	// the identical check, instead of router's own separate typed-struct and
	// map decodes of the same body.
	_, reqContext, becknErr := model.ExtractContext(body)
	if becknErr != nil {
		return nil, model.WrapExtractContextErr("error parsing request body", becknErr)
	}

	// Checks legacy snake_case (bpp_uri, bap_uri), then camelCase (bppUri,
	// bapUri), then the new Beckn spec v2 names (receiverUri, senderUri).
	bppURI := getContextString(reqContext, "bpp_uri", "bppUri", "receiverUri")
	bapURI := getContextString(reqContext, "bap_uri", "bapUri", "senderUri")
	version := getContextString(reqContext, "version")

	// For v2.x.x, ignore domain and use wildcard; for v1.x.x, use actual domain
	domain := getContextString(reqContext, "domain")
	if isV2Version(version) {
		domain = "*"
	}

	// Lookup route in the optimized map
	domainRules, ok := r.rules[domain]
	if !ok {
		if domain == "*" {
			return nil, fmt.Errorf("no routing rules found for version %s", version)
		}
		return nil, fmt.Errorf("no routing rules found for domain %s", domain)
	}

	versionRules, ok := domainRules[version]
	if !ok {
		if domain == "*" {
			return nil, fmt.Errorf("no routing rules found for version %s", version)
		}
		return nil, fmt.Errorf("no routing rules found for domain %s version %s", domain, version)
	}

	route, ok := versionRules[endpoint]
	if !ok {
		if domain == "*" {
			return nil, fmt.Errorf("endpoint '%s' is not supported for version %s in routing config", endpoint, version)
		}
		return nil, fmt.Errorf("endpoint '%s' is not supported for domain %s and version %s in routing config",
			endpoint, domain, version)
	}
	// Handle BPP/BAP routing with request URIs.
	// Both legacy ("bpp"/"bap") and new spec v2 ("receiver"/"sender") values are accepted.
	switch route.TargetType {
	case targetTypeBPP, targetTypeReceiver:
		return handleProtocolMapping(route, bppURI, endpoint, rawQuery)
	case targetTypeBAP, targetTypeSender:
		return handleProtocolMapping(route, bapURI, endpoint, rawQuery)
	case targetTypeURL:
		// Copy inbound query params onto a URL clone so the upstream receives them.
		// The baked-in URL has no RawQuery of its own.
		if rawQuery != "" && route.URL != nil {
			return withRawQuery(route, rawQuery), nil
		}
	case targetTypeURLs:
		if rawQuery != "" && len(route.URLs) > 0 {
			return withRawQuery(route, rawQuery), nil
		}
	}
	return route, nil
}

// routeBodyless handles routing for GET/DELETE requests that carry no body.
// Since domain and version cannot be read from a missing payload, the version
// registered in the v2 wildcard config ("*") is used for the rules lookup.
//
// Supported target types:
//   - url:       routed to the configured static URL (primary use case).
//   - urls:      fanned out to every configured URL, same as with a body.
//   - publisher: technically permitted — the empty-body message is forwarded
//     to the queue as-is. No known protocol use case exists for routing
//     catalog GET/DELETE requests through a message queue; this is allowed
//     rather than rejected to avoid unnecessary constraint, but operators
//     should not configure publisher targets for bodyless endpoints.
//   - bpp / bap / receiver / sender: rejected — the target URI is read from the request body,
//     which is absent for bodyless requests.
func (r *Router) routeBodyless(endpoint, rawQuery string) (*model.Route, error) {
	v2Rules, ok := r.rules["*"]
	if !ok {
		return nil, fmt.Errorf("no v2 routing rules found; bodyless requests require a v2 config")
	}

	for version, versionRules := range v2Rules {
		route, ok := versionRules[endpoint]
		if !ok {
			continue
		}
		switch route.TargetType {
		case targetTypeBPP, targetTypeBAP, targetTypeReceiver, targetTypeSender:
			return nil, fmt.Errorf("bodyless endpoint '%s' (version %s) is configured with target type '%s': dynamic BAP/BPP URI routing is not supported for bodyless requests", endpoint, version, route.TargetType)
		}
		// Publisher routes address a queue by ID — they carry no URL, so
		// RawQuery does not apply. Only clone for URL-type targets.
		if rawQuery != "" {
			switch {
			case route.TargetType == targetTypeURL && route.URL != nil:
				return withRawQuery(route, rawQuery), nil
			case route.TargetType == targetTypeURLs && len(route.URLs) > 0:
				return withRawQuery(route, rawQuery), nil
			}
		}
		return route, nil
	}

	return nil, fmt.Errorf("endpoint '%s' is not supported in v2 routing config", endpoint)
}

// canonicalRoleName returns a stable, human-readable role label for use in error
// messages regardless of which targetType alias ("bpp"/"receiver", "bap"/"sender")
// was used in the routing config.
func canonicalRoleName(targetType string) string {
	switch targetType {
	case targetTypeBPP, targetTypeReceiver:
		return "BPP"
	case targetTypeBAP, targetTypeSender:
		return "BAP"
	default:
		return strings.ToUpper(targetType)
	}
}

// handleProtocolMapping handles both BPP and BAP routing with proper URL construction.
// rawQuery is the inbound request's query string and is forwarded verbatim to the upstream URL.
func handleProtocolMapping(route *model.Route, npURI, endpoint, rawQuery string) (*model.Route, error) {
	target := strings.TrimSpace(npURI)
	role := canonicalRoleName(route.TargetType)
	if len(target) == 0 {
		if route.URL == nil {
			return nil, fmt.Errorf("could not determine destination for endpoint '%s': neither request contained a %s URI nor was a default URL configured in routing rules", endpoint, role)
		}
		if rawQuery != "" {
			fallback := *route.URL
			fallback.RawQuery = rawQuery
			return &model.Route{TargetType: targetTypeURL, URL: &fallback}, nil
		}
		return &model.Route{TargetType: targetTypeURL, URL: route.URL}, nil
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, model.NewBadReqErr("SCH_INVALID_FORMAT",
			fmt.Errorf("invalid %s URI - %s in request body for %s: %w", role, target, endpoint, err))
	}
	targetURL.Path = joinPath(targetURL, endpoint)
	if rawQuery != "" {
		targetURL.RawQuery = rawQuery
	}
	return &model.Route{TargetType: targetTypeURL, URL: targetURL}, nil
}

// withRawQuery returns a copy of a "url" or "urls" route with the inbound
// query string applied to every target.
//
// Built from Clone(), not a fresh &model.Route{} literal: every call site
// already guards on TargetType before calling this, so route.TargetType is
// safe to carry through unchanged, and Clone means a field this function
// doesn't know to touch (MergeFieldPath, at the time this bug was found) is
// preserved rather than silently dropped -- the same class of bug
// Route.Clone exists to close off in addRouteStep. route.URL is nil for a
// "urls" route, so it's left untouched below rather than merged.
func withRawQuery(route *model.Route, rawQuery string) *model.Route {
	inbound, err := url.ParseQuery(rawQuery)
	if err != nil {
		// Unparseable: pass it through verbatim rather than silently dropping
		// it. The upstream is the one entitled to reject its own query string.
		inbound = nil
	}

	out := route.Clone()
	if route.URL != nil {
		out.URL = mergeQuery(route.URL, rawQuery, inbound)
	}
	out.URLs = nil
	for _, u := range route.URLs {
		out.URLs = append(out.URLs, mergeQuery(u, rawQuery, inbound))
	}
	return out
}

// mergeQuery clones one target with the inbound query laid over its own.
func mergeQuery(u *url.URL, rawQuery string, inbound url.Values) *url.URL {
	clone := *u
	switch {
	case clone.RawQuery == "":
		// Nothing of its own to preserve either way.
		clone.RawQuery = rawQuery
	case inbound == nil:
		// Unparseable inbound, but this target already has a working query
		// of its own -- append the raw (invalid) string after it rather
		// than overwrite, so an upstream selector like ?network=maha
		// survives a caller sending a malformed query on top of it.
		clone.RawQuery = clone.RawQuery + "&" + rawQuery
	default:
		merged := clone.Query()
		for key, values := range inbound {
			merged[key] = values
		}
		clone.RawQuery = merged.Encode()
	}
	return &clone
}

func joinPath(u *url.URL, endpoint string) string {
	if u.Path == "" {
		u.Path = "/"
	}
	return path.Join(u.Path, endpoint)
}

// isV2Version checks if the version is 2.x.x
func isV2Version(version string) bool {
	return strings.HasPrefix(version, "2.")
}
