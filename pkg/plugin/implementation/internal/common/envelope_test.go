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
	exchange         Exchange
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

func (e *reversingEnvelope) Seal(_ context.Context, exchange Exchange, mapped []byte) ([]byte, Opener, error) {
	e.sealed++
	e.exchange = exchange
	if e.sealErr != nil {
		return nil, nil, e.sealErr
	}
	return reverse(mapped), func(_ context.Context, answer []byte) ([]byte, error) {
		e.opened++
		if e.openErr != nil {
			return nil, e.openErr
		}
		return reverse(answer), nil
	}, nil
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

func TestRunHandsTheEnvelopeTheExchange(t *testing.T) {
	var sent string
	url := recordingUpstream(t, string(reverse([]byte(`{}`))), &sent)
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	envelope := &reversingEnvelope{}

	if _, err := runStep(t, envelopedStep(t, url, mapper, envelope), selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if envelope.exchange.Action != "select" || envelope.exchange.Plan == nil || envelope.exchange.Beckn == nil {
		t.Errorf("exchange = %+v, want the action, the plan and the inbound payload", envelope.exchange)
	}
}

// keyedRegistry answers each binding key with its own record.
type keyedRegistry map[string]*model.ProviderRecord

func (r keyedRegistry) ProviderRecord(_ context.Context, key string) (*model.ProviderRecord, error) {
	if record, ok := r[key]; ok {
		return record, nil
	}
	return nil, definition.ErrProviderRecordNotFound
}

const otherBindingKey = "other-host|openagrinet:Other"

// hostedStep serves testBindingKey on participant, with select hosted by
// otherBindingKey's participant at elsewhere ("" leaves it unpublished).
func hostedStep(t *testing.T, participant, elsewhere string, mapper definition.Mapper) *Step {
	t.Helper()
	registry := keyedRegistry{testBindingKey: planWithAction(participant, "select")}
	if elsewhere != "" {
		registry[otherBindingKey] = &model.ProviderRecord{BindingKey: otherBindingKey, BaseURL: elsewhere}
	}
	return newStep(t, registry, mapper, func(c *Config) {
		c.HostedElsewhere = map[string]map[string]string{testProvider: {"select": otherBindingKey}}
	})
}

func TestRunCallsTheHostOfTheParticipantNamedForTheAction(t *testing.T) {
	var onElsewhere, onParticipant string
	elsewhere := recordingUpstream(t, `{"ok":true}`, &onElsewhere)
	participant := recordingUpstream(t, `{"ok":true}`, &onParticipant)

	mapper := &stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"a":1}`)}
	if _, err := runStep(t, hostedStep(t, participant, elsewhere, mapper), selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if onElsewhere == "" || onParticipant != "" {
		t.Errorf("the named host got %q, the participant got %q; want the named host called", onElsewhere, onParticipant)
	}
}

func TestRunCallsTheParticipantForAnActionHostedNowhereElse(t *testing.T) {
	var sent string
	participant := recordingUpstream(t, `{"ok":true}`, &sent)

	mapper := &stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"a":1}`)}
	step := newStep(t, keyedRegistry{testBindingKey: planWithAction(participant, "select")}, mapper, func(c *Config) {
		c.HostedElsewhere = map[string]map[string]string{testProvider: {"init": otherBindingKey}}
	})
	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if sent == "" {
		t.Error("the participant's host was not called")
	}
}

func TestRunIsAnUpstreamFailureWhenTheNamedHostIsUnpublished(t *testing.T) {
	var sent string
	participant := recordingUpstream(t, `{}`, &sent)

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	_, err := runStep(t, hostedStep(t, participant, "", mapper), selectBody)
	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadGateway {
		t.Fatalf("Run() = %v, want a 502", err)
	}
	if sent != "" {
		t.Errorf("the participant was called with %q instead", sent)
	}
}

func TestNewRefusesAHostForAProviderItDoesNotServeOrABadKey(t *testing.T) {
	for name, hosts := range map[string]map[string]map[string]string{
		"an unserved provider": {"someone-else": {"init": otherBindingKey}},
		"a key with no |":      {testProvider: {"init": "other-host"}},
		"an empty half":        {testProvider: {"init": "|openagrinet:Other"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{BindingKeys: []string{testBindingKey}, HostedElsewhere: hosts}
			cfg.setProviderAuth(AuthProfile{Scheme: util.AuthSchemeNone})
			if _, _, err := New(context.Background(), &stubRegistry{}, &stubMapper{}, nil, cfg); err == nil {
				t.Error("New() accepted it")
			}
		})
	}
}

func TestRunHandsTheEnvelopeTheRegistry(t *testing.T) {
	var sent string
	url := recordingUpstream(t, string(reverse([]byte(`{}`))), &sent)
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)}
	envelope := &reversingEnvelope{}
	if _, err := runStep(t, envelopedStep(t, url, mapper, envelope), selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if envelope.exchange.Registry == nil {
		t.Error("the envelope was not handed the registry")
	}
}
