package Grievance

import (
	"errors"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// pmkisanSettings are what pmkisan.yaml declares; the field names live there,
// not in this package.
var pmkisanSettings = challengeSettings{
	bodyFields:   "Types, Values, OTP",
	identityFrom: "enrolmentId=Ben_id, applicant.mobile=Mobile",
	otpFrom:      "challenge.response.value",
	methodFrom:   "challenge.response.method=OTP",
}

func pmkisanSpec(t *testing.T) *challengeSpec {
	t.Helper()
	spec, err := newChallengeSpec("pmkisan", pmkisanSettings)
	if err != nil {
		t.Fatalf("newChallengeSpec() = %v", err)
	}
	return spec
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
		if got := pmkisanSaidYes([]byte(answer)); got != want {
			t.Errorf("pmkisanSaidYes(%s) = %v, want %v", answer, got, want)
		}
	}
}

// The slot names the kind, and which slot names which kind is config:
// enrolmentId is a Ben_id and applicant.mobile is a Mobile because
// otpVerifyIdentityFrom says so.
func TestRequestReadsTheKindFromTheSlot(t *testing.T) {
	spec := pmkisanSpec(t)
	for name, tc := range map[string]struct {
		attributes map[string]any
		kind, id   string
	}{
		"enrolmentId":          {map[string]any{"enrolmentId": "UP12345678A"}, "Ben_id", "UP12345678A"},
		"mobile":               {map[string]any{"applicant": map[string]any{"mobile": "9812345670"}}, "Mobile", "9812345670"},
		"an Aadhaar":           {map[string]any{"applicant": map[string]any{"aadhaar": "123456789012"}}, "", ""},
		"the old idType shape": {map[string]any{"applicant": map[string]any{"idType": "Mobile", "id": "9812345670"}}, "", ""},
		"nothing":              {map[string]any{}, "", ""},
	} {
		tc.attributes["challenge"] = map[string]any{"response": map[string]any{"method": "OTP", "value": "4821"}}
		payload := map[string]any{"message": map[string]any{"contract": map[string]any{"commitments": []any{
			map[string]any{"commitmentAttributes": tc.attributes}}}}}

		fields, err := spec.request(common.Exchange{Beckn: payload})
		if tc.id == "" {
			if err == nil {
				t.Errorf("request(%s) = %v, want a refusal: no identifier the provider knows", name, fields)
			}
			continue
		}
		if err != nil || fields["Types"] != tc.kind || fields["Values"] != tc.id {
			t.Errorf("request(%s) = %v, %v; want %q, %q", name, fields, err, tc.kind, tc.id)
		}
	}
	support := map[string]any{"message": map[string]any{"support": map[string]any{"orderId": "UP12345678A",
		"channels": []any{map[string]any{"enrolmentId": "BR295454592",
			"challenge": map[string]any{"response": map[string]any{"method": "OTP", "value": "4821"}}}}}}}
	fields, err := spec.request(common.Exchange{Beckn: support})
	if err != nil || fields["Values"] != "BR295454592" {
		t.Errorf("request(support) = %v, %v; want the channel's enrolmentId, not orderId", fields, err)
	}
}

