package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
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

func sathiConfig() map[string]string {
	return map[string]string{
		"bindingKeys":          "sathi|openagrinet:AgricultureResource",
		"authScheme-sathi":     "body",
		"bodyField-sathi":      "apiKey",
		"bodyValueEnv-sathi":   "SATHI_API_KEY",
		"answerStatuses-sathi": "409",
	}
}

func TestParseConfigReadsTheSathiProviderBlock(t *testing.T) {
	t.Parallel()

	cfg, err := Provider.parseConfig(sathiConfig())
	if err != nil {
		t.Fatalf("parseConfig() error: %v", err)
	}
	if !reflect.DeepEqual(cfg.BindingKeys, []string{"sathi|openagrinet:AgricultureResource"}) {
		t.Errorf("BindingKeys = %v", cfg.BindingKeys)
	}
	auth := cfg.AuthByProvider["sathi"]
	if auth == nil || auth.Scheme != "body" || auth.BodyField != "apiKey" ||
		auth.BodyValueEnv != "SATHI_API_KEY" || auth.AnswerStatusesRaw != "409" {
		t.Errorf("sathi profile = %+v", auth)
	}
}

func TestParseConfigRefusesAnUnknownProviderSetting(t *testing.T) {
	t.Parallel()

	config := sathiConfig()
	config["bodyFeild-sathi"] = "apiKey"
	if _, err := Provider.parseConfig(config); err == nil {
		t.Fatal("expected a misspelled provider setting to be refused")
	}
}

func TestNewBuildsTheStep(t *testing.T) {
	t.Parallel()

	step, closer, err := Provider.New(context.Background(), stubRegistry{}, stubMapper{}, sathiConfig())
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if step == nil {
		t.Fatal("New() returned no step")
	}
	if closer != nil {
		if err := closer(); err != nil {
			t.Errorf("closer() error: %v", err)
		}
	}
}

func TestNewRefusesANilContext(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // a nil context is the case under test
	if _, _, err := Provider.New(nil, stubRegistry{}, stubMapper{}, sathiConfig()); err == nil {
		t.Fatal("expected a nil context to be refused")
	}
}

func TestNewRefusesAHalfConfiguredProvider(t *testing.T) {
	t.Parallel()

	config := sathiConfig()
	delete(config, "bodyField-sathi")
	if _, _, err := Provider.New(context.Background(), stubRegistry{}, stubMapper{}, config); err == nil {
		t.Fatal("expected a body scheme with no bodyField to be refused")
	}
}
