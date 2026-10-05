package jsonmapper

// reject_test.go covers the reject section and the code and status a failed
// check can carry.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// withReject refuses a wrong OTP as the caller's fault and anything else that
// is not responseCode 1 as the provider's.
const withReject = `
required:
  - check: $exists(beckn.phone)
    message: "phone is required"
    code: SCH_REQUIRED_FIELD_MISSING
  - check: $exists(beckn.season) = false or beckn.season = "Kharif"
    message: "season must be Kharif"
reject:
  - check: response.responseCode = 1 or response.responseMessage != "Invalid OTP"
    message: "the OTP is wrong"
    code: BIZ_GENERIC_ERROR
    status: 400
  - check: response.responseCode = 1
    message: "the provider refused"
    code: NET_DOWNSTREAM_UNAVAILABLE
    status: 502
request: |
  { "phone": beckn.phone }
response: |
  { "ok": true }
`

// assertCoded fails unless err is a CodedErr with this status and code.
func assertCoded(t *testing.T, err error, status int, code string) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) {
		t.Fatalf("error = %v (%T), want a coded %d %s", err, err, status, code)
	}
	if coded.HTTPStatus() != status || coded.BecknError().Code != code {
		t.Errorf("error = %d %s, want %d %s", coded.HTTPStatus(), coded.BecknError().Code, status, code)
	}
}

func rejectInput(code int, message string) map[string]any {
	return map[string]any{
		"beckn":    map[string]any{"phone": "9876543210"},
		"response": map[string]any{"responseCode": code, "responseMessage": message},
	}
}

func TestReject_SuccessfulAnswer_ReturnsNil(t *testing.T) {
	srv := newMappingServer(t, withReject, nil)
	defer srv.Close()

	if err := newTestMapper(t).Reject(context.Background(), ref(srv.URL), rejectInput(1, "ok")); err != nil {
		t.Errorf("Reject() = %v, want nil for a successful answer", err)
	}
}

func TestReject_FirstFailingCondition_ReturnsItsCodeAndStatus(t *testing.T) {
	srv := newMappingServer(t, withReject, nil)
	defer srv.Close()

	err := newTestMapper(t).Reject(context.Background(), ref(srv.URL), rejectInput(0, "Invalid OTP"))
	assertCoded(t, err, http.StatusBadRequest, "BIZ_GENERIC_ERROR")
	if !strings.Contains(err.Error(), "the OTP is wrong") {
		t.Errorf("error %q should carry the mapping's message", err)
	}
}

func TestReject_LaterCondition_ReturnsItsCodeAndStatus(t *testing.T) {
	srv := newMappingServer(t, withReject, nil)
	defer srv.Close()

	err := newTestMapper(t).Reject(context.Background(), ref(srv.URL), rejectInput(0, "Service down"))
	assertCoded(t, err, http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE")
	if strings.Contains(err.Error(), "Service down") {
		t.Errorf("error %q leaks the provider's own message", err)
	}
}

func TestReject_NoRejectSection_ReturnsNil(t *testing.T) {
	srv := newMappingServer(t, withChecks, nil)
	defer srv.Close()

	if err := newTestMapper(t).Reject(context.Background(), ref(srv.URL), rejectInput(0, "anything")); err != nil {
		t.Errorf("Reject() = %v, want nil when the file declares no reject", err)
	}
}

func TestReject_ConditionNotBoolean_ReturnsError(t *testing.T) {
	srv := newMappingServer(t, `
reject:
  - check: response.responseMessage
    message: "never shown"
response: |
  { "ok": true }
`, nil)
	defer srv.Close()

	if err := newTestMapper(t).Reject(context.Background(), ref(srv.URL), rejectInput(1, "text")); err == nil {
		t.Error("Reject() accepted a condition that answered a string")
	}
}

func TestReject_ConditionWithoutMessage_ReturnsError(t *testing.T) {
	srv := newMappingServer(t, `
reject:
  - check: response.responseCode = 1
response: |
  { "ok": true }
`, nil)
	defer srv.Close()

	err := newTestMapper(t).Reject(context.Background(), ref(srv.URL), rejectInput(1, "ok"))
	if err == nil || !strings.Contains(err.Error(), "no message") {
		t.Errorf("Reject() = %v, want a condition with no message refused", err)
	}
}

func TestReject_ConditionThatWillNotCompile_ReturnsErrorAndKeepsTransform(t *testing.T) {
	srv := newMappingServer(t, `
reject:
  - check: response.responseCode = = 1
    message: "broken"
response: |
  { "ok": true }
`, nil)
	defer srv.Close()

	mapper := newTestMapper(t)
	if err := mapper.Reject(context.Background(), ref(srv.URL), rejectInput(1, "ok")); err == nil {
		t.Error("Reject() ran a condition that does not compile")
	}
	if out, err := mapper.Transform(context.Background(), ref(srv.URL), "response", rejectInput(1, "ok")); err != nil || string(out) != `{"ok":true}` {
		t.Errorf("Transform() = %s, %v; a broken reject must not take the response half down", out, err)
	}
}

func TestReject_UnfetchableMapping_ReturnsError(t *testing.T) {
	if err := newTestMapper(t).Reject(context.Background(), "file:///etc/passwd", rejectInput(1, "ok")); err == nil {
		t.Error("Reject() fetched a reference that is not http or https")
	}
}

func TestVerify_RequirementWithCode_ReturnsThatCode(t *testing.T) {
	srv := newMappingServer(t, withReject, nil)
	defer srv.Close()

	err := newTestMapper(t).Verify(context.Background(), ref(srv.URL), map[string]any{"beckn": map[string]any{}})
	assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
}

func TestVerify_RequirementWithoutCode_KeepsDefaultCode(t *testing.T) {
	srv := newMappingServer(t, withReject, nil)
	defer srv.Close()

	err := newTestMapper(t).Verify(context.Background(), ref(srv.URL),
		map[string]any{"beckn": map[string]any{"phone": "1", "season": "Rabi"}})
	assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
}

func TestVerify_RejectSection_NotRunBeforeTheCall(t *testing.T) {
	srv := newMappingServer(t, withReject, nil)
	defer srv.Close()

	// No response yet: only required runs, so a payload meeting it passes.
	if err := newTestMapper(t).Verify(context.Background(), ref(srv.URL),
		map[string]any{"beckn": map[string]any{"phone": "1"}}); err != nil {
		t.Errorf("Verify() = %v, want reject conditions left for after the call", err)
	}
}
