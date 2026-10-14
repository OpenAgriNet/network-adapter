package Grievance_test

// PM-KISAN application status, end to end: the real step over the shipped
// mappings, against a stand-in for PM-KISAN's chatbot service, configured as
// config/provider-adapter.yaml configures it.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/Grievance"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

const (
	asCapability = "openagrinet:PMKISANApplicationStatus"
	asBindingKey = "pmkisan-chatbot|" + asCapability

	// The chatbot service, named by the participant's baseUrl; the registry
	// rows and the plugin paths carry the method alone.
	asService = "/services/chatbotservice.asmx"
)

const asInitRequest = `{
  "context": {
    "version": "2.0.0", "action": "init", "networkId": "openagrinet",
    "transactionId": "4c810000-0000-4000-8000-000000000001",
    "messageId": "9e270000-0000-4000-8000-000000000002",
    "timestamp": "2026-10-07T10:58:00Z"
  },
  "message": { "contract": {
    "id": "a9f3b062-5e17-4c84-9d20-6b48f1c7e053",
    "commitments": [{
      "status": { "descriptor": { "code": "DRAFT" } },
      "offer": {
        "id": "off:pmkisan:application-status",
        "provider": { "id": "pmkisan-chatbot", "descriptor": { "name": "PM-KISAN Chatbot Service" } },
        "resourceIds": ["res:pmkisan:application-status"]
      },
      "resources": [{ "id": "res:pmkisan:application-status", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANApplicationStatus",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "applicant": { "mobile": "9812345670" }
      }
    }]
  }}
}`

const asStatusRequest = `{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "transactionId": "4c810000-0000-4000-8000-000000000001",
    "messageId": "b5300000-0000-4000-8000-000000000003",
    "timestamp": "2026-10-07T11:02:00Z"
  },
  "message": { "contract": {
    "id": "a9f3b062-5e17-4c84-9d20-6b48f1c7e053",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE" } },
      "offer": {
        "id": "off:pmkisan:application-status",
        "provider": { "id": "pmkisan-chatbot", "descriptor": { "name": "PM-KISAN Chatbot Service" } },
        "resourceIds": ["res:pmkisan:application-status"]
      },
      "resources": [{ "id": "res:pmkisan:application-status", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANApplicationStatus",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "applicant": { "mobile": "9812345670" },
        "challenge": { "method": "SMS_OTP", "value": "4821" }
      }
    }]
  }}
}`

// asRecord is what /ChatbotUserDetails decrypts to: the record, with the
// farmer's personal data beside it.
const asRecord = `{
  "Rsponce": "True", "Message": "Success",
  "BeneficiaryName": "LAKSHMI DEVI", "FatherName": "RAMAIAH", "DOB": "1980-01-01",
  "Address": "12 Main Road", "StateName": "UTTAR PRADESH", "DistrictName": "AGRA",
  "SubDistrictName": "ETMADPUR", "VillageName": "BARHAN",
  "Reg_No": "UP45678901C", "DateOfRegistration": "2021-06-14T00:00:00",
  "LatestInstallmentPaid": 15, "eKYC_Status": "N"
}`

// asBlocked is what /ChatbotBeneficiaryStatus decrypts to: one key per
// condition, each holding the portal's phrase.
const asBlocked = `{
  "Rsponce": "True", "Message": "Success",
  "LandStatus": "Land Seeding, KYS",
  "BankStatus": "Bank account details could not be verified",
  "AadhaarStatus": "No Errors",
  "TaxStatus": ""
}`

// asChatbot stands in for PM-KISAN's chatbot service: every path AES-128-CBC
// with the key in band, answering each under the request's key.
type asChatbot struct {
	t       *testing.T
	calls   map[string]int
	sent    map[string]map[string]string
	answers map[string]string // per path; empty means the defaults below
	status  map[string]int    // per path; non-zero answers with it and no body
}

func newASChatbot(t *testing.T) *asChatbot {
	return &asChatbot{t: t, calls: map[string]int{}, sent: map[string]map[string]string{},
		answers: map[string]string{
			"/ChatbotUserDetails":       asRecord,
			"/ChatbotBeneficiaryStatus": asBlocked,
		},
		status: map[string]int{}}
}

