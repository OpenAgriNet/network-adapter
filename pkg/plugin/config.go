package plugin

import (
	"fmt"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

// Settings is a plugin's own configuration block.
//
// It is map[string]string underneath, deliberately: every plugin's New takes a
// map[string]string, and a named map type stays assignable to one, so widening
// this changed no call site and no plugin contract.
//
// What it adds is ONE level of nesting on the way in. A block keyed by
// something a plugin recognises -- a participant id, for the provider steps --
// is flattened to `<field>-<blockKey>`, so
//
//	knowledge-provider:
//	  authScheme: oauth2
//	  tokenUrl: https://issuer.example/token
//
// arrives as authScheme-knowledge-provider and tokenUrl-knowledge-provider.
// The nesting is therefore a way of WRITING config, not a shape any plugin has
// to handle: by the time one is called, its settings are flat strings as
// before.
//
// This type stays deliberately ignorant of which keys mean anything. It knows
// the format, not the vocabulary -- naming valid fields here would put every
// plugin's settings in one place and make this file a dependency of all of
// them. Rejecting an unknown field is the plugin's job, where the field is
// already defined.
type Settings map[string]string

// Settings must satisfy the unmarshaler of the SAME yaml package the adapter
// decodes its config with -- gopkg.in/yaml.v2, in cmd/adapter/main.go. The two
// versions declare incompatible interfaces, and a mismatch is silent: the
// method is simply never called and a nested block reaches the plain map,
// failing with "cannot unmarshal !!map into string" at startup. This assertion
// turns that into a compile error instead.
var _ yaml.Unmarshaler = (*Settings)(nil)

// UnmarshalYAML accepts scalars at the top level and one level of nesting.
func (s *Settings) UnmarshalYAML(unmarshal func(any) error) error {
	var raw map[string]any
	if err := unmarshal(&raw); err != nil {
		return err
	}

	out := Settings{}
	// Sorted so that a duplicate is reported against the same pair whichever
	// order the map happened to iterate in.
	blocks := make([]string, 0, len(raw))
	for key := range raw {
		blocks = append(blocks, key)
	}
	sort.Strings(blocks)

	for _, key := range blocks {
		value := raw[key]

		block, nested := asBlock(value)
		if !nested {
			out[key] = scalar(value)
			continue
		}

		// A block key carrying a dash would produce a flattened key that reads
		// as another field's, so the split on the first dash could no longer
		// tell them apart. Participant ids DO carry dashes, which is why the
		// dash goes between field and key rather than inside either.
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("plugin config: a settings block has an empty name")
		}

		fields := make([]string, 0, len(block))
		for field := range block {
			fields = append(fields, field)
		}
		sort.Strings(fields)

		for _, field := range fields {
			if _, deeper := asBlock(block[field]); deeper {
				return fmt.Errorf(
					"plugin config: %s.%s nests further; only one level is supported", key, field)
			}
			if strings.Contains(field, "-") {
				return fmt.Errorf(
					"plugin config: %s.%s carries a dash; inside a block a setting is named "+
						"plainly, and the block already says which one it belongs to", key, field)
			}
			flat := field + "-" + key
			if _, taken := out[flat]; taken {
				return fmt.Errorf(
					"plugin config: %s is set twice, once as %s.%s and once directly",
					flat, key, field)
			}
			out[flat] = scalar(block[field])
		}
	}

	*s = out
	return nil
}

// asBlock recognises a nested mapping.
//
// Two shapes because yaml.v2 decodes a mapping into map[interface{}]interface{}
// unless the target says otherwise, so the top level arrives with string keys
// and anything under it does not. Normalised here rather than at each use, and
// the keys are rendered the same way values are so a non-string key cannot
// silently become a different setting.
func asBlock(v any) (map[string]any, bool) {
	switch typed := v.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[scalar(key)] = value
		}
		return out, true
	}
	return nil, false
}

// scalar renders a YAML scalar the way the flat form would have carried it.
// %v on an int gives "1048576" rather than an exponent, which a float would;
// YAML decodes an integer literal as int, so a byte count survives intact.
func scalar(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

type PublisherCfg struct {
	ID     string   `yaml:"id"`
	Config Settings `yaml:"config"`
}

type ValidatorCfg struct {
	ID     string   `yaml:"id"`
	Config Settings `yaml:"config"`
}

type Config struct {
	ID     string   `yaml:"id"`
	Config Settings `yaml:"config"`
}

type ManagerConfig struct {
	Root           string                `yaml:"root"`
	RemoteRoot     string                `yaml:"remoteRoot"`
	BecknConstants *BecknConstantsConfig `yaml:"becknConstants,omitempty"`
}

// BecknConstantsConfig controls the beckn constants loader inside the manager.
type BecknConstantsConfig struct {
	DisableRemoteRefresh bool `yaml:"disableRemoteRefresh"`
}
