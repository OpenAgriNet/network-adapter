package Grievance

import (
	"reflect"
	"strings"
	"testing"
)

const (
	grievanceKey   = "pmkisan|openagrinet:Grievance"
	applicationKey = "pmkisan|openagrinet:SchemeApplication"
)

// grievanceRow and applicationRow are the encryption blocks register-provider
// writes from quick-start's config/registry/providers/pmkisan.json.
func grievanceRow() map[string]any {
	return map[string]any{
		"type": "aesGcm",
		"aesGcm": map[string]any{
			"serviceTokenRef": "PMKISAN_SERVICE_TOKEN", "serviceTokenField": "TokenNo",
			"encryptedRequestField": "EncryptedRequest", "encryptedResponsePaths": "d.output,output",
			"aesKeyRef": "PMKISAN_GRIEVANCE_KEY", "aesNonceRef": "PMKISAN_GRIEVANCE_IV",
		},
		"otp": map[string]any{
			"type": "aesCbcKeyInBand", "bindingKey": applicationKey,
			"serviceTokenRef": "PMKISAN_OTP_TOKEN", "serviceTokenField": "Token",
			"encryptedRequestField": "EncryptedRequest", "encryptedResponsePaths": "d.output,output",
			"actions": "init",
		},
		"otpVerify": map[string]any{"path": "/ChatbotOTPVerified", "actions": "support,status", "timeoutMs": float64(10000)},
	}
}

func applicationRow() map[string]any {
	return map[string]any{
		"type": "aesCbcKeyInBand",
		"aesCbcKeyInBand": map[string]any{
			"serviceTokenRef": "PMKISAN_OTP_TOKEN", "serviceTokenField": "Token",
			"encryptedRequestField": "EncryptedRequest", "encryptedResponsePaths": "d.output,output",
		},
		"otpVerify": map[string]any{"path": "/ChatbotOTPVerified", "actions": "status", "timeoutMs": float64(10000)},
		"followUp": map[string]any{"path": "/ChatbotBeneficiaryStatus", "answerField": "_beneficiaryStatus",
			"actions": "status", "timeoutMs": float64(10000)},
	}
}

// suffixed is a config block as quick-start's pmkisan.yaml.tmpl writes it,
// flattened the way pkg/plugin hands it to the plugin.
func suffixed(key string, settings map[string]string) map[string]string {
	out := make(map[string]string, len(settings))
	for name, value := range settings {
		out[name+"-"+key] = value
	}
	return out
}

