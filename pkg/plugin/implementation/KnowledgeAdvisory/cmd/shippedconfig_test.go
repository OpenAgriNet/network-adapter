package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/beckn-one/beckn-onix/pkg/plugin"
)

// The shipped config and this code move together: the nested per-provider
// block is only valid if pkg/plugin flattens it and ParseAuth reads it back.
// This reads the real file rather than a copy, so the two cannot drift.
func TestShippedConfigParsesIntoAProfilePerProvider(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../../../../config/provider-adapter.yaml")
	if err != nil {
		t.Skipf("reference config unavailable: %v", err)
	}

	// Only the providerSteps entries: the rest of the file is other modules'.
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

	var steps []plugin.Config
	for _, m := range file.Modules {
		steps = append(steps, m.Handler.Plugins.ProviderSteps...)
	}
	if len(steps) == 0 {
		t.Fatal("no providerSteps found in the shipped config")
	}

	for _, step := range steps {
		t.Run(step.ID, func(t *testing.T) {
			// Flattened on the way in: a plugin never sees a block, so every
			// value here is a string. parseConfig failing would mean the
			// nesting did not survive.
			cfg, err := knowledgeAdvisoryProvider{}.parseConfig(step.Config)
			if err != nil {
				t.Fatalf("parseConfig on the shipped %s entry: %v", step.ID, err)
			}
			if len(cfg.BindingKeys) == 0 {
				t.Fatal("no bindingKeys")
			}
			// Every provider it serves must have a profile, which is what
			// New would refuse it for otherwise.
			for _, key := range cfg.BindingKeys {
				provider := key
				for i := 0; i < len(key); i++ {
					if key[i] == '|' {
						provider = key[:i]
						break
					}
				}
				profile, ok := cfg.Auth[provider]
				if !ok {
					t.Errorf("%s serves %q with no auth block", step.ID, provider)
					continue
				}
				if profile.Scheme == "" {
					t.Errorf("%s: %q has no authScheme", step.ID, provider)
				}
				t.Logf("%-20s %-22s authScheme=%s", step.ID, provider, profile.Scheme)
			}
		})
	}
}
