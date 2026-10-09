package Grievance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

const validOTP = "4821"

// otpService stands in for the OTP realm's verify call: it opens the
// key-in-band request, checks the OTP and token, and answers sealed.
type otpService struct {
	t      *testing.T
	calls  int
	path   string
	sent   map[string]string
	status int // non-zero answers with this status and no body
}

func (s *otpService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.calls++
	s.path = r.URL.Path
	if s.status != 0 {
		w.WriteHeader(s.status)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var envelope map[string]string
	_ = json.Unmarshal(raw, &envelope)
	sealed, key, _ := strings.Cut(envelope["EncryptedRequest"], "@")
	cipherText, _ := base64.StdEncoding.DecodeString(sealed)
	plain, err := cbcDecrypt(key, cipherText)
	if err != nil {
		s.t.Errorf("the OTP service could not open the request: %v", err)
		return
	}
	s.sent = map[string]string{}
	_ = json.Unmarshal(plain, &s.sent)

	answer := `{"Rsponce":"False","Message":"Invalid OTP"}`
	if s.sent["OTP"] == validOTP && s.sent["Token"] == testOTPToken {
		answer = `{"Rsponce":"True","Message":"OTP Verified"}`
	}
	out, _ := cbcEncrypt(key, []byte(answer))
	fmt.Fprintf(w, `{"d":{"output":%q}}`, out)
}

// pmkisanEnvelopeSettings is PM-KISAN's block as config/provider-adapter.yaml
// declares it: GCM by default, CBC for init, OTP verified before support and
// status.
func pmkisanEnvelopeSettings() envelopeSettings {
	s := pmkisanSettings()
	s.otp = otpRealm()
	s.otpActions = "init"
	s.verifyPath = "/ChatbotOTPVerified"
	s.verifyActions = "support,status"
	return s
}

func pmkisanEnvelope(t *testing.T) *providerEnvelope {
	t.Helper()
	setTestSecrets(t)
	t.Setenv(testOTPTokenEnv, testOTPToken)
	e, err := newProviderEnvelope("pmkisan", pmkisanEnvelopeSettings())
	if err != nil {
		t.Fatalf("newProviderEnvelope() = %v", err)
	}
	return e
}

// planOn is a call plan whose init action lives on the OTP service at url.
func planOn(url string) *model.ProviderRecord {
	return &model.ProviderRecord{
		BaseURL: "http://grievance.invalid",
		Actions: map[string]model.ActionPlan{
			"init":    {Method: http.MethodPost, Path: "/ChatbotOTP", BaseURL: url, TimeoutMs: 5000},
			"support": {Method: http.MethodPost, Path: "/LodgeGrievance"},
			"status":  {Method: http.MethodPost, Path: "/GrievanceStatusCheck"},
		},
	}
}

func supportPayload(otp string) map[string]any {
	channel := map[string]any{"provider": map[string]any{"id": "pmkisan"}}
	if otp != "" {
		channel["challenge"] = map[string]any{"method": "SMS_OTP", "value": otp}
	}
	return map[string]any{"message": map[string]any{"support": map[string]any{
		"orderId": "UP12345678A", "channels": []any{channel}}}}
}

func statusPayload(otp string) map[string]any {
	attributes := map[string]any{"enrolmentId": "UP12345678A"}
	if otp != "" {
		attributes["challenge"] = map[string]any{"method": "SMS_OTP", "value": otp}
	}
	return map[string]any{"message": map[string]any{"contract": map[string]any{
		"commitments": []any{map[string]any{"commitmentAttributes": attributes}}}}}
}

func TestInitGoesThroughTheOTPRealm(t *testing.T) {
	e := pmkisanEnvelope(t)
	wire, _, err := e.Seal(context.Background(), common.Exchange{Action: "init", Plan: planOn("http://otp.invalid")},
		[]byte(`{"Types":"Ben_id","Values":"UP12345678A"}`))
	if err != nil {
		t.Fatalf("Seal() = %v", err)
	}
	var body map[string]string
	_ = json.Unmarshal(wire, &body)
	if !strings.Contains(body["EncryptedRequest"], "@") {
		t.Errorf("init sealed %q, want the OTP realm's cipher@key", body["EncryptedRequest"])
	}
}

func TestSupportAndStatusVerifyTheOTPThenGoThroughTheGrievanceRealm(t *testing.T) {
	for action, payload := range map[string]map[string]any{
		"support": supportPayload(validOTP),
		"status":  statusPayload(validOTP),
	} {
		t.Run(action, func(t *testing.T) {
			e := pmkisanEnvelope(t)
			otp := &otpService{t: t}
			server := httptest.NewServer(otp)
			defer server.Close()

			wire, _, err := e.Seal(context.Background(),
				common.Exchange{Action: action, Beckn: payload, Plan: planOn(server.URL)}, []byte(`{"Type":"x"}`))
			if err != nil {
				t.Fatalf("Seal() = %v", err)
			}
			if otp.calls != 1 || otp.path != "/ChatbotOTPVerified" {
				t.Fatalf("the OTP service saw %d call(s) at %q, want one at /ChatbotOTPVerified", otp.calls, otp.path)
			}
			for field, want := range map[string]string{"Types": "Ben_id", "Values": "UP12345678A", "OTP": validOTP, "Token": testOTPToken} {
				if otp.sent[field] != want {
					t.Errorf("verify sent %s = %q, want %q", field, otp.sent[field], want)
				}
			}
			var body map[string]string
			_ = json.Unmarshal(wire, &body)
			if body["EncryptedRequest"] == "" || strings.Contains(body["EncryptedRequest"], "@") {
				t.Errorf("%s sealed %q, want the grievance realm's GCM payload", action, body["EncryptedRequest"])
			}
		})
	}
}

func TestAWrongOTPStopsTheCallItGuards(t *testing.T) {
	e := pmkisanEnvelope(t)
	otp := &otpService{t: t}
	server := httptest.NewServer(otp)
	defer server.Close()

	_, _, err := e.Seal(context.Background(),
		common.Exchange{Action: "support", Beckn: supportPayload("0000"), Plan: planOn(server.URL)}, []byte(`{}`))
	if err == nil {
		t.Fatal("Seal() succeeded on a wrong OTP")
	}
	if code, status := codeOf(t, err); code != "BIZ_GENERIC_ERROR" || status != http.StatusBadRequest {
		t.Errorf("got %s/%d, want BIZ_GENERIC_ERROR/400 -- a wrong OTP is the farmer's to fix, and not a 401", code, status)
	}
	if strings.Contains(err.Error(), "Invalid OTP") {
		t.Errorf("error %q passes the portal's text through", err)
	}
}

func TestAMissingChallengeIsRefusedWithoutCallingTheOTPService(t *testing.T) {
	e := pmkisanEnvelope(t)
	otp := &otpService{t: t}
	server := httptest.NewServer(otp)
	defer server.Close()

	_, _, err := e.Seal(context.Background(),
		common.Exchange{Action: "status", Beckn: statusPayload(""), Plan: planOn(server.URL)}, []byte(`{}`))
	if code, status := codeOf(t, err); code != "SCH_REQUIRED_FIELD_MISSING" || status != http.StatusBadRequest {
		t.Errorf("got %s/%d, want SCH_REQUIRED_FIELD_MISSING/400", code, status)
	}
	if otp.calls != 0 {
		t.Errorf("the OTP service was called %d time(s) without an OTP to verify", otp.calls)
	}
}

func TestAnUnreachableOTPServiceIsTheUpstreamFailing(t *testing.T) {
	e := pmkisanEnvelope(t)
	otp := &otpService{t: t, status: http.StatusServiceUnavailable}
	server := httptest.NewServer(otp)
	defer server.Close()

	_, _, err := e.Seal(context.Background(),
		common.Exchange{Action: "support", Beckn: supportPayload(validOTP), Plan: planOn(server.URL)}, []byte(`{}`))
	if code, status := codeOf(t, err); code != "NET_DOWNSTREAM_UNAVAILABLE" || status != http.StatusBadGateway {
		t.Errorf("got %s/%d, want NET_DOWNSTREAM_UNAVAILABLE/502", code, status)
	}
}

func TestSucceededFailsClosed(t *testing.T) {
	for answer, want := range map[string]bool{
		`{"Rsponce":"True"}`:  true,
		`{"Rsponce":"true"}`:  true,
		`{"Responce":"True"}`: true,
		`{"Rsponce":"False"}`: false,
		`{"Rsponce":""}`:      false,
		`{"Message":"sent"}`:  false,
		`not json`:            false,
	} {
		if got := succeeded([]byte(answer)); got != want {
			t.Errorf("succeeded(%s) = %v, want %v", answer, got, want)
		}
	}
}

func TestNewProviderEnvelopeRefusesInconsistentOTPSettings(t *testing.T) {
	for name, tweak := range map[string]func(*envelopeSettings){
		"otp envelope with no actions":  func(s *envelopeSettings) { s.otpActions = "" },
		"verify path with no actions":   func(s *envelopeSettings) { s.verifyActions = "" },
		"verify actions with no path":   func(s *envelopeSettings) { s.verifyPath = "" },
		"otp settings with no envelope": func(s *envelopeSettings) { s.otp.kind = "" },
		"neither envelope nor otp":      func(s *envelopeSettings) { *s = envelopeSettings{} },
	} {
		t.Run(name, func(t *testing.T) {
			s := pmkisanEnvelopeSettings()
			tweak(&s)
			if _, err := newProviderEnvelope("pmkisan", s); err == nil {
				t.Fatal("newProviderEnvelope() accepted settings that cannot work")
			}
		})
	}
}

func TestAProviderWithOnlyAnOTPRealmSendsItsOtherActionsPlain(t *testing.T) {
	t.Setenv(testOTPTokenEnv, testOTPToken)
	s := envelopeSettings{otp: otpRealm(), otpActions: "init"}
	e, err := newProviderEnvelope("pmkisan", s)
	if err != nil {
		t.Fatalf("newProviderEnvelope() = %v", err)
	}
	wire, open, err := e.Seal(context.Background(), common.Exchange{Action: "status"}, []byte(`{"a":1}`))
	if err != nil || string(wire) != `{"a":1}` {
		t.Fatalf("Seal() = %s, %v; want the body untouched", wire, err)
	}
	if got, _ := open(context.Background(), []byte(`{"b":2}`)); string(got) != `{"b":2}` {
		t.Errorf("open() = %s, want the answer untouched", got)
	}
}
