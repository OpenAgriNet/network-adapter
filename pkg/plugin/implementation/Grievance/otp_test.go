package Grievance

import (
	"errors"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

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

// The slot names the kind: enrolmentId or applicant.mobile.
func TestChallengeInReadsTheKindFromTheSlot(t *testing.T) {
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
		payload := map[string]any{"message": map[string]any{"contract": map[string]any{"commitments": []any{
			map[string]any{"commitmentAttributes": tc.attributes}}}}}
		if _, kind, id, _, _ := challengeIn(common.Exchange{Beckn: payload}); kind != tc.kind || id != tc.id {
			t.Errorf("challengeIn(%s) = %q, %q, want %q, %q", name, kind, id, tc.kind, tc.id)
		}
	}
	support := map[string]any{"message": map[string]any{"support": map[string]any{"orderId": "UP12345678A",
		"channels": []any{map[string]any{"enrolmentId": "BR295454592"}}}}}
	if _, kind, id, _, _ := challengeIn(common.Exchange{Beckn: support}); kind != "Ben_id" || id != "BR295454592" {
		t.Errorf("challengeIn(support) = %q, %q, want the channel's enrolmentId, not orderId", kind, id)
	}
}

func TestVerifyRequestRefusesWhatCannotBeVerified(t *testing.T) {
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
			_, err := verifyRequest(tc.exchange)
			var be model.BecknErrorer
			if !errors.As(err, &be) || be.BecknError().Code != tc.code {
				t.Fatalf("verifyRequest() = %v, want %s", err, tc.code)
			}
			want := "$.message.contract.commitments[0].commitmentAttributes." + tc.field
			if d := be.BecknError().Details; d == nil || d.Path != want {
				t.Errorf("details = %+v, want path %s", d, want)
			}
		})
	}
	fields, err := verifyRequest(withOTP("OTP", "4821"))
	if err != nil || fields["Types"] != "Ben_id" || fields["Values"] != "UP12345678A" || fields["OTP"] != "4821" {
		t.Errorf("verifyRequest() = %v, %v; want the identifier, its type and the OTP", fields, err)
	}
}
