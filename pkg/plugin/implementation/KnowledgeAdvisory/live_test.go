package KnowledgeAdvisory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/KnowledgeAdvisory"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// TestLiveEndToEnd drives the real step against the LIVE provider: a real
// oauth2 exchange, a real POST /search, and the shipped mapping over the
// answer that comes back.
//
// SKIPPED BY DEFAULT. It runs only when the credentials and endpoints are in
// the environment, so CI and an ordinary `go test ./...` never reach it:
//
//	KNOWLEDGE_TOKEN_URL      the provider's OAuth2 token endpoint
//	KNOWLEDGE_BASE_URL       the provider's base URL
//	KNOWLEDGE_CLIENT_ID      } the client credentials, by variable name --
//	KNOWLEDGE_CLIENT_SECRET  } the values are never in this repo
//
// No host appears in this file for the same reason it appears in no mapping or
// config: the endpoints are deployment facts, so they arrive from the
// environment.
//
// It is worth having despite needing credentials, because three things can
// only be established against the real thing: that the token endpoint answers
// the shape the code parses, that the token it issues is accepted by the
// provider, and that a second request reuses it. The last is measured, not
// timed -- the exchange goes through a counting proxy.
func TestLiveEndToEnd(t *testing.T) {
	tokenURL, base := os.Getenv("KNOWLEDGE_TOKEN_URL"), os.Getenv("KNOWLEDGE_BASE_URL")
	if tokenURL == "" || base == "" || os.Getenv("KNOWLEDGE_CLIENT_ID") == "" {
		t.Skip("live credentials not in the environment")
	}

	mappings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(filepath.Join("../../../../config/mappings/knowledge",
			filepath.Base(r.URL.Path)))
		if err != nil {
			t.Errorf("mapping read: %v", err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, string(body))
	}))
	defer mappings.Close()

	// A counting proxy in front of the real issuer, so token REUSE is measured
	// against the live endpoint rather than inferred from timings.
	var exchanges atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		body, _ := io.ReadAll(r.Body)
		fwd, err := http.NewRequestWithContext(r.Context(), http.MethodPost, tokenURL,
			strings.NewReader(string(body)))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fwd.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		resp, err := http.DefaultClient.Do(fwd)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
	}))
	defer issuer.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("mapper: %v", err)
	}
	defer closeMapper()

	const key = "knowledge-provider|openagrinet:KnowledgeAdvisory"
	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey: key, ParticipantID: "knowledge-provider",
		CapabilityCode: "openagrinet:KnowledgeAdvisory", BaseURL: base,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodPost, Path: "/docs-pipeline-api/search",
				Mappings:  mappings.URL + "/knowledge-advisory.select.yaml",
				TimeoutMs: 60000, RetryMax: 1},
		},
	}}

	step, closeStep, err := KnowledgeAdvisory.New(context.Background(), registry, mapper,
		&KnowledgeAdvisory.Config{
			BindingKeys: []string{key},
			AuthByProvider: map[string]*common.AuthProfile{
				strings.Split(key, "|")[0]: {
					Scheme:          "oauth2",
					TokenURL:        issuer.URL + "/token",
					ClientIDEnv:     "KNOWLEDGE_CLIENT_ID",
					ClientSecretEnv: "KNOWLEDGE_CLIENT_SECRET",
				},
			},
			MaxResponseBytes: 1048576,
		})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	defer closeStep()

	// Two requests: the second must reuse the token rather than exchange again.
	for i := 1; i <= 2; i++ {
		ctx := &model.StepContext{Context: t.Context(), Body: []byte(selectRequest)}
		if err := step.Run(ctx); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if len(ctx.ResponseBody) == 0 {
			t.Fatalf("request %d produced no answer", i)
		}
		var answer map[string]any
		if err := json.Unmarshal(ctx.ResponseBody, &answer); err != nil {
			t.Fatalf("request %d: answer is not JSON: %v", i, err)
		}
		res := resourcesOf(t, answer)
		t.Logf("  request %d: %d resource(s), %d bytes", i, len(res), len(ctx.ResponseBody))
		if i == 2 && len(res) > 0 {
			a := res[0].(map[string]any)["resourceAttributes"].(map[string]any)
			recs, _ := a["recommendations"].([]any)
			sup, _ := a["supportingResourceIds"].([]any)
			msg, _ := recs[0].(map[string]any)["message"].(string)
			t.Logf("  informationMode=%v topics=%v", a["informationMode"], a["topics"])
			t.Logf("  recommendations=%d supportingIds=%d first message=%d chars",
				len(recs), len(sup), len(msg))
			t.Logf("  rationale=%v", a["rationale"])
			if strings.TrimSpace(msg) == "" {
				t.Error("the advisory carries an empty message")
			}
			out, _ := json.Marshal(a)
			_ = os.WriteFile(os.Getenv("CLAUDE_JOB_DIR")+"/tmp/live_attrs.json", out, 0o644)
		}
	}

	// The whole point of caching: two requests, ONE exchange.
	if got := exchanges.Load(); got != 1 {
		t.Errorf("the issuer was called %d times for 2 requests, want 1", got)
	}
	t.Logf("  token exchanges for 2 requests: %d", exchanges.Load())
}
