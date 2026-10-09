package main

import (
	"context"
	"os"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v2"

	"github.com/beckn-one/beckn-onix/pkg/plugin"
)

const pmkisanBindingKey = "pmkisan|openagrinet:PMKISANGrievance"

// bothSchemesConfig is PMFBY's block plus PM-KISAN's, flattened as the plugin
// receives them: one step serving both schemes.
func bothSchemesConfig() map[string]string {
	config := pmfbyConfig()
	config["bindingKeys"] = pmfbyBindingKey + "," + pmkisanBindingKey
	for key, value := range map[string]string{
		"fallbackProviderIdAt":          "message.support.channels[].provider.id",
		"fallbackCapabilityCodeAt":      "message.support.channels[].@type",
		"authScheme-pmkisan":            "none",
		"envelope-pmkisan":              "aesGcm",
		"envelopeKeyEnv-pmkisan":        "PMKISAN_GRIEVANCE_KEY",
		"envelopeNonceEnv-pmkisan":      "PMKISAN_GRIEVANCE_NONCE",
		"envelopeTokenEnv-pmkisan":      "PMKISAN_SERVICE_TOKEN",
		"envelopeTokenField-pmkisan":    "TokenNo",
		"envelopeRequestField-pmkisan":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan": "d.output,output",
		"otpEnvelope-pmkisan":           "aesCbcKeyInBand",
		"otpTokenEnv-pmkisan":           "PMKISAN_OTP_TOKEN",
		"otpTokenField-pmkisan":         "Token",
		"otpRequestField-pmkisan":       "EncryptedRequest",
		"otpResponsePaths-pmkisan":      "d.output,output",
		"otpActions-pmkisan":            "init",
		"otpVerifyPath-pmkisan":         "/ChatbotOTPVerified",
		"otpVerifyActions-pmkisan":      "support,status",
	} {
		config[key] = value
	}
	return config
}

func TestParseConfig_PMKISANBlock_ReadsEnvelope(t *testing.T) {
	cfg, err := grievanceProvider{}.parseConfig(bothSchemesConfig())
	if err != nil {
		t.Fatalf("parseConfig() = %v", err)
	}
	if len(cfg.BindingKeys) != 2 {
		t.Errorf("BindingKeys = %v, want both schemes", cfg.BindingKeys)
	}
	if _, ok := cfg.EnvelopeByProvider["pmkisan"]; !ok || len(cfg.EnvelopeByProvider) != 1 {
		t.Errorf("EnvelopeByProvider = %v, want pmkisan's alone -- PMFBY speaks plain JSON", cfg.EnvelopeByProvider)
	}
	if len(cfg.AuthByProvider) != 2 || cfg.AuthByProvider["pmkisan"].Scheme != "none" {
		t.Errorf("AuthByProvider = %v, want pmfby and pmkisan", cfg.AuthByProvider)
	}
}

func TestNew_BothSchemes_BuildsStep(t *testing.T) {
	step, closer, err := grievanceProvider{}.New(context.Background(), stubRegistry{}, stubMapper{}, bothSchemesConfig())
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if step == nil {
		t.Fatal("New() returned no step")
	}
	_ = closer()
}

func TestParseConfig_IncompleteOTPRealm_Refused(t *testing.T) {
	config := bothSchemesConfig()
	delete(config, "otpVerifyActions-pmkisan")
	if _, err := (grievanceProvider{}).parseConfig(config); err == nil ||
		!strings.Contains(err.Error(), "otpVerifyActions") {
		t.Fatalf("parseConfig() = %v, want the half-set verify named", err)
	}
}

func TestParseConfig_IncompleteEnvelope_Refused(t *testing.T) {
	config := bothSchemesConfig()
	delete(config, "envelopeKeyEnv-pmkisan")
	if _, err := (grievanceProvider{}).parseConfig(config); err == nil ||
		!strings.Contains(err.Error(), "envelopeKeyEnv") {
		t.Fatalf("parseConfig() = %v, want the missing envelopeKeyEnv named", err)
	}
}

func TestNew_EnvelopeForAnUnservedProvider_Refused(t *testing.T) {
	config := bothSchemesConfig()
	config["bindingKeys"] = pmfbyBindingKey
	delete(config, "authScheme-pmkisan")
	if _, _, err := (grievanceProvider{}).New(context.Background(), stubRegistry{}, stubMapper{}, config); err == nil {
		t.Fatal("New() accepted an envelope for a provider the step does not serve")
	}
}

// The shipped config and this code move together: the fallback paths and the
// envelope settings are only valid if pkg/plugin flattens them and
// parseConfig reads them back. This reads the real file rather than a copy.
//
// Decoded with yaml.v2, the version cmd/adapter/main.go uses: a test on v3
// passes while the adapter refuses the same file.
func TestShippedConfig_GrievanceEntry_BuildsStep(t *testing.T) {
	raw, err := os.ReadFile("../../../../../config/provider-adapter.yaml")
	if err != nil {
		t.Skipf("reference config unavailable: %v", err)
	}
	var file struct {
		Modules []struct {
			Handler struct {
				Plugins struct {
					ProviderSteps []plugin.Config `yaml:"providerSteps"`
				} `yaml:"plugins"`
			} `yaml:"handler"`
		} `yaml:"modules"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("the shipped config does not decode: %v", err)
	}

	var shipped map[string]string
	for _, m := range file.Modules {
		for _, step := range m.Handler.Plugins.ProviderSteps {
			if step.ID == "Grievance" {
				shipped = step.Config
			}
		}
	}
	if shipped == nil {
		t.Fatal("the shipped config has no Grievance entry")
	}

	cfg, err := grievanceProvider{}.parseConfig(shipped)
	if err != nil {
		t.Fatalf("parseConfig on the shipped entry: %v", err)
	}
	if cfg.FallbackProviderIDAt == "" || cfg.FallbackCapabilityCodeAt == "" {
		t.Error("the shipped entry gives support no fallback paths; every PM-KISAN lodge would pass through unserved")
	}
	if _, ok := cfg.EnvelopeByProvider["pmkisan"]; !ok {
		t.Error("the shipped entry gives pmkisan no envelope; the portal would be sent plaintext")
	}
	if _, _, err := (grievanceProvider{}).New(context.Background(), stubRegistry{}, stubMapper{}, shipped); err != nil {
		t.Fatalf("the shipped entry does not build a step: %v", err)
	}
}
