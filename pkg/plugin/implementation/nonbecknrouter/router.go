// Package nonbecknrouter routes a request by its path alone, never reading the
// body, for services whose calls are not Beckn messages.
package nonbecknrouter

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"gopkg.in/yaml.v3"
)

const targetTypeURL = "url"

// Config holds the configuration for the non-Beckn router.
type Config struct {
	RoutingConfig string
}

type routingFile struct {
	RoutingRules []rule `yaml:"routingRules"`
}

type rule struct {
	TargetType string   `yaml:"targetType"`
	Target     target   `yaml:"target"`
	Endpoints  []string `yaml:"endpoints"`
}

type target struct {
	URL           string `yaml:"url"`
	ExcludeAction bool   `yaml:"excludeAction"`
}

// Router routes a request to the target of the rule that names its path.
type Router struct {
	routes map[string]*url.URL
}

// New loads the routing rules from the configured file.
func New(ctx context.Context, config *Config) (*Router, func() error, error) {
	if config == nil {
		return nil, nil, fmt.Errorf("config cannot be nil")
	}
	rules, err := readRules(config.RoutingConfig)
	if err != nil {
		return nil, nil, err
	}
	routes, err := buildRoutes(rules)
	if err != nil {
		return nil, nil, err
	}
	log.Infof(ctx, "nonbecknrouter: loaded %d routes from %s", len(routes), config.RoutingConfig)
	return &Router{routes: routes}, nil, nil
}

func readRules(file string) ([]rule, error) {
	if file == "" {
		return nil, fmt.Errorf("routingConfig path is empty")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("error reading routing config %s: %w", file, err)
	}
	var parsed routingFile
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("error parsing routing config %s: %w", file, err)
	}
	if len(parsed.RoutingRules) == 0 {
		return nil, fmt.Errorf("routing config %s has no routingRules", file)
	}
	return parsed.RoutingRules, nil
}

func buildRoutes(rules []rule) (map[string]*url.URL, error) {
	routes := make(map[string]*url.URL)
	for i, r := range rules {
		base, err := parseTarget(r)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
		if len(r.Endpoints) == 0 {
			return nil, fmt.Errorf("rule %d: endpoints is empty", i+1)
		}
		for _, raw := range r.Endpoints {
			endpoint := cleanEndpoint(raw)
			if endpoint == "" {
				return nil, fmt.Errorf("rule %d: an endpoint is blank", i+1)
			}
			if _, taken := routes[endpoint]; taken {
				return nil, fmt.Errorf("rule %d: endpoint %q is routed twice", i+1, endpoint)
			}
			routes[endpoint] = routeURL(base, endpoint, r.Target.ExcludeAction)
		}
	}
	return routes, nil
}

func parseTarget(r rule) (*url.URL, error) {
	if r.TargetType != targetTypeURL {
		return nil, fmt.Errorf("targetType %q is not supported, only %q", r.TargetType, targetTypeURL)
	}
	raw := strings.TrimSpace(r.Target.URL)
	if raw == "" {
		return nil, fmt.Errorf("target url is required")
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid target url %q: %w", raw, err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("target url %q must be an http or https URL with a host", raw)
	}
	return base, nil
}

func cleanEndpoint(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "/")
}

func routeURL(base *url.URL, endpoint string, excludeAction bool) *url.URL {
	routed := *base
	if !excludeAction {
		routed.Path = path.Join("/", base.Path, endpoint)
	}
	return &routed
}

// Route returns the target for the request path. The body is never read, so a
// request that is not a Beckn message routes like any other.
func (r *Router) Route(ctx context.Context, reqURL *url.URL, body []byte) (*model.Route, error) {
	if reqURL == nil {
		return nil, fmt.Errorf("reqURL must not be nil")
	}
	endpoint := cleanEndpoint(reqURL.Path)
	base, found := r.routes[endpoint]
	if !found {
		log.Infof(ctx, "nonbecknrouter: refusing %q, no routing rule names it", endpoint)
		return nil, fmt.Errorf("no routing rule for %q", endpoint)
	}
	routed := *base
	routed.RawQuery = reqURL.RawQuery
	return &model.Route{TargetType: targetTypeURL, URL: &routed}, nil
}
