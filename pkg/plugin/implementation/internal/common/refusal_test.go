// _error: a mapping half refusing the call instead of producing a document.
package common

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// refusalStep is a step over mapper whose provider answers {"ok":true},
// counting calls.
func refusalStep(t *testing.T, mapper *stubMapper, calls *int) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(provider.Close)
	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodPost)}, mapper)
}

func TestRefusal_RequestHalf_RefusesWithoutCallingProvider(t *testing.T) {
	var calls int
	mapper := &stubMapper{
		requestResult:  []byte(`{"_error":{"status":400,"code":"SCH_REQUIRED_FIELD_MISSING","message":"phone is required"}}`),
		responseResult: []byte(`{"a":1}`),
	}
	_, err := runStep(t, refusalStep(t, mapper, &calls), selectBody)

	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest ||
		coded.BecknError().Code != "SCH_REQUIRED_FIELD_MISSING" {
		t.Fatalf("error = %v, want 400 SCH_REQUIRED_FIELD_MISSING", err)
	}
	if calls != 0 {
		t.Errorf("provider called %d times for a refused request, want 0", calls)
	}
}

func TestRefusal_ResponseHalf_ReturnsItsStatusAndCode(t *testing.T) {
	for name, tc := range map[string]struct {
		refusal string
		status  int
		code    string
	}{
		"explicit status": {`{"status":502,"code":"NET_DOWNSTREAM_UNAVAILABLE","message":"refused"}`, http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE"},
		"status omitted":  {`{"code":"BIZ_GENERIC_ERROR","message":"refused"}`, http.StatusBadRequest, "BIZ_GENERIC_ERROR"},
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"_error":` + tc.refusal + `}`)}
			stepCtx, err := runStep(t, refusalStep(t, mapper, &calls), selectBody)

			var coded *model.CodedErr
			if !errors.As(err, &coded) || coded.HTTPStatus() != tc.status || coded.BecknError().Code != tc.code {
				t.Fatalf("error = %v, want %d %s", err, tc.status, tc.code)
			}
			if len(stepCtx.ResponseBody) != 0 {
				t.Errorf("a refusal produced a response body: %s", stepCtx.ResponseBody)
			}
		})
	}
}

func TestRefusal_Status202_ReturnsAckWithCode(t *testing.T) {
	var calls int
	mapper := &stubMapper{requestResult: []byte(`{}`),
		responseResult: []byte(`{"_error":{"status":202,"code":"BIZ_NO_RESULTS_FOUND","message":"nothing on file"}}`)}
	_, err := runStep(t, refusalStep(t, mapper, &calls), selectBody)

	var ack *model.AckNoCallbackErr
	if !errors.As(err, &ack) {
		t.Fatalf("error = %v (%T), want a 202 ACK", err, err)
	}
	if ack.Status != model.StatusACK || ack.Err.Code != "BIZ_NO_RESULTS_FOUND" || ack.Err.Message != "nothing on file" {
		t.Errorf("ack = %s %+v, want ACK BIZ_NO_RESULTS_FOUND with the mapping's message", ack.Status, ack.Err)
	}
}

func TestRefusal_UnreadableError_FailsRatherThanAnswering(t *testing.T) {
	for name, refusal := range map[string]string{
		"no message": `{"_error":{"status":502,"code":"X"}}`,
		"not object": `{"_error":"refused"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(refusal)}
			stepCtx, err := runStep(t, refusalStep(t, mapper, &calls), selectBody)
			if err == nil || len(stepCtx.ResponseBody) != 0 {
				t.Errorf("Run() = %v with body %s, want an error and no answer", err, stepCtx.ResponseBody)
			}
		})
	}
}

func TestRefusal_NoErrorField_AnswersAsBefore(t *testing.T) {
	var calls int
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"message":{"ok":true}}`)}
	stepCtx, err := runStep(t, refusalStep(t, mapper, &calls), selectBody)
	if err != nil || string(stepCtx.ResponseBody) != `{"message":{"ok":true}}` || calls != 1 {
		t.Errorf("Run() = %v, body %s, %d calls; want the mapped answer after one call", err, stepCtx.ResponseBody, calls)
	}
}

// statusStep is a step whose provider answers every call with status and body.
func statusStep(t *testing.T, mapper *stubMapper, status int, body string, calls *int) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(provider.Close)
	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodPost)}, mapper)
}

func TestExplainRefusal_Provider4xxMappedToError_ReturnsIt(t *testing.T) {
	var calls int
	mapper := &stubMapper{requestResult: []byte(`{}`),
		responseResult: []byte(`{"_error":{"status":400,"code":"BIZ_GENERIC_ERROR","message":"wrong OTP"}}`)}
	_, err := runStep(t, statusStep(t, mapper, http.StatusBadRequest, `{"reason":"x"}`, &calls), selectBody)

	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest || coded.BecknError().Code != "BIZ_GENERIC_ERROR" {
		t.Fatalf("error = %v, want the mapping's 400 BIZ_GENERIC_ERROR", err)
	}
	input, _ := mapper.responseInput.(map[string]any)
	if input["_status"] != http.StatusBadRequest {
		t.Errorf("_status = %v, want the provider's 400", input["_status"])
	}
	if calls != 1 {
		t.Errorf("provider called %d times, want 1", calls)
	}
}

func TestExplainRefusal_Provider4xxNotRecognised_Returns502(t *testing.T) {
	var calls int
	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	_, err := runStep(t, statusStep(t, mapper, http.StatusNotFound, `not json`, &calls), selectBody)
	assertStatus(t, err, http.StatusBadGateway)
}

func TestExplainRefusal_Provider5xx_NotShownToTheMapping(t *testing.T) {
	var calls int
	mapper := &stubMapper{requestResult: []byte(`{}`),
		responseResult: []byte(`{"_error":{"status":400,"code":"X","message":"would hide the outage"}}`)}
	_, err := runStep(t, statusStep(t, mapper, http.StatusServiceUnavailable, ``, &calls), selectBody)
	assertStatus(t, err, http.StatusBadGateway)
	if mapper.responseInput != nil {
		t.Error("the response half was run for a 5xx; only a 4xx is the request's to explain")
	}
}