func (c *asChatbot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, asService)
	if path == r.URL.Path {
		c.t.Errorf("the chatbot service was called at %s, outside %s", r.URL.Path, asService)
	}
	c.calls[path]++
	if code := c.status[path]; code != 0 {
		w.WriteHeader(code)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var envelope map[string]string
	_ = json.Unmarshal(body, &envelope)
	sealed, keyString, found := strings.Cut(envelope["EncryptedRequest"], "@")
	if !found {
		c.t.Errorf("the chatbot service received %s, want base64(ciphertext)@key", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	key := pkCBCKey(keyString)
	block, _ := aes.NewCipher(key)
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plain, raw)
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	sent := map[string]string{}
	_ = json.Unmarshal(plain, &sent)
	c.sent[path] = sent

	answer := `{"Rsponce":"False","Message":"Request could not be processed"}`
	switch {
	case sent["Token"] != pkOTPToken:
		answer = `{"Rsponce":"False","Message":"Invalid Token"}`
	case c.answers[path] != "":
		answer = c.answers[path]
	case path == "/ChatbotOTP":
		answer = `{"Rsponce":"True","Message":"OTP has been sent to the registered mobile number"}`
	case path == "/ChatbotOTPVerified" && sent["OTP"] == pkValidOTP:
		answer = `{"Rsponce":"True","Message":"OTP Verified"}`
	}
	padded := []byte(answer)
	n := aes.BlockSize - len(padded)%aes.BlockSize
	padded = append(padded, bytes.Repeat([]byte{byte(n)}, n)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	fmt.Fprintf(w, `{"d":{"__type":"Chatbot.Output","output":%q}}`, base64.StdEncoding.EncodeToString(out))
}

// runAppStatus drives the real step over the shipped mappings against chatbot.
func runAppStatus(t *testing.T, chatbot *asChatbot, request string) ([]byte, error) {
	t.Helper()
	t.Setenv("TEST_PMKISAN_OTP_TOKEN", pkOTPToken)

	mappings := pkServeMappings(t)
	host := httptest.NewServer(chatbot)
	t.Cleanup(host.Close)

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	t.Cleanup(func() { _ = closeMapper() })

	registry := &pkRegistry{plan: &model.ProviderRecord{
		BindingKey:     asBindingKey,
		ParticipantID:  "pmkisan-chatbot",
		CapabilityCode: asCapability,
		BaseURL:        host.URL + asService,
		Actions: map[string]model.ActionPlan{
			"init": {Method: http.MethodPost, Path: "/ChatbotOTP",
				Mappings: mappings.URL + "/application-status.init.yaml", TimeoutMs: 20000},
			"status": {Method: http.MethodPost, Path: "/ChatbotUserDetails",
				Mappings: mappings.URL + "/application-status.status.yaml", TimeoutMs: 30000},
		},
	}}

	envelopes, _, err := Grievance.ParseEnvelopes(map[string]string{
		"envelope-pmkisan-chatbot":              "aesCbcKeyInBand",
		"envelopeTokenEnv-pmkisan-chatbot":      "TEST_PMKISAN_OTP_TOKEN",
		"envelopeTokenField-pmkisan-chatbot":    "Token",
		"envelopeRequestField-pmkisan-chatbot":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan-chatbot": "d.output,output",
		"otpVerifyPath-pmkisan-chatbot":         "/ChatbotOTPVerified",
		"otpVerifyActions-pmkisan-chatbot":      "status",
		"followUpPath-pmkisan-chatbot":          "/ChatbotBeneficiaryStatus",
		"followUpField-pmkisan-chatbot":         "_beneficiaryStatus",
		"followUpActions-pmkisan-chatbot":       "status",
	})
	if err != nil {
		t.Fatalf("ParseEnvelopes() = %v", err)
	}

	step, closeStep, err := Grievance.New(context.Background(), registry, mapper, &Grievance.Config{
		BindingKeys:      []string{asBindingKey},
		ProviderIDAt:     "message.contract.commitments[].offer.provider.id",
		CapabilityCodeAt: "message.contract.commitments[].commitmentAttributes.@type",
		AuthByProvider: map[string]*common.AuthProfile{
			"pmkisan-chatbot": {Scheme: util.AuthSchemeNone},
		},
		EnvelopeByProvider: envelopes,
	})
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	t.Cleanup(func() { _ = closeStep() })

	stepCtx := &model.StepContext{Context: t.Context(), Body: []byte(request)}
	if err := step.Run(stepCtx); err != nil {
		return nil, err
	}
	if len(stepCtx.ResponseBody) == 0 {
		t.Fatal("the step produced no answer and no error")
	}
	return stepCtx.ResponseBody, nil
}

func asAttributes(t *testing.T, body []byte) map[string]any {
	t.Helper()
	attributes, _ := pkAt(t, pkDecode(t, body), "message", "contract", "commitments", 0, "commitmentAttributes").(map[string]any)
	return attributes
}

// --- init -------------------------------------------------------------------

// The slot names the kind: enrolmentId, applicant.mobile or applicant.aadhaar.
func TestAppStatusInitAsksForAnOTPByTheSlotItIsIn(t *testing.T) {
	for _, tc := range []struct{ name, slot, id, types string }{
		{"mobile", `"applicant": { "mobile": "9812345670" }`, "9812345670", "Mobile"},
		{"registration", `"enrolmentId": "UP45678901C"`, "UP45678901C", "Ben_id"},
		{"aadhaar", `"applicant": { "aadhaar": "123456789012" }`, "123456789012", "Aadhar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chatbot := newASChatbot(t)
			request := strings.Replace(asInitRequest, `"applicant": { "mobile": "9812345670" }`, tc.slot, 1)
			body, err := runAppStatus(t, chatbot, request)
			if err != nil {
				t.Fatalf("Run() = %v", err)
			}
			sent := chatbot.sent["/ChatbotOTP"]
			if sent["Types"] != tc.types || sent["Values"] != tc.id || sent["Token"] != pkOTPToken {
				t.Errorf("/ChatbotOTP got %v, want Types %s, the id and the token", sent, tc.types)
			}

			answer := pkDecode(t, body)
			if got := pkAt(t, answer, "context", "action"); got != "on_init" {
				t.Errorf("action = %v, want on_init", got)
			}
			if got := pkAt(t, answer, "message", "contract", "commitments", 0, "status", "descriptor", "code"); got != "DRAFT" {
				t.Errorf("commitment status = %v, want DRAFT", got)
			}
			attributes := asAttributes(t, body)
			issued, _ := attributes["challengeIssued"].(map[string]any)
			if issued["method"] != "SMS_OTP" || issued["length"] != float64(4) || issued["valueType"] != "NUMERIC" ||
				issued["expiresAt"] == nil || issued["sentTo"] != nil {
				t.Errorf("challengeIssued = %v, want method, length 4, valueType NUMERIC, expiresAt and no sentTo", issued)
			}
			if attributes["applicant"] != nil || attributes["enrolmentId"] != nil || attributes["informationMode"] != "OnDemand" {
				t.Errorf("attributes = %v, want OnDemand with the identifier not echoed", attributes)
			}
			if strings.Contains(string(body), tc.id) {
				t.Errorf("on_init carries the applicant's id: %s", body)
			}
		})
	}
}

func TestAppStatusRefusesAnApplicantBeforeCallingAnything(t *testing.T) {
	at := "$.message.contract.commitments[0].commitmentAttributes."
	applicant := `"applicant": { "mobile": "9812345670" }`
	bad := "SCH_SCHEMA_VALIDATION_FAILED"
	for _, tc := range []struct {
		name, to, code, path string
	}{
		{"no identifier", `"x": 1`, bad, at + "enrolmentId"},
		{"an empty applicant", `"applicant": {}`, bad, at + "applicant"},
		{"the old idType shape", `"applicant": { "idType": "Mobile", "id": "9812345670" }`, bad, at + "applicant"},
		{"both enrolmentId and applicant", applicant + `, "enrolmentId": "UP45678901C"`, bad, at + "applicant"},
		{"both a mobile and an Aadhaar", `"applicant": { "mobile": "9812345670", "aadhaar": "123456789012" }`, bad, at + "applicant"},
		{"a mobile that is not one", `"applicant": { "mobile": "UP45678901" }`, bad, at + "applicant.mobile"},
		{"a mobile starting 5", `"applicant": { "mobile": "5812345670" }`, bad, at + "applicant.mobile"},
		{"an Aadhaar of eleven digits", `"applicant": { "aadhaar": "12345678901" }`, bad, at + "applicant.aadhaar"},
		{"a registration in native script", `"enrolmentId": "UP१२३४५"`, bad, at + "enrolmentId"},
		{"an Aadhaar number as the registration", `"enrolmentId": "123456789012"`, bad, at + "enrolmentId"},
		{"a grievance band", applicant + `, "grievance": { "description": "where is my money" }`, "SCH_INVALID_FORMAT", ""},
	} {
		for name, request := range map[string]string{"init": asInitRequest, "status": asStatusRequest} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				chatbot := newASChatbot(t)
				_, err := runAppStatus(t, chatbot, strings.Replace(request, applicant, tc.to, 1))
				if err == nil {
					t.Fatal("Run() succeeded, want a refusal")
				}
				beckn, status := pkBecknErr(t, err)
				if status != http.StatusBadRequest || beckn.Code != tc.code {
					t.Errorf("got %s/%d, want %s/400", beckn.Code, status, tc.code)
				}
				if tc.path != "" && (beckn.Details == nil || beckn.Details.Path != tc.path) {
					t.Errorf("details = %+v, want path %s", beckn.Details, tc.path)
				}
				if len(chatbot.calls) != 0 {
					t.Errorf("the chatbot service was called %v for a refused payload", chatbot.calls)
				}
				if strings.Contains(beckn.Message, "9812345670") || strings.Contains(beckn.Message, "UP45678901") ||
					strings.Contains(beckn.Message, "123456789012") {
					t.Errorf("the error %q carries the applicant's id", beckn.Message)
				}
			})
		}
	}
}

