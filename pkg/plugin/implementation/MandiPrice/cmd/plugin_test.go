package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/MandiPrice"
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
		expected    *MandiPrice.Config
		expectedErr string
	}{
		{
			// Everything absent is left zero: MandiPrice.New defaults it, so the
			// rules are defined in exactly one place.
			name:     "leaves everything unset for New to default",
			config:   map[string]string{},
			expected: &MandiPrice.Config{AuthByProvider: map[string]*upstream.AuthProfile{}},
		},
		{
			// Query auth is why this capability has its own entry rather than
			// sharing weather's: Agmarknet's Vistaar API takes its token as a
			// QUERY parameter, and these two keys are the only place that is
			// expressible.
			name: "reads the query auth scheme this capability needs",
			config: map[string]string{
				"bindingKeys":             "agmarknet|openagrinet:MandiPrice",
				"authScheme-agmarknet":    "query",
				"queryName-agmarknet":     "api-key",
				"queryValueEnv-agmarknet": "MANDI_TOKEN",
			},
			expected: &MandiPrice.Config{
				BindingKeys: []string{"agmarknet|openagrinet:MandiPrice"},
				AuthByProvider: map[string]*upstream.AuthProfile{
					"agmarknet": {
						Provider:      "agmarknet",
						Scheme:        "query",
						QueryName:     "api-key",
						QueryValueEnv: "MANDI_TOKEN",
					},
				},
			},
		},
		{
			name: "reads every supported setting",
			config: map[string]string{
				"bindingKeys":          "other|capability",
				"authScheme-other":     "basic",
				"usernameEnv-other":    "U",
				"passwordEnv-other":    "P",
				"headerName-other":     "X-Key",
				"headerValueEnv-other": "V",
				"queryName-other":      "q",
				"queryValueEnv-other":  "Q",
				"maxResponseBytes":     "2048",
			},
			expected: &MandiPrice.Config{
				BindingKeys: []string{"other|capability"},
				AuthByProvider: map[string]*upstream.AuthProfile{
					"other": {
						Provider:       "other",
						Scheme:         "basic",
						UsernameEnv:    "U",
						PasswordEnv:    "P",
						HeaderName:     "X-Key",
						HeaderValueEnv: "V",
						QueryName:      "q",
						QueryValueEnv:  "Q",
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
			// Present but empty is not the same as malformed. A rendered
			// config with an unset variable produces this, and it should read
			// as "unset" rather than failing startup.
			name:     "treats an empty response cap as unset",
			config:   map[string]string{"maxResponseBytes": ""},
			expected: &MandiPrice.Config{AuthByProvider: map[string]*upstream.AuthProfile{}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := mandiProvider{}.parseConfig(tc.config)

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

// A plugin config is map[string]string, so a list arrives comma-separated --
// the convention reqpreprocessor and schemav2validator already use. Binding keys
// separate their own halves with a pipe, so a comma is unambiguous.
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
			name: "several, with blanks and spacing a wrapped config line produces",
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

	cfg, err := mandiProvider{}.parseConfig(map[string]string{
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

// Absent leaves them empty, and upstream reads that as "use the convention".
func TestParseConfigLeavesTheOverrideUnsetByDefault(t *testing.T) {
	t.Parallel()

	cfg, err := mandiProvider{}.parseConfig(map[string]string{"bindingKeys": "a|openagrinet:One"})
	if err != nil {
		t.Fatalf("parseConfig() returned an unexpected error: %v", err)
	}
	if cfg.ProviderIDAt != "" || cfg.CapabilityCodeAt != "" {
		t.Errorf("override = %q / %q, want both empty", cfg.ProviderIDAt, cfg.CapabilityCodeAt)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("rejects a nil context", func(t *testing.T) {
		t.Parallel()

		//nolint:staticcheck // deliberately passing a nil context to assert the guard.
		_, _, err := mandiProvider{}.New(nil, stubRegistry{}, stubMapper{}, map[string]string{})
		if err == nil {
			t.Fatal("expected an error for a nil context, got none")
		}
	})

	t.Run("rejects an unparseable config", func(t *testing.T) {
		t.Parallel()

		_, _, err := mandiProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{"maxResponseBytes": "lots"})
		if err == nil {
			t.Fatal("expected an error for an invalid cap, got none")
		}
	})

	t.Run("propagates an invalid auth scheme from New", func(t *testing.T) {
		t.Parallel()

		_, _, err := mandiProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{
				"bindingKeys":          "agmarknet|openagrinet:MandiPrice",
				"authScheme-agmarknet": "oauth",
			})
		if err == nil {
			t.Fatal("expected an unknown auth scheme to be refused")
		}
	})

	// A domain plugin serves a family of capabilities, so it cannot guess which
	// of them a deployment has providers for. Refused at startup rather than
	// answering to nothing.
	t.Run("refuses a config naming no capability", func(t *testing.T) {
		t.Parallel()

		if _, _, err := (mandiProvider{}).New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{}); err == nil {
			t.Fatal("expected a config with no bindingKeys to be refused")
		}
	})

	t.Run("returns the step and its closer on a config that loads", func(t *testing.T) {
		// Not parallel: it swaps the package-level newStepFunc.
		closed := false
		original := newStepFunc
		newStepFunc = func(context.Context, definition.ProviderRecordLookup, definition.Mapper,
			*MandiPrice.Config) (definition.Step, func() error, error) {
			return nil, func() error { closed = true; return nil }, nil
		}
		defer func() { newStepFunc = original }()

		_, closer, err := mandiProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{"bindingKeys": "agmarknet|openagrinet:MandiPrice"})
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
			*MandiPrice.Config) (definition.Step, func() error, error) {
			return nil, nil, wanted
		}
		defer func() { newStepFunc = original }()

		_, _, err := mandiProvider{}.New(context.Background(), stubRegistry{}, stubMapper{},
			map[string]string{"bindingKeys": "agmarknet|openagrinet:MandiPrice"})
		if !errors.Is(err, wanted) {
			t.Errorf("New() error = %v, want it to wrap %v", err, wanted)
		}
	})
}
