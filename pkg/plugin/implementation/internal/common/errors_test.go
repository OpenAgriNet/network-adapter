package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// Every capability serves through Step, so these cases are what any provider's
// caller sees for each failure: one status, one code, one message.
//
// Not parallel: each step's cleanup closes idle connections on the shared
// default transport, which breaks a connection another test is reusing.

type wantError struct {
	status  int
	code    string
	message string
}

// requireCallerError checks err is answered with want, and that the message
// names no host: what the provider and the network said stays in the log.
func requireCallerError(t *testing.T, err error, want wantError, hostsToHide ...string) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) {
		t.Fatalf("error = %v (%T), want a *model.CodedErr", err, err)
	}
	wireErr := coded.BecknError()
	if coded.HTTPStatus() != want.status || wireErr.Code != want.code || wireErr.Message != want.message {
		t.Errorf("answer = %d %s %q, want %d %s %q",
			coded.HTTPStatus(), wireErr.Code, wireErr.Message, want.status, want.code, want.message)
	}
	for _, host := range hostsToHide {
		if strings.Contains(wireErr.Message, host) {
			t.Errorf("message %q names the host %s", wireErr.Message, host)
		}
	}
}

func TestRun_ProviderFailure_AnswersStandardError(t *testing.T) {

	answerWith := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	tests := []struct {
		name      string
		provider  http.HandlerFunc
		timeoutMs int
		want      wantError
	}{
		{"provider answers 400", answerWith(http.StatusBadRequest, `{"error":"bad district"}`),
			0, wantError{http.StatusBadRequest, "BIZ_GENERIC_ERROR", "mausamgram rejected the request"}},
		{"provider answers 404", answerWith(http.StatusNotFound, `{"error":"no such record"}`),
			0, wantError{http.StatusNotFound, "NET_ENTITY_NOT_FOUND", "mausamgram has no matching record"}},
		{"provider answers 401", answerWith(http.StatusUnauthorized, ""),
			0, wantError{http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", "mausamgram rejected the adapter's credentials"}},
		{"provider answers 403", answerWith(http.StatusForbidden, ""),
			0, wantError{http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", "mausamgram rejected the adapter's credentials"}},
		{"provider answers 500", answerWith(http.StatusInternalServerError, "stack trace"),
			0, wantError{http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", "mausamgram returned 500 after 2 attempts"}},
		{"provider answers 429", answerWith(http.StatusTooManyRequests, ""),
			0, wantError{http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", "mausamgram returned 429 after 2 attempts"}},
		{"provider answers 405", answerWith(http.StatusMethodNotAllowed, ""),
			0, wantError{http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", "mausamgram returned 405 after 1 attempt"}},
		{"provider answers HTML", answerWith(http.StatusOK, "<html>maintenance</html>"),
			0, wantError{http.StatusBadGateway, "SCH_SCHEMA_ADAPTATION_FAILED", "mausamgram sent a response that could not be read"}},
		{"provider answers an empty body", answerWith(http.StatusOK, ""),
			0, wantError{http.StatusBadGateway, "SCH_SCHEMA_ADAPTATION_FAILED", "mausamgram sent a response that could not be read"}},
		{"provider is too slow", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
			100, wantError{http.StatusGatewayTimeout, "NET_TIMEOUT", "mausamgram did not respond within 100ms (2 attempts)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(tt.provider)
			defer upstream.Close()
			plan := testPlan(upstream.URL, http.MethodGet)
			if tt.timeoutMs > 0 {
				action := plan.Actions["select"]
				action.TimeoutMs = tt.timeoutMs
				plan.Actions["select"] = action
			}
			mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
			step := newStep(t, &stubRegistry{plan: plan}, mapper)

			_, err := runStep(t, step, selectBody)

			requireCallerError(t, err, tt.want, strings.TrimPrefix(upstream.URL, "http://"))
		})
	}
}

func TestRun_ProviderNotReachable_Answers502(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := upstream.URL
	upstream.Close()
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(unreachableURL, http.MethodGet)}, mapper)

	_, err := runStep(t, step, selectBody)

	requireCallerError(t, err,
		wantError{http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", "mausamgram is not reachable after 2 attempts"},
		strings.TrimPrefix(unreachableURL, "http://"))
}

func TestRun_CredentialNotSet_Answers500(t *testing.T) {
	var called atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) }))
	defer upstream.Close()
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper, func(c *Config) {
		c.AuthByProvider = authForTestProvider(AuthProfile{Scheme: util.AuthSchemeBasic,
			UsernameEnv: "TEST_ERRORS_ABSENT_USER", PasswordEnv: "TEST_ERRORS_ABSENT_KEY"})
	})

	_, err := runStep(t, step, selectBody)

	requireCallerError(t, err,
		wantError{http.StatusInternalServerError, "NET_INTERNAL_ERROR", "mausamgram is not configured on this adapter"})
	if called.Load() {
		t.Error("the provider was called without a credential")
	}
}

func TestRun_FailureBeforeTheCall_AnswersStandardError(t *testing.T) {

	switchedOff := testPlan("http://provider.invalid", http.MethodGet)
	switchedOff.Actions = map[string]model.ActionPlan{"status": {Method: http.MethodGet, Path: "/status", Mappings: testMappingRef}}
	tests := []struct {
		name     string
		registry *stubRegistry
		body     string
		want     wantError
	}{
		{"registry unavailable", &stubRegistry{err: errors.New("dial tcp 10.0.0.9:8081: connection refused")}, selectBody,
			wantError{http.StatusServiceUnavailable, "NET_DOWNSTREAM_UNAVAILABLE", "registry is unavailable"}},
		{"binding switched off", &stubRegistry{err: definition.ErrProviderRecordNotFound}, selectBody,
			wantError{http.StatusNotFound, "NET_ENTITY_NOT_FOUND", testBindingKey + " is not available"}},
		{"action switched off", &stubRegistry{plan: switchedOff}, selectBody,
			wantError{http.StatusNotFound, "NET_ENTITY_NOT_FOUND", testBindingKey + ` does not serve action "select"; it serves status`}},
		{"receiverId missing", &stubRegistry{}, strings.Replace(selectBody, `"receiverId": "oan.test"`, `"bppId": ""`, 1),
			wantError{http.StatusBadRequest, "CTX_MISSING_FIELD", "context.receiverId is missing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := newStep(t, tt.registry, &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)})

			_, err := runStep(t, step, tt.body)

			requireCallerError(t, err, tt.want, "10.0.0.9")
		})
	}
}

func TestRun_MappingFileFailsToLoad_Answers500(t *testing.T) {
	mapper := &stubMapper{verifyErr: errors.New(`jsonmapper: mapping "https://example.com/m.yaml" returned 404 Not Found`)}
	step := newStep(t, &stubRegistry{plan: testPlan("http://provider.invalid", http.MethodGet)}, mapper)

	_, err := runStep(t, step, selectBody)

	requireCallerError(t, err,
		wantError{http.StatusInternalServerError, "NET_INTERNAL_ERROR", "mausamgram is not configured on this adapter"}, "example.com")
}

func TestRun_MappingRefusal_KeepsItsOwnAnswer(t *testing.T) {
	refusal := model.NewBadReqErr("", errors.New("this capability needs a Point location"))
	step := newStep(t, &stubRegistry{plan: testPlan("http://provider.invalid", http.MethodGet)}, &stubMapper{verifyErr: refusal})

	_, err := runStep(t, step, selectBody)

	requireCallerError(t, err, wantError{http.StatusBadRequest, "SCH_INVALID_FORMAT", "this capability needs a Point location"})
}

func TestRun_ResponseHalfProducesNothing_Answers502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"unexpected": true}`))
	}))
	defer upstream.Close()
	mapper := &stubMapper{requestResult: []byte(`{}`)}
	step := newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodGet)}, mapper)

	_, err := runStep(t, step, selectBody)

	requireCallerError(t, err,
		wantError{http.StatusBadGateway, "SCH_SCHEMA_ADAPTATION_FAILED", "the provider's response could not be converted"})
}

// A request with a deadline (the adapter sets one just under the server's
// write timeout) gets its answer AnswerMargin early, so a hop that forwarded
// it still receives the 504 rather than timing out itself.
func TestRun_RequestDeadline_Answers504BeforeIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer upstream.Close()
	plan := testPlan(upstream.URL, http.MethodGet)
	action := plan.Actions["select"]
	action.TimeoutMs = 30000
	plan.Actions["select"] = action
	step := newStep(t, &stubRegistry{plan: plan}, &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)})
	ctx, cancel := context.WithTimeout(t.Context(), util.AnswerMargin+500*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := step.Run(&model.StepContext{Context: ctx, OwnSubscriberID: "oan.test", Body: []byte(selectBody)})

	requireCallerError(t, err,
		wantError{http.StatusGatewayTimeout, "NET_TIMEOUT", "mausamgram did not answer before the request ended"})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("answered after %v, want within the margin left before the deadline", elapsed)
	}
}