func TestAppStatusInitReportsAnOTPServiceRefusal(t *testing.T) {
	chatbot := newASChatbot(t)
	chatbot.answers["/ChatbotOTP"] = `{"Rsponce":"False","Message":"OTP limit exceeded for 9812345670"}`
	_, err := runAppStatus(t, chatbot, asInitRequest)
	beckn, status := pkBecknErr(t, err)
	if status != http.StatusBadGateway || beckn.Code != "NET_DOWNSTREAM_UNAVAILABLE" {
		t.Errorf("got %s/%d, want NET_DOWNSTREAM_UNAVAILABLE/502", beckn.Code, status)
	}
	if strings.Contains(beckn.Message, "9812345670") {
		t.Errorf("the error %q passes the portal's text through", beckn.Message)
	}
}

// --- status -----------------------------------------------------------------

func TestAppStatusReadsTheRecordAndWhatIsBlockingPayment(t *testing.T) {
	chatbot := newASChatbot(t)
	body, err := runAppStatus(t, chatbot, asStatusRequest)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	// Verified first, then the record, then the blockers -- each once, all
	// with the declared kind and the id, and the OTP to the verify alone.
	for _, path := range []string{"/ChatbotOTPVerified", "/ChatbotUserDetails", "/ChatbotBeneficiaryStatus"} {
		if chatbot.calls[path] != 1 {
			t.Errorf("%s called %d time(s), want 1", path, chatbot.calls[path])
		}
		sent := chatbot.sent[path]
		if sent["Types"] != "Mobile" || sent["Values"] != "9812345670" || sent["Token"] != pkOTPToken {
			t.Errorf("%s got %v, want Types Mobile, the id and the token", path, sent)
		}
	}
	if chatbot.sent["/ChatbotOTPVerified"]["OTP"] != pkValidOTP {
		t.Error("the verify did not carry the OTP")
	}
	for _, path := range []string{"/ChatbotUserDetails", "/ChatbotBeneficiaryStatus"} {
		if _, carried := chatbot.sent[path]["OTP"]; carried {
			t.Errorf("%s was sent the OTP; it goes to the verify alone", path)
		}
	}

	answer := pkDecode(t, body)
	if got := pkAt(t, answer, "context", "action"); got != "on_status" {
		t.Errorf("action = %v, want on_status", got)
	}
	if got := pkAt(t, answer, "message", "contract", "commitments", 0, "status", "descriptor", "code"); got != "CLOSED" {
		t.Errorf("commitment status = %v, want CLOSED: the question is answered", got)
	}

	attributes := asAttributes(t, body)
	want := map[string]any{
		"@context":        "https://openagrinet.github.io/network-specs/api-schemas/PMKISANApplicationStatus/v0.1/context.jsonld",
		"@type":           "openagrinet:PMKISANApplicationStatus",
		"informationMode": "Direct",
		"scheme":          map[string]any{"code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi"},
		"enrolmentId":     "UP45678901C",
		"application": map[string]any{
			"registeredOn":          "2021-06-14T00:00:00+05:30",
			"latestInstallmentPaid": float64(15),
			"ekyc":                  map[string]any{"code": "Pending"},
			"blockers": []any{
				map[string]any{"code": "Other", "name": "Bank account details could not be verified"},
				map[string]any{"code": "LandSeedingPending", "name": "Land Seeding, KYS"},
			},
		},
	}
	got, _ := json.Marshal(attributes)
	wanted, _ := json.Marshal(want)
	if string(got) != string(wanted) {
		t.Errorf("attributes =\n%s\nwant\n%s", got, wanted)
	}

	// An allow-list: none of the farmer's personal data, and not the id they
	// were looked up by.
	for _, leaked := range []string{"LAKSHMI", "RAMAIAH", "1980-01-01", "Main Road", "AGRA", "BARHAN", "9812345670", pkValidOTP} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("on_status carries %q", leaked)
		}
	}
}

