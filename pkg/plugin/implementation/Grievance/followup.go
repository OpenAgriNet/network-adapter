package Grievance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/envelope"
)

// followUpFields are a provider block's follow-up settings, read here and not
// by envelope.Parse.
var followUpFields = map[string]func(*followUpSettings, string){
	"followUpPath":      func(s *followUpSettings, v string) { s.path = v },
	"followUpField":     func(s *followUpSettings, v string) { s.field = v },
	"followUpActions":   func(s *followUpSettings, v string) { s.actions = v },
	"followUpTimeoutMs": func(s *followUpSettings, v string) { s.timeoutMs = v },
}

type followUpSettings struct{ path, field, actions, timeoutMs string }

// followUp makes a second read after a successful call on the provider's own
// host and adds its answer under field; on failure the field is absent, never
// empty (ADR-0005). PM-KISAN application status reads its blockers this way.
type followUp struct {
	inner     common.Envelope
	reseal    envelope.Resealer
	provider  string
	path      string
	field     string
	actions   map[string]bool
	timeoutMs int
	client    *http.Client
}

var _ common.Envelope = (*followUp)(nil)

// splitFollowUps takes the follow-up settings out of the plugin config.
func splitFollowUps(config map[string]string) (map[string]*followUpSettings, map[string]string) {
	settings := map[string]*followUpSettings{}
	rest := make(map[string]string, len(config))
	for key, value := range config {
		field, provider, dashed := strings.Cut(key, "-")
		set, isFollowUp := followUpFields[field]
		if !dashed || !isFollowUp {
			rest[key] = value
			continue
		}
		if settings[provider] == nil {
			settings[provider] = &followUpSettings{}
		}
		set(settings[provider], value)
	}
	return settings, rest
}

// newFollowUp wraps a provider's envelope with its follow-up read.
func newFollowUp(provider string, s followUpSettings, inner common.Envelope) (*followUp, error) {
	actions := map[string]bool{}
	for _, a := range strings.Split(s.actions, ",") {
		if a = strings.TrimSpace(a); a != "" {
			actions[a] = true
		}
	}
	if s.path == "" || s.field == "" || len(actions) == 0 {
		return nil, fmt.Errorf("%s: followUpPath, followUpField and followUpActions are set together or not at all", provider)
	}
	reseal, ok := inner.(envelope.Resealer)
	if inner == nil || !ok {
		return nil, fmt.Errorf("%s: a follow-up needs the provider's envelope to seal it", provider)
	}
	timeoutMs := 0
	if strings.TrimSpace(s.timeoutMs) != "" {
		ms, err := strconv.Atoi(strings.TrimSpace(s.timeoutMs))
		if err != nil || ms <= 0 {
			return nil, fmt.Errorf("%s: followUpTimeoutMs must be a positive number of milliseconds, got %q", provider, s.timeoutMs)
		}
		timeoutMs = ms
	}
	return &followUp{inner: inner, reseal: reseal, provider: provider, path: s.path, field: s.field,
		actions: actions, timeoutMs: timeoutMs, client: envelope.NewClient()}, nil
}

// Seal seals through the provider's envelope and, for a follow-up action,
// wraps the opener so a successful answer gets the second read added.
func (f *followUp) Seal(ctx context.Context, exchange common.Exchange, mapped []byte) ([]byte, common.Opener, error) {
	sealed, open, err := f.inner.Seal(ctx, exchange, mapped)
	if err != nil || !f.actions[exchange.Action] {
		return sealed, open, err
	}
	return sealed, func(ctx context.Context, answer []byte) ([]byte, error) {
		plain, err := open(ctx, answer)
		if err != nil || !succeeded(plain) {
			return plain, err
		}
		extra, ok := f.read(ctx, exchange, mapped)
		if !ok {
			return plain, nil
		}
		var first map[string]any
		if json.Unmarshal(plain, &first) != nil {
			return plain, nil
		}
		first[f.field] = extra
		return json.Marshal(first)
	}, nil
}

// read makes the follow-up call. A failure is logged, never returned.
func (f *followUp) read(ctx context.Context, exchange common.Exchange, mapped []byte) (any, bool) {
	if exchange.Plan == nil || exchange.Plan.BaseURL == "" {
		return nil, false
	}
	sealed, open, err := f.reseal.Reseal(ctx, exchange, mapped)
	if err != nil {
		log.Warnf(ctx, "%s's follow-up %s was not sent: %v", f.provider, f.path, err)
		return nil, false
	}
	answer, err := envelope.Post(ctx, f.client, f.provider, "the follow-up read", exchange.Plan.BaseURL, f.path, f.timeoutMs, sealed)
	if err != nil {
		return nil, false
	}
	plain, err := open(ctx, answer)
	if err != nil {
		log.Warnf(ctx, "%s's follow-up %s answered with nothing usable", f.provider, f.path)
		return nil, false
	}
	var extra any
	if json.Unmarshal(plain, &extra) != nil {
		return nil, false
	}
	return extra, true
}
