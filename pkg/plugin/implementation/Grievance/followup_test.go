package Grievance

import (
	"strings"
	"testing"
)

// statusBlock is PM-KISAN application status's provider block.
func statusBlock() map[string]string {
	return map[string]string{
		"envelope-pmkisan|openagrinet:SchemeApplication":              "aesCbcKeyInBand",
		"envelopeTokenEnv-pmkisan|openagrinet:SchemeApplication":      "TEST_PMKISAN_OTP_TOKEN",
		"envelopeTokenField-pmkisan|openagrinet:SchemeApplication":    "Token",
		"envelopeRequestField-pmkisan|openagrinet:SchemeApplication":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan|openagrinet:SchemeApplication": "d.output,output",
		"followUpPath-pmkisan|openagrinet:SchemeApplication":          "/ChatbotBeneficiaryStatus",
		"followUpField-pmkisan|openagrinet:SchemeApplication":         "_beneficiaryStatus",
		"followUpActions-pmkisan|openagrinet:SchemeApplication":       "status",
		"authScheme-pmkisan": "none",
	}
}

func TestParseEnvelopesWrapsAProviderWithAFollowUp(t *testing.T) {
	parsed, rest, err := ParseEnvelopes(statusBlock())
	if err != nil {
		t.Fatalf("ParseEnvelopes() = %v", err)
	}
	if _, ok := parsed.Envelopes["pmkisan|openagrinet:SchemeApplication"].(*followUp); !ok {
		t.Errorf("envelope = %T, want the follow-up wrapping it", parsed.Envelopes["pmkisan|openagrinet:SchemeApplication"])
	}
	for key := range rest {
		if strings.HasPrefix(key, "followUp") {
			t.Errorf("%s was left for the credential parser, which refuses it", key)
		}
	}
}

func TestParseEnvelopesRefusesAFollowUpThatCannotWork(t *testing.T) {
	for name, tweak := range map[string]func(map[string]string){
		"no field":      func(c map[string]string) { delete(c, "followUpField-pmkisan|openagrinet:SchemeApplication") },
		"no actions":    func(c map[string]string) { delete(c, "followUpActions-pmkisan|openagrinet:SchemeApplication") },
		"no path":       func(c map[string]string) { delete(c, "followUpPath-pmkisan|openagrinet:SchemeApplication") },
		"a bad timeout": func(c map[string]string) { c["followUpTimeoutMs-pmkisan|openagrinet:SchemeApplication"] = "soon" },
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