func TestAppStatusSaysPaymentIsClearOnlyWhenTheCheckRan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		blocked  string
		status   int
		blockers any // nil: absent
	}{
		{"the check names nothing", `{"Rsponce":"True","Message":"Success","AadhaarStatus":"No Errors"}`, 0, []any{}},
		{"the check is unreachable", "", http.StatusInternalServerError, nil},
		{"the check refuses", `{"Rsponce":"False","Message":"Try later"}`, 0, nil},
		{"the check answers without a flag", `{"LandStatus":"Land Seeding, KYS"}`, 0, nil},
		{"the check names an income-tax payee", `{"Rsponce":"True","TaxStatus":"Income Tax Payee"}`, 0,
			[]any{map[string]any{"code": "IncomeTaxPayee", "name": "Income Tax Payee"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chatbot := newASChatbot(t)
			chatbot.answers["/ChatbotBeneficiaryStatus"] = tc.blocked
			chatbot.status["/ChatbotBeneficiaryStatus"] = tc.status
			body, err := runAppStatus(t, chatbot, asStatusRequest)
			if err != nil {
				t.Fatalf("Run() = %v; a failed blocker check must not fail the read", err)
			}
			application, _ := asAttributes(t, body)["application"].(map[string]any)
			if application["latestInstallmentPaid"] != float64(15) {
				t.Errorf("application = %v, want the record either way", application)
			}
			blockers, present := application["blockers"]
			switch {
			case tc.blockers == nil && present:
				t.Errorf("blockers = %v, want ABSENT: the check did not answer, which is not 'clear'", blockers)
			case tc.blockers != nil:
				got, _ := json.Marshal(blockers)
				want, _ := json.Marshal(tc.blockers)
				if !present || string(got) != string(want) {
					t.Errorf("blockers = %s (present %v), want %s", got, present, want)
				}
			}
		})
	}
}

