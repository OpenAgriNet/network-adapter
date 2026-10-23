package envelope

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// followUp adds a second read's answer under field after a successful call;
// on failure the field is absent, never empty (ADR-0005).
type followUp struct {
	provider  string
	path      string
	field     string
	actions   map[string]bool
	succeeded func([]byte) bool
	client    *http.Client
	// budget is the follow-up's own time budget, not the first call's.
	budget model.ActionPlan
}

// wrap opens the first answer and, if it succeeded, adds the follow-up's.
func (f *followUp) wrap(c codec, base string, mapped []byte, open common.Opener) common.Opener {
	return func(ctx context.Context, answer []byte) ([]byte, error) {
		plain, err := open(ctx, answer)
		if err != nil || !f.succeeded(plain) {
			return plain, err
		}
		extra, ok := f.read(ctx, c, base, mapped)
		if !ok {
			return plain, nil
		}
		var first map[string]any
		if json.Unmarshal(plain, &first) != nil {
			return plain, nil
		}
		first[f.field] = extra
		return json.Marshal(first)
	}
}

// read makes the follow-up call. A failure is logged, never returned.
func (f *followUp) read(ctx context.Context, c codec, base string, mapped []byte) (any, bool) {
	sealed, open, err := c.seal(ctx, mapped)
	if err != nil {
		log.Warnf(ctx, "%s's follow-up %s was not sent: %v", f.provider, f.path, err)
		return nil, false
	}
	answer, err := postSealed(ctx, f.client, f.provider, "the follow-up read", base, f.path, f.budget, sealed)
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
