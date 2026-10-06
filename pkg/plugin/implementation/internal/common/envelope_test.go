// The Envelope hook: what a capability needs when its provider's bodies are not
// plain JSON on the wire.
//
// A separate file from common_test.go, which is already past two thousand
// lines.
package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

func planWithAction(baseURL, action string) *model.ProviderRecord {
	plan := testPlan(baseURL, http.MethodPost)
	plan.Actions = map[string]model.ActionPlan{
		action: {Method: http.MethodPost, Path: "/lodge", Mappings: testMappingRef, TimeoutMs: 2000},
	}
	return plan
}

// recordingUpstream answers with answer and remembers the body it was sent.
func recordingUpstream(t *testing.T, answer string, sent *string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*sent = string(body)
		fmt.Fprint(w, answer)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// --- the envelope -------------------------------------------------------------

// reversingEnvelope stands in for a real codec: reversible, and obviously not
// the plaintext, so a test can tell which side of it a body was seen on.
type reversingEnvelope struct {
	sealErr, openErr error
	sealed, opened   int
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

func (e *reversingEnvelope) Seal(_ context.Context, mapped []byte) ([]byte, error) {
	e.sealed++
	if e.sealErr != nil {
		return nil, e.sealErr
	}
	return reverse(mapped), nil
}

func (e *reversingEnvelope) Open(_ context.Context, answer []byte) ([]byte, error) {
	e.opened++
	if e.openErr != nil {
		return nil, e.openErr
	}
	return reverse(answer), nil
}

func envelopedStep(t *testing.T, url string, mapper definition.Mapper, envelope Envelope) *Step {
	t.Helper()
	return newStep(t, &stubRegistry{plan: planWithAction(url, "select")}, mapper, func(c *Config) {
		c.EnvelopeByProvider = map[string]Envelope{testProvider: envelope}
	})
}

func TestRunSealsTheRequestAndOpensTheAnswer(t *testing.T) {
	var sent string
	url := recordingUpstream(t, string(reverse([]byte(`{"answer":42}`))), &sent)
	mapper := &stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"ok":true}`)}
	envelope := &reversingEnvelope{}

	if _, err := runStep(t, envelopedStep(t, url, mapper, envelope), selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if sent != string(reverse([]byte(`{"ask":1}`))) {
		t.Errorf("upstream received %q, want the sealed request", sent)
	}
	response, _ := mapper.responseInput.(map[string]any)
	if fmt.Sprint(response["response"]) != "map[answer:42]" {
		t.Errorf("the response half saw %v, want the opened answer", response["response"])
	}
	if envelope.sealed != 1 || envelope.opened != 1 {
		t.Errorf("sealed %d, opened %d; want one of each", envelope.sealed, envelope.opened)
	}
}

func TestRunStopsWhenTheEnvelopeRefuses(t *testing.T) {
	refusal := model.NewCodedErr(http.StatusInternalServerError, "NET_INTERNAL_ERROR", errors.New("no key"))

	t.Run("seal", func(t *testing.T) {
		var sent string
		url := recordingUpstream(t, `{}`, &sent)
		step := envelopedStep(t, url, &stubMapper{requestResult: []byte(`{}`)},
			&reversingEnvelope{sealErr: refusal})
		if _, err := runStep(t, step, selectBody); !errors.Is(err, refusal) {
			t.Fatalf("Run() = %v, want the envelope's own error", err)
		}
		if sent != "" {
			t.Errorf("upstream was called with %q after the seal failed; it must never see plaintext", sent)
		}
	})

	t.Run("open", func(t *testing.T) {
		var sent string
		url := recordingUpstream(t, `{}`, &sent)
		mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
		step := envelopedStep(t, url, mapper, &reversingEnvelope{openErr: refusal})
		if _, err := runStep(t, step, selectBody); !errors.Is(err, refusal) {
			t.Fatalf("Run() = %v, want the envelope's own error", err)
		}
		if mapper.responseInput != nil {
			t.Error("the response half ran on an answer the envelope refused")
		}
	})
}

func TestNewRefusesAnEnvelopeForAProviderItDoesNotServe(t *testing.T) {
	cfg := &Config{BindingKeys: []string{testBindingKey}}
	cfg.setProviderAuth(AuthProfile{Scheme: util.AuthSchemeNone})
	cfg.EnvelopeByProvider = map[string]Envelope{"elsewhere": &reversingEnvelope{}}
	_, _, err := New(context.Background(), &stubRegistry{}, &stubMapper{}, nil, cfg)
	if err == nil || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("New() = %v, want the stray provider refused", err)
	}
}

func TestRunSendsPlainJSONToAProviderWithNoEnvelope(t *testing.T) {
	var sent string
	url := recordingUpstream(t, `{"ok":true}`, &sent)
	mapper := &stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"a":1}`)}
	step := newStep(t, &stubRegistry{plan: planWithAction(url, "select")}, mapper)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if sent != `{"ask":1}` {
		t.Errorf("upstream received %q, want the mapped request untouched", sent)
	}
}