func TestAppStatusReadsTheRecordsFieldsAsTheyArrive(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		path           []any
		want           any
	}{
		{"a .NET date", `"2021-06-14T00:00:00"`, `"/Date(1623609000000)/"`, []any{"registeredOn"}, "2021-06-14T00:00:00+05:30"},
		{"a US date with a time", `"2021-06-14T00:00:00"`, `"6/14/2021 12:00:00 AM"`, []any{"registeredOn"}, "2021-06-14T00:00:00+05:30"},
		{"a bare ISO date", `"2021-06-14T00:00:00"`, `"2021-06-14"`, []any{"registeredOn"}, "2021-06-14T00:00:00+05:30"},
		{"a date it cannot read", `"2021-06-14T00:00:00"`, `"sometime in June"`, []any{"registeredOn"}, nil},
		{"a count as text", `15`, `"0"`, []any{"latestInstallmentPaid"}, float64(0)},
		{"eKYC done", `"eKYC_Status": "N"`, `"eKYC_Status": "Y"`, []any{"ekyc", "code"}, "Done"},
		{"eKYC unrecognised", `"eKYC_Status": "N"`, `"eKYC_Status": "maybe"`, []any{"ekyc", "code"}, "Pending"},
		{"eKYC absent", `, "eKYC_Status": "N"`, ``, []any{"ekyc"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chatbot := newASChatbot(t)
			record := strings.Replace(asRecord, tc.from, tc.to, 1)
			if record == asRecord {
				t.Fatalf("the edit %q did not apply", tc.from)
			}
			chatbot.answers["/ChatbotUserDetails"] = record
			body, err := runAppStatus(t, chatbot, asStatusRequest)
			if err != nil {
				t.Fatalf("Run() = %v", err)
			}
			var node any = asAttributes(t, body)["application"]
			for _, key := range tc.path {
				fields, _ := node.(map[string]any)
				node = fields[key.(string)]
			}
			if fmt.Sprint(node) != fmt.Sprint(tc.want) {
				t.Errorf("%v = %v, want %v", tc.path, node, tc.want)
			}
		})
	}
}