func TestEncryptionKeysMatchTheConfigForTheGrievanceRow(t *testing.T) {
	t.Parallel()

	got, err := encryptionKeys(grievanceKey, grievanceRow())
	if err != nil {
		t.Fatalf("encryptionKeys() = %v", err)
	}
	want := suffixed(grievanceKey, map[string]string{
		"envelope": "aesGcm", "envelopeKeyEnv": "PMKISAN_GRIEVANCE_KEY", "envelopeNonceEnv": "PMKISAN_GRIEVANCE_IV",
		"envelopeTokenEnv": "PMKISAN_SERVICE_TOKEN", "envelopeTokenField": "TokenNo",
		"envelopeRequestField": "EncryptedRequest", "envelopeResponsePaths": "d.output,output",
		"otpEnvelope": "aesCbcKeyInBand", "otpBindingKey": applicationKey, "otpTokenEnv": "PMKISAN_OTP_TOKEN",
		"otpTokenField": "Token", "otpRequestField": "EncryptedRequest", "otpResponsePaths": "d.output,output",
		"otpActions": "init", "otpVerifyPath": "/ChatbotOTPVerified", "otpVerifyActions": "support,status",
		"otpVerifyTimeoutMs": "10000",
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v\nwant %v", got, want)
	}
}

func TestEncryptionKeysMatchTheConfigForTheApplicationRow(t *testing.T) {
	t.Parallel()

	got, err := encryptionKeys(applicationKey, applicationRow())
	if err != nil {
		t.Fatalf("encryptionKeys() = %v", err)
	}
	want := suffixed(applicationKey, map[string]string{
		"envelope": "aesCbcKeyInBand", "envelopeTokenEnv": "PMKISAN_OTP_TOKEN", "envelopeTokenField": "Token",
		"envelopeRequestField": "EncryptedRequest", "envelopeResponsePaths": "d.output,output",
		"otpVerifyPath": "/ChatbotOTPVerified", "otpVerifyActions": "status", "otpVerifyTimeoutMs": "10000",
		"followUpPath": "/ChatbotBeneficiaryStatus", "followUpField": "_beneficiaryStatus",
		"followUpActions": "status", "followUpTimeoutMs": "10000",
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v\nwant %v", got, want)
	}
}

func TestEncryptionKeysIgnoreTheCipherBlockTypeDoesNotName(t *testing.T) {
	t.Parallel()

	row := applicationRow()
	row["aesGcm"] = map[string]any{"aesKeyRef": "LEFT_OVER"}
	got, err := encryptionKeys(applicationKey, row)
	if err != nil {
		t.Fatalf("encryptionKeys() = %v", err)
	}
	if _, kept := got["envelopeKeyEnv-"+applicationKey]; kept {
		t.Error("a leftover aesGcm block was read for an aesCbcKeyInBand row")
	}
}

func TestEncryptionKeysRefuseWhatTheyDoNotKnow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		change  func(map[string]any)
		mention string
	}{
		{"no type", func(r map[string]any) { delete(r, "type") }, "type"},
		{"unknown block", func(r map[string]any) { r["otpp"] = map[string]any{} }, "otpp"},
		{"misspelled field", func(r map[string]any) { r["aesGcm"].(map[string]any)["aesKeyRf"] = "X" }, "aesKeyRf"},
		{"block that is not an object", func(r map[string]any) { r["otpVerify"] = "/ChatbotOTPVerified" }, "otpVerify"},
		{"value that is neither text nor a number", func(r map[string]any) { r["otpVerify"].(map[string]any)["actions"] = true }, "actions"},
		{"fractional number", func(r map[string]any) { r["otpVerify"].(map[string]any)["timeoutMs"] = 1.5 }, "timeoutMs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			row := grievanceRow()
			tt.change(row)
			_, err := encryptionKeys(grievanceKey, row)
			if err == nil {
				t.Fatal("encryptionKeys() accepted a bad encryption block")
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %q does not mention %q", err, tt.mention)
			}
		})
	}
}

func TestEncryptionFromRowBuildsTheGrievanceEnvelopeAndItsOTPHost(t *testing.T) {
	t.Parallel()

	envelope, hosted, err := EncryptionFromRow(grievanceKey, grievanceRow())
	if err != nil {
		t.Fatalf("EncryptionFromRow() = %v", err)
	}
	if envelope == nil {
		t.Fatal("no envelope built for the grievance row")
	}
	if want := map[string]string{"init": applicationKey}; !reflect.DeepEqual(hosted, want) {
		t.Errorf("hosted elsewhere = %v, want %v", hosted, want)
	}
}

func TestEncryptionFromRowBuildsTheApplicationEnvelopeWithItsFollowUp(t *testing.T) {
	t.Parallel()

	envelope, hosted, err := EncryptionFromRow(applicationKey, applicationRow())
	if err != nil {
		t.Fatalf("EncryptionFromRow() = %v", err)
	}
	if _, wrapped := envelope.(*followUp); !wrapped {
		t.Errorf("envelope is %T, want it wrapped with the follow-up read", envelope)
	}
	if len(hosted) != 0 {
		t.Errorf("hosted elsewhere = %v, want none", hosted)
	}
}

func TestEncryptionFromRowRefusesWhatTheParserRefuses(t *testing.T) {
	t.Parallel()

	row := grievanceRow()
	delete(row, "aesGcm")
	if _, _, err := EncryptionFromRow(grievanceKey, row); err == nil {
		t.Error("EncryptionFromRow() accepted an aesGcm row with no aesGcm block")
	}
}
