package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/Grievance"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

type stubRegistry struct{}

func (stubRegistry) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	return nil, nil
}

type stubMapper struct{}

func (stubMapper) Verify(context.Context, string, any) error { return nil }

func (stubMapper) Transform(context.Context, string, definition.Direction, any) ([]byte, error) {
	return nil, nil
}

const (
	pmfbyBindingKey  = "pmfby|openagrinet:PMFBYGrievance"
	providerIDAt     = "message.contract.commitments[].offer.provider.id, message.support.channels[].provider.id"
	capabilityCodeAt = "message.contract.commitments[].commitmentAttributes.@type, message.support.channels[].@type"
)

// pmfbyConfig is the block config/provider-adapter.yaml ships, as the plugin
// receives it: flattened.
func pmfbyConfig() map[string]string {
	return map[string]string{
		"bindingKeys":              pmfbyBindingKey,
		"providerIdAt":             providerIDAt,
		"capabilityCodeAt":         capabilityCodeAt,
		"authScheme-pmfby":         "tokenHeader",
		"tokenUrl-pmfby":           "https://pmfby.example/krphapi/FGMS/NICUsersLogin",
		"tokenUserField-pmfby":     "appAccessUID",
		"tokenUserEnv-pmfby":       "PMFBY_USER",
		"tokenSecretField-pmfby":   "appAccessPWD",
		"tokenSecretEnv-pmfby":     "PMFBY_PASSWORD",
		"tokenResponseField-pmfby": "token",
		"tokenTtl-pmfby":           "10m",
		"headerName-pmfby":         "Authorization",
	}
}

func TestParseConfig_PMFBYBlock_ReadsEverySetting(t *testing.T) {
	got, err := grievanceProvider{}.parseConfig(pmfbyConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := &Grievance.Config{
		BindingKeys:      []string{pmfbyBindingKey},
		ProviderIDAt:     providerIDAt,
		CapabilityCodeAt: capabilityCodeAt,
		AuthByProvider: map[string]*common.AuthProfile{"pmfby": {
			Provider: "pmfby", Scheme: "tokenHeader", TokenURL: "https://pmfby.example/krphapi/FGMS/NICUsersLogin",
			TokenUserField: "appAccessUID", TokenUserEnv: "PMFBY_USER",
			TokenSecretName: "appAccessPWD", TokenSecretEnv: "PMFBY_PASSWORD",
			TokenResponseField: "token", TokenTTLRaw: "10m", HeaderName: "Authorization",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseConfig() = %+v, want %+v", got, want)
	}
}

func TestParseConfig_ResponseCap_ValidatesValue(t *testing.T) {
	for raw, wantErr := range map[string]string{
		"lots": "invalid maxResponseBytes value 'lots'",
		"0":    "maxResponseBytes must be positive",
		"-1":   "maxResponseBytes must be positive",
		"":     "",
		"2048": "",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := grievanceProvider{}.parseConfig(map[string]string{"maxResponseBytes": raw})
			if wantErr == "" && err != nil {
				t.Errorf("parseConfig() = %v, want no error", err)
			}
			if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
				t.Errorf("parseConfig() = %v, want %q", err, wantErr)
			}
		})
	}
}

func TestParseConfig_MisspelledAuthSetting_Refused(t *testing.T) {
	config := pmfbyConfig()
	config["tokenUrll-pmfby"] = "https://typo.example"
	if _, err := (grievanceProvider{}).parseConfig(config); err == nil {
		t.Error("parseConfig() accepted a misspelled credential setting")
	}
}

func TestSplitList_CommaSeparated_TrimsAndDropsBlanks(t *testing.T) {
	for raw, want := range map[string][]string{
		"":    nil,
		"   ": nil,
		pmfbyBindingKey + ", b|openagrinet:Two ,,": {pmfbyBindingKey, "b|openagrinet:Two"},
	} {
		if got := splitList(raw); !reflect.DeepEqual(got, want) {
			t.Errorf("splitList(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestNew_ShippedPMFBYConfig_BuildsStep(t *testing.T) {
	step, closer, err := grievanceProvider{}.New(context.Background(), stubRegistry{}, stubMapper{}, pmfbyConfig())
	if err != nil {
		t.Fatalf("New() = %v, want the shipped config to load", err)
	}
	if step == nil || closer == nil {
		t.Fatal("New() returned no step or no closer")
	}
	if err := closer(); err != nil {
		t.Errorf("closer() = %v", err)
	}
}

func TestNew_InvalidConfig_Refused(t *testing.T) {
	for name, edit := range map[string]func(map[string]string){
		"no binding keys":            func(c map[string]string) { delete(c, "bindingKeys") },
		"providerIdAt alone":         func(c map[string]string) { delete(c, "capabilityCodeAt") },
		"capabilityCodeAt alone":     func(c map[string]string) { delete(c, "providerIdAt") },
		"unpaired path lists":        func(c map[string]string) { c["capabilityCodeAt"] = "a.@type" },
		"blank listed path":          func(c map[string]string) { c["providerIdAt"] = "a.id, " },
		"no auth block for pmfby":    func(c map[string]string) { delete(c, "authScheme-pmfby") },
		"unknown auth scheme":        func(c map[string]string) { c["authScheme-pmfby"] = "loginHeader" },
		"tokenHeader without ttl":    func(c map[string]string) { delete(c, "tokenTtl-pmfby") },
		"tokenHeader without login":  func(c map[string]string) { delete(c, "tokenUrl-pmfby") },
		"tokenHeader without header": func(c map[string]string) { delete(c, "headerName-pmfby") },
		"auth for unserved provider": func(c map[string]string) { c["authScheme-pmkisan"] = "none" },
		"bad response cap":           func(c map[string]string) { c["maxResponseBytes"] = "lots" },
	} {
		t.Run(name, func(t *testing.T) {
			config := pmfbyConfig()
			edit(config)
			if _, _, err := (grievanceProvider{}).New(context.Background(), stubRegistry{}, stubMapper{}, config); err == nil {
				t.Error("New() accepted an invalid config")
			}
		})
	}
}

func TestNew_NilContext_Refused(t *testing.T) {
	//nolint:staticcheck // deliberately passing a nil context to assert the guard.
	if _, _, err := (grievanceProvider{}).New(nil, stubRegistry{}, stubMapper{}, pmfbyConfig()); err == nil {
		t.Error("New() accepted a nil context")
	}
}

func TestNew_StepConstructorFails_PropagatesError(t *testing.T) {
	original := newStepFunc
	wanted := errors.New("upstream refused the config")
	newStepFunc = func(context.Context, definition.ProviderRecordLookup, definition.Mapper,
		*Grievance.Config) (definition.Step, func() error, error) {
		return nil, nil, wanted
	}
	defer func() { newStepFunc = original }()

	if _, _, err := (grievanceProvider{}).New(context.Background(), stubRegistry{}, stubMapper{}, pmfbyConfig()); !errors.Is(err, wanted) {
		t.Errorf("New() = %v, want it to wrap %v", err, wanted)
	}
}