func TestAppStatusReportsNoRegistrationAsAnAcceptedAnswer(t *testing.T) {
	chatbot := newASChatbot(t)
	chatbot.answers["/ChatbotUserDetails"] = `{"Rsponce":"False","Message":"No record found for 9812345670"}`
	_, err := runAppStatus(t, chatbot, asStatusRequest)
	beckn, status := pkBecknErr(t, err)
	if status != http.StatusAccepted || beckn.Code != "BIZ_NO_RESULTS_FOUND" {
		t.Errorf("got %s/%d, want BIZ_NO_RESULTS_FOUND/202", beckn.Code, status)
	}
	if strings.Contains(beckn.Message, "9812345670") {
		t.Errorf("the answer %q passes the portal's text through", beckn.Message)
	}
	if chatbot.calls["/ChatbotBeneficiaryStatus"] != 0 {
		t.Error("the blockers were read for a registration that does not exist")
	}
}

func TestAppStatusFailsClosedOnARecordItCannotUse(t *testing.T) {
	for name, record := range map[string]string{
		"no flag":          `{"Reg_No":"UP45678901C","LatestInstallmentPaid":3}`,
		"no instalments":   `{"Rsponce":"True","Reg_No":"UP45678901C"}`,
		"a negative count": `{"Rsponce":"True","Reg_No":"UP45678901C","LatestInstallmentPaid":"-1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			chatbot := newASChatbot(t)
			chatbot.answers["/ChatbotUserDetails"] = record
			_, err := runAppStatus(t, chatbot, asStatusRequest)
			beckn, status := pkBecknErr(t, err)
			if status != http.StatusBadGateway || beckn.Code != "NET_DOWNSTREAM_UNAVAILABLE" {
				t.Errorf("got %s/%d, want NET_DOWNSTREAM_UNAVAILABLE/502", beckn.Code, status)
			}
		})
	}
}

func TestAppStatusAWrongOTPReadsNothing(t *testing.T) {
	chatbot := newASChatbot(t)
	_, err := runAppStatus(t, chatbot, strings.Replace(asStatusRequest, `"value": "4821"`, `"value": "0000"`, 1))
	beckn, status := pkBecknErr(t, err)
	if status != http.StatusBadRequest || beckn.Code != "BIZ_GENERIC_ERROR" {
		t.Errorf("got %s/%d, want BIZ_GENERIC_ERROR/400", beckn.Code, status)
	}
	if chatbot.calls["/ChatbotUserDetails"] != 0 || chatbot.calls["/ChatbotBeneficiaryStatus"] != 0 {
		t.Errorf("the record was read on a wrong OTP: %v", chatbot.calls)
	}
}

func TestAppStatusRefusesAMissingOrMalformedOTP(t *testing.T) {
	for _, tc := range []struct{ name, from, to, code string }{
		{"no OTP", `,
        "challenge": { "method": "SMS_OTP", "value": "4821" }`, ``, "SCH_REQUIRED_FIELD_MISSING"},
		{"five digits", `"value": "4821"`, `"value": "48210"`, "SCH_INVALID_FORMAT"},
		{"another method", `"method": "SMS_OTP"`, `"method": "EMAIL_OTP"`, "SCH_INVALID_FORMAT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chatbot := newASChatbot(t)
			request := strings.Replace(asStatusRequest, tc.from, tc.to, 1)
			if request == asStatusRequest {
				t.Fatalf("the edit %q did not apply", tc.from)
			}
			_, err := runAppStatus(t, chatbot, request)
			beckn, status := pkBecknErr(t, err)
			if status != http.StatusBadRequest || beckn.Code != tc.code {
				t.Errorf("got %s/%d, want %s/400", beckn.Code, status, tc.code)
			}
			if len(chatbot.calls) != 0 {
				t.Errorf("the chatbot service was called %v", chatbot.calls)
			}
		})
	}
}