func TestRequestRefusesWhatCannotBeVerified(t *testing.T) {
	spec := pmkisanSpec(t)
	withOTP := func(method, value string) common.Exchange {
		return common.Exchange{Beckn: map[string]any{"message": map[string]any{"contract": map[string]any{"commitments": []any{
			map[string]any{"commitmentAttributes": map[string]any{"enrolmentId": "UP12345678A",
				"challenge": map[string]any{"response": map[string]any{"method": method, "value": value}}}}}}}}}
	}
	for name, tc := range map[string]struct {
		exchange common.Exchange
		code     string
		field    string
	}{
		"no OTP":         {withOTP("OTP", ""), "SCH_REQUIRED_FIELD_MISSING", "challenge.response.value"},
		"another method": {withOTP("EMAIL_OTP", "4821"), "SCH_INVALID_FORMAT", "challenge.response.method"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := spec.request(tc.exchange)
			var be model.BecknErrorer
			if !errors.As(err, &be) || be.BecknError().Code != tc.code {
				t.Fatalf("request() = %v, want %s", err, tc.code)
			}
			want := "$.message.contract.commitments[0].commitmentAttributes." + tc.field
			if d := be.BecknError().Details; d == nil || d.Path != want {
				t.Errorf("details = %+v, want path %s", d, want)
			}
		})
	}
	fields, err := spec.request(withOTP("OTP", "4821"))
	if err != nil || fields["Types"] != "Ben_id" || fields["Values"] != "UP12345678A" || fields["OTP"] != "4821" {
		t.Errorf("request() = %v, %v; want the identifier, its type and the OTP", fields, err)
	}
}

// A half-written block is a startup error, not a farmer's failed OTP.
func TestChallengeSettingsAreRefusedIncomplete(t *testing.T) {
	for name, s := range map[string]challengeSettings{
		"no body fields":    {identityFrom: "enrolmentId=Ben_id", otpFrom: "challenge.response.value"},
		"two body fields":   {bodyFields: "Types, Values", identityFrom: "enrolmentId=Ben_id", otpFrom: "challenge.response.value"},
		"no identity":       {bodyFields: "Types, Values, OTP", otpFrom: "challenge.response.value"},
		"identity unpaired": {bodyFields: "Types, Values, OTP", identityFrom: "enrolmentId", otpFrom: "challenge.response.value"},
		"no OTP path":       {bodyFields: "Types, Values, OTP", identityFrom: "enrolmentId=Ben_id"},
		"method unpaired":   {bodyFields: "Types, Values, OTP", identityFrom: "enrolmentId=Ben_id", otpFrom: "challenge.response.value", methodFrom: "challenge.response.method"},
	} {
		if _, err := newChallengeSpec("pmkisan", s); err == nil {
			t.Errorf("newChallengeSpec(%s) = nil, want an error naming the provider", name)
		}
	}
}

// otpVerifyPath without the field settings would only fail on the first farmer
// to present an OTP, so ParseEnvelopes refuses it at startup.
func TestVerifyWithoutFieldSettingsIsRefusedAtStartup(t *testing.T) {
	_, _, err := ParseEnvelopes(map[string]string{
		"envelope-pmkisan":         "aesGcm",
		"envelopeKeyEnv-pmkisan":   "PMKISAN_KEY",
		"envelopeNonceEnv-pmkisan": "PMKISAN_NONCE",
		"otpVerifyPath-pmkisan":    "/ChatbotOTPVerified",
		"otpVerifyActions-pmkisan": "status",
	})
	if err == nil || !strings.Contains(err.Error(), "otpVerifyBodyFields") {
		t.Fatalf("ParseEnvelopes() = %v, want an error naming otpVerifyBodyFields", err)
	}
}

// splitChallenges takes only the otpVerify* field settings; everything else,
// otpVerifyPath included, goes on to envelope.Parse.
func TestSplitChallengesLeavesTheRest(t *testing.T) {
	settings, rest := splitChallenges(map[string]string{
		"otpVerifyBodyFields-pmkisan": "Types, Values, OTP",
		"otpVerifyOtpFrom-pmkisan":    "challenge.response.value",
		"otpVerifyPath-pmkisan":       "/ChatbotOTPVerified",
		"envelope-pmkisan":            "aesGcm",
	})
	if settings["pmkisan"] == nil || settings["pmkisan"].bodyFields != "Types, Values, OTP" {
		t.Fatalf("settings = %+v, want pmkisan's body fields", settings)
	}
	if len(rest) != 2 || rest["otpVerifyPath-pmkisan"] == "" || rest["envelope-pmkisan"] == "" {
		t.Errorf("rest = %v, want otpVerifyPath and envelope left for envelope.Parse", rest)
	}
}
