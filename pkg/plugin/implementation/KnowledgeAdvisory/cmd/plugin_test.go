package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/KnowledgeAdvisory"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/upstream"
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

func TestParseConfig(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		config      map[string]string
		expected    *KnowledgeAdvisory.Config
		expectedErr string
	}{
		{
			// Everything absent is left zero: KnowledgeAdvisory.New defaults
			// it, so the rules are defined in exactly one place. Auth is an
			// empty map rather than nil -- ParseAuth always returns one.
			name:     "leaves everything unset for New to default",
			config:   map[string]string{},
			expected: &KnowledgeAdvisory.Config{Auth: map[string]*upstream.Auth{}},
		},
		{
			// The scheme this capability exists to use. Its provider sits
			// behind an OAuth2 token endpoint issuing ten-hour tokens, so a
			// static value in an environment variable is wrong twice a day.
			name: "reads the oauth2 settings this capability needs",
			config: map[string]string{
				"bindingKeys":                        "knowledge-provider|openagrinet:KnowledgeAdvisory",
				"authScheme-knowledge-provider":      "oauth2",
				"tokenUrl-knowledge-provider":        "https://issuer.invalid/token",
				"clientIdEnv-knowledge-provider":     "KNOWLEDGE_CLIENT_ID",
				"clientSecretEnv-knowledge-provider": "KNOWLEDGE_CLIENT_SECRET",
			},
			expected: &KnowledgeAdvisory.Config{
				BindingKeys: []string{"knowledge-provider|openagrinet:KnowledgeAdvisory"},
				Auth: map[string]*upstream.Auth{
					"knowledge-provider": {
						Provider:        "knowledge-provider",
						Scheme:          "oauth2",
						TokenURL:        "https://issuer.invalid/token",
						ClientIDEnv:     "KNOWLEDGE_CLIENT_ID",
						ClientSecretEnv: "KNOWLEDGE_CLIENT_SECRET",
					},
				},
			},
		},
		{
			// TWO PROVIDERS, DIFFERENT SCHEMES -- the reason this shape exists.
			// One step serves both binding keys, and each authenticates as
			// itself.
			name: "reads a profile per provider",
			config: map[string]string{
				"bindingKeys": "knowledge-provider|openagrinet:KnowledgeAdvisory," +
					"vistaar-two|openagrinet:KnowledgeAdvisory",
				"authScheme-knowledge-provider":      "oauth2",
				"tokenUrl-knowledge-provider":        "https://issuer.invalid/token",
				"clientIdEnv-knowledge-provider":     "ID",
				"clientSecretEnv-knowledge-provider": "SECRET",
				"authScheme-vistaar-two":             "query",
				"queryName-vistaar-two":              "token",
				"queryValueEnv-vistaar-two":          "VISTAAR_TWO_TOKEN",
			},
			expected: &KnowledgeAdvisory.Config{
				BindingKeys: []string{
					"knowledge-provider|openagrinet:KnowledgeAdvisory",
					"vistaar-two|openagrinet:KnowledgeAdvisory",
				},
				Auth: map[string]*upstream.Auth{
					"knowledge-provider": {
						Provider:        "knowledge-provider",
						Scheme:          "oauth2",
						TokenURL:        "https://issuer.invalid/token",
						ClientIDEnv:     "ID",
						ClientSecretEnv: "SECRET",
					},
					"vistaar-two": {
						Provider:      "vistaar-two",
						Scheme:        "query",
						QueryName:     "token",
						QueryValueEnv: "VISTAAR_TWO_TOKEN",
					},
				},
			},
		},
		{
			// A participant id is hostname-shaped, so dots are legal. The
			// field name in front is what the split anchors on.
			name: "reads a dotted participant id",
			config: map[string]string{
				"bindingKeys":                  "provider.oan.dev|openagrinet:KnowledgeAdvisory",
				"authScheme-provider.oan.dev":  "basic",
				"usernameEnv-provider.oan.dev": "P_USER",
				"passwordEnv-provider.oan.dev": "P_PASS",
			},
			expected: &KnowledgeAdvisory.Config{
				BindingKeys: []string{"provider.oan.dev|openagrinet:KnowledgeAdvisory"},
				Auth: map[string]*upstream.Auth{
					"provider.oan.dev": {
						Provider:    "provider.oan.dev",
						Scheme:      "basic",
						UsernameEnv: "P_USER",
						PasswordEnv: "P_PASS",
					},
				},
			},
		},
		{
			// The old step-wide spelling. Refused rather than ignored: dropping
			// it silently leaves every provider on no credential at all, which
			// reads as the provider rejecting us.
			name: "refuses a step-wide authScheme",
			config: map[string]string{
				"bindingKeys": "knowledge-provider|openagrinet:KnowledgeAdvisory",
				"authScheme":  "oauth2",
			},
			expectedErr: "auth is per provider now",
		},
		{
			name: "refuses a misspelled credential setting",
			config: map[string]string{
				"bindingKeys":                    "knowledge-provider|openagrinet:KnowledgeAdvisory",
				"authScheeme-knowledge-provider": "oauth2",
			},
			expectedErr: "is not a credential setting",
		},
		{
			name: "refuses a setting that names no provider",
			config: map[string]string{
				"bindingKeys": "knowledge-provider|openagrinet:KnowledgeAdvisory",
				"authScheme-": "oauth2",
			},
			expectedErr: "names no provider",
		},
		{
			name: "reads every supported setting",
			config: map[string]string{
				"bindingKeys":           "other|capability",
				"authScheme-other":      "basic",
				"usernameEnv-other":     "U",
				"passwordEnv-other":     "P",
				"headerName-other":      "X-Key",
				"headerValueEnv-other":  "V",
				"queryName-other":       "q",
				"queryValueEnv-other":   "Q",
				"tokenUrl-other":        "https://issuer.invalid/token",
				"clientIdEnv-other":     "ID",
				"clientSecretEnv-other": "SECRET",
				"maxResponseBytes":      "2048",
			},
			expected: &KnowledgeAdvisory.Config{
				BindingKeys: []string{"other|capability"},
				Auth: map[string]*upstream.Auth{
					"other": {
						Provider:        "other",
						Scheme:          "basic",
						UsernameEnv:     "U",
						PasswordEnv:     "P",
						HeaderName:      "X-Key",
						HeaderValueEnv:  "V",
						QueryName:       "q",
						QueryValueEnv:   "Q",
						TokenURL:        "https://issuer.invalid/token",
						ClientIDEnv:     "ID",
						ClientSecretEnv: "SECRET",
					},
				},
				MaxResponseBytes: 2048,
			},
		},
		{
			name:        "rejects a malformed response cap",
			config:      map[string]string{"maxResponseBytes": "lots"},
			expectedErr: "invalid maxResponseBytes value 'lots'",
		},
		{
			name:        "rejects a non-positive response cap",
			config:      map[string]string{"maxResponseBytes": "0"},
			expectedErr: "maxResponseBytes must be positive",
		},
		{
			// Present but empty is not malformed. A rendered config with an
			// unset variable produces this, and it should read as "unset".
			name:     "treats an empty response cap as unset",
			config:   map[string]string{"maxResponseBytes": ""},
			expected: &KnowledgeAdvisory.Config{Auth: map[string]*upstream.Auth{}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := knowledgeAdvisoryProvider{}.parseConfig(tc.config)

			if tc.expectedErr != "" {
				if err == nil {
					t.Fatalf("expected error %q but got none", tc.expectedErr)
				}
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Errorf("expected error containing %q, got %q", tc.expectedErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if !reflect.DeepEqual(got, tc.expected) {
				t.Errorf("expected config %+v, got %+v", tc.expected, got)
			}
		})
	}
}

// A plugin config is map[string]string, so a list arrives comma-separated.
// Binding keys separate their own halves with a pipe, so a comma is
// unambiguous.
func TestSplitList(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty is nil, not a one-element list of nothing", raw: "", want: nil},
		{name: "whitespace only is nil", raw: "   ", want: nil},
		{name: "one value", raw: "a|openagrinet:One", want: []string{"a|openagrinet:One"}},
		{
			name: "several, with the blanks and spacing a wrapped config line produces",
			raw:  "a|openagrinet:One, b|openagrinet:Two ,,  ",
			want: []string{"a|openagrinet:One", "b|openagrinet:Two"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := splitList(tc.raw); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitList(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// The override is two keys, both or neither. Absent leaves the step on the
// Beckn v2 convention, which is what every deployment should be running.
func TestParseConfigReadsTheBindingKeyOverride(t *testing.T) {
	t.Parallel()

	cfg, err := knowledgeAdvisoryProvider{}.parseConfig(map[string]string{
		"bindingKeys":      "a|openagrinet:One",
		"providerIdAt":     "who.provider",
		"capabilityCodeAt": "what[].type",
	})
	if err != nil {
		t.Fatalf("parseConfig() returned an unexpected error: %v", err)
	}
	if cfg.ProviderIDAt != "who.provider" || cfg.CapabilityCodeAt != "what[].type" {
		t.Errorf("override = %q / %q, want the configured paths", cfg.ProviderIDAt, cfg.CapabilityCodeAt)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("rejects a nil context", func(t *testing.T) {
		t.Parallel()

		//nolint:staticcheck // deliberately passing a nil context to assert the guard.
		_, _, err := knowledgeAdvisoryProvider{}.New(nil, stubRegistry{}, stubMapper{}, map[string]string{})
		if err == nil {
			t.Fatal("expected an error for a nil context, got none")
		}
	})

	t.Run("rejects an unparseable config", func(t *testing.T) {
		t.Parallel()

		_, _, err := knowledgeAdvisoryProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{"maxResponseBytes": "lots"})
		if err == nil {
			t.Fatal("expected an error for an invalid cap, got none")
		}
	})

	// oauth2 without its three settings cannot authenticate, so it is refused
	// at startup rather than on the first request.
	t.Run("refuses oauth2 with no token url", func(t *testing.T) {
		t.Parallel()

		_, _, err := knowledgeAdvisoryProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{
				"bindingKeys":     "a|openagrinet:One",
				"authScheme":      "oauth2",
				"clientIdEnv":     "ID",
				"clientSecretEnv": "SECRET",
			})
		if err == nil {
			t.Fatal("expected oauth2 without tokenUrl to be refused")
		}
	})

	// A domain plugin serves a family of capabilities, so it cannot guess which
	// of them a deployment has providers for.
	t.Run("refuses a config naming no capability", func(t *testing.T) {
		t.Parallel()

		if _, _, err := (knowledgeAdvisoryProvider{}).New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{}); err == nil {
			t.Fatal("expected a config with no bindingKeys to be refused")
		}
	})

	t.Run("returns the step and its closer on a config that loads", func(t *testing.T) {
		closed := false
		original := newStepFunc
		newStepFunc = func(context.Context, definition.ProviderRecordLookup, definition.Mapper,
			*KnowledgeAdvisory.Config) (definition.Step, func() error, error) {
			return nil, func() error { closed = true; return nil }, nil
		}
		defer func() { newStepFunc = original }()

		_, closer, err := knowledgeAdvisoryProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{"bindingKeys": "knowledge-provider|openagrinet:KnowledgeAdvisory"})
		if err != nil {
			t.Fatalf("New() returned an unexpected error: %v", err)
		}
		if closer == nil {
			t.Fatal("New() returned no closer, so nothing can release the step")
		}
		if err := closer(); err != nil {
			t.Errorf("closer() returned %v, want nil", err)
		}
		if !closed {
			t.Error("closer() did not reach the step's own closer")
		}
	})

	t.Run("propagates a failure from the step constructor", func(t *testing.T) {
		original := newStepFunc
		wanted := errors.New("upstream refused the config")
		newStepFunc = func(context.Context, definition.ProviderRecordLookup, definition.Mapper,
			*KnowledgeAdvisory.Config) (definition.Step, func() error, error) {
			return nil, nil, wanted
		}
		defer func() { newStepFunc = original }()

		_, _, err := knowledgeAdvisoryProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{"bindingKeys": "knowledge-provider|openagrinet:KnowledgeAdvisory"})
		if !errors.Is(err, wanted) {
			t.Errorf("New() error = %v, want it to wrap %v", err, wanted)
		}
	})
}
