package plugin

import (
	"fmt"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v2"
)

// ConfigBlock is a plugin's own configuration block.
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
type ConfigBlock map[string]string

// ConfigBlock must satisfy the unmarshaler of the SAME yaml package the adapter
// decodes its config with -- gopkg.in/yaml.v2, in cmd/adapter/main.go. The two
// versions declare incompatible interfaces, and a mismatch is silent: the
// method is simply never called and a nested block reaches the plain map,
// failing with "cannot unmarshal !!map into string" at startup. This assertion
// turns that into a compile error instead.
var _ yaml.Unmarshaler = (*ConfigBlock)(nil)

// UnmarshalYAML accepts scalars at the top level and one level of nesting.
func (block *ConfigBlock) UnmarshalYAML(unmarshal func(any) error) error {
	var written map[string]any
	if err := unmarshal(&written); err != nil {
		return err
	}

	flattened := ConfigBlock{}

	// Sorted so a config with two mistakes reports the same one every run.
	names := make([]string, 0, len(written))
	for name := range written {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		nested, isNested := asNestedBlock(written[name])
		if !isNested {
			flattened[name] = asString(written[name])
			continue
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("plugin config: a nested block has no name")
		}

		settings := make([]string, 0, len(nested))
		for setting := range nested {
			settings = append(settings, setting)
		}
		sort.Strings(settings)

		for _, setting := range settings {
			if _, deeper := asNestedBlock(nested[setting]); deeper {
				return fmt.Errorf(
					"plugin config: %s.%s nests further; only one level is supported",
					name, setting)
			}
			// A setting carrying a dash would flatten to <setting>-<name>-<name>
			// and resolve to a block that does not exist. The block name
			// already says what it belongs to, so inside one a setting is
			// named plainly.
			if strings.Contains(setting, "-") {
				return fmt.Errorf(
					"plugin config: %s.%s carries a dash; inside a block a setting is named "+
						"plainly, and the block already says which one it belongs to",
					name, setting)
			}
			flatName := setting + "-" + name
			if _, alreadySet := flattened[flatName]; alreadySet {
				return fmt.Errorf(
					"plugin config: %s is set twice, once as %s.%s and once directly",
					flatName, name, setting)
			}
			flattened[flatName] = asString(nested[setting])
		}
	}

	*block = flattened
	return nil
}

// asNestedBlock recognises a nested mapping and normalises its keys.
//
// Two shapes because yaml.v2 decodes a mapping into map[interface{}]interface{}
// unless the target says otherwise, so the top level arrives with string keys
// and anything under it does not. Normalised here rather than at each use, and
// the keys are rendered the same way values are so a non-string key cannot
// silently become a different setting.
func asNestedBlock(v any) (map[string]any, bool) {
	switch typed := v.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			out[asString(key)] = value
		}
		return out, true
	}
	return nil, false
}

// scalar renders a YAML scalar the way the flat form would have carried it.
// %v on an int gives "1048576" rather than an exponent, which a float would;
// YAML decodes an integer literal as int, so a byte count survives intact.
func asString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

type PublisherCfg struct {
	ID     string      `yaml:"id"`
	Config ConfigBlock `yaml:"config"`
}

type ValidatorCfg struct {
	ID     string      `yaml:"id"`
	Config ConfigBlock `yaml:"config"`
}

type Config struct {
	ID     string      `yaml:"id"`
	Config ConfigBlock `yaml:"config"`
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
