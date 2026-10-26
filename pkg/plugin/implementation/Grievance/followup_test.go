package Grievance

import (
	"strings"
	"testing"
)

// statusBlock is PM-KISAN application status's provider block.
func statusBlock() map[string]string {
	return map[string]string{
		"envelope-pmkisan-status":              "aesCbcKeyInBand",
		"envelopeTokenEnv-pmkisan-status":      "TEST_PMKISAN_OTP_TOKEN",
		"envelopeTokenField-pmkisan-status":    "Token",
		"envelopeRequestField-pmkisan-status":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan-status": "d.output,output",
		"followUpPath-pmkisan-status":          "/ChatbotBeneficiaryStatus",
		"followUpField-pmkisan-status":         "_beneficiaryStatus",
		"followUpActions-pmkisan-status":       "status",
		"authScheme-pmkisan-status":            "none",
	}
}

func TestParseEnvelopesWrapsAProviderWithAFollowUp(t *testing.T) {
	parsed, rest, err := ParseEnvelopes(statusBlock())
	if err != nil {
		t.Fatalf("ParseEnvelopes() = %v", err)
	}
	if _, ok := parsed.Envelopes["pmkisan-status"].(*followUp); !ok {
		t.Errorf("envelope = %T, want the follow-up wrapping it", parsed.Envelopes["pmkisan-status"])
	}
	for key := range rest {
		if strings.HasPrefix(key, "followUp") {
			t.Errorf("%s was left for the credential parser, which refuses it", key)
		}
	}
}

func TestParseEnvelopesRefusesAFollowUpThatCannotWork(t *testing.T) {
	for name, tweak := range map[string]func(map[string]string){
		"no field":      func(c map[string]string) { delete(c, "followUpField-pmkisan-status") },
		"no actions":    func(c map[string]string) { delete(c, "followUpActions-pmkisan-status") },
		"no path":       func(c map[string]string) { delete(c, "followUpPath-pmkisan-status") },
		"a bad timeout": func(c map[string]string) { c["followUpTimeoutMs-pmkisan-status"] = "soon" },
		"no envelope to seal it": func(c map[string]string) {
			for key := range c {
				if strings.HasPrefix(key, "envelope") {
					delete(c, key)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := statusBlock()
			tweak(config)
			if _, _, err := ParseEnvelopes(config); err == nil {
				t.Error("ParseEnvelopes() accepted a follow-up that cannot work")
			}
		})
	}
}
