package plugin

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decode is the path a real config takes: YAML -> Config.Config.
func decode(t *testing.T, body string) (Settings, error) {
	t.Helper()
	var c Config
	err := yaml.Unmarshal([]byte("id: X\nconfig:\n"+body), &c)
	return c.Config, err
}

// The flat form is what every shipped config uses, and it has to survive
// untouched -- this type was introduced to ADD nesting, not to change what
// already worked.
func TestSettingsKeepsTheFlatFormUntouched(t *testing.T) {
	got, err := decode(t, `
  bindingKeys: "a|cap,b|cap"
  authScheme: oauth2
  clientIdEnv: KNOWLEDGE_CLIENT_ID
`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := Settings{
		"bindingKeys": "a|cap,b|cap",
		"authScheme":  "oauth2",
		"clientIdEnv": "KNOWLEDGE_CLIENT_ID",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// One level of nesting flattens to <field>-<blockKey>, which is what turns a
// per-provider block into settings a plugin can read without knowing YAML.
func TestSettingsFlattensOneLevelOfNesting(t *testing.T) {
	got, err := decode(t, `
  bindingKeys: "knowledge-provider|cap,vistaar-two|cap"
  knowledge-provider:
    authScheme: oauth2
    tokenUrl: "https://issuer.example/token"
  vistaar-two:
    authScheme: query
    queryValueEnv: VISTAAR_TWO_TOKEN
`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for key, want := range map[string]string{
		"authScheme-knowledge-provider": "oauth2",
		"tokenUrl-knowledge-provider":   "https://issuer.example/token",
		"authScheme-vistaar-two":        "query",
		"queryValueEnv-vistaar-two":     "VISTAAR_TWO_TOKEN",
		"bindingKeys":                   "knowledge-provider|cap,vistaar-two|cap",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	// The block names themselves must not survive as keys.
	for _, gone := range []string{"knowledge-provider", "vistaar-two"} {
		if _, present := got[gone]; present {
			t.Errorf("block name %q leaked through as a key", gone)
		}
	}
}

// A participant id is hostname-shaped, so dots are legal and must not confuse
// the flattening -- the dash between field and key is the only separator.
func TestSettingsHandlesADottedBlockName(t *testing.T) {
	got, err := decode(t, `
  provider.oan.dev:
    authScheme: basic
    usernameEnv: P_USER
`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["authScheme-provider.oan.dev"] != "basic" {
		t.Errorf("dotted block name did not flatten: %v", got)
	}
	if got["usernameEnv-provider.oan.dev"] != "P_USER" {
		t.Errorf("dotted block name did not flatten: %v", got)
	}
}

// An integer must arrive as its digits. A float formatting would turn a byte
// count into 1.048576e+06 and the plugin's ParseInt would reject it.
func TestSettingsRendersAnIntegerWithoutAnExponent(t *testing.T) {
	got, err := decode(t, "  maxResponseBytes: 1048576\n  enabled: true\n")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["maxResponseBytes"] != "1048576" {
		t.Errorf("maxResponseBytes = %q, want 1048576", got["maxResponseBytes"])
	}
	if got["enabled"] != "true" {
		t.Errorf("enabled = %q, want true", got["enabled"])
	}
}

// Two levels have no flat spelling, so they are refused rather than dropped:
// silently discarding a setting an operator wrote is the worse failure.
func TestSettingsRefusesDeeperNesting(t *testing.T) {
	_, err := decode(t, `
  knowledge-provider:
    retry:
      attempts: 3
`)
	if err == nil {
		t.Fatal("two levels of nesting was accepted")
	}
	if !strings.Contains(err.Error(), "only one level") {
		t.Errorf("error %q should say one level is supported", err)
	}
}

// Inside a block the id is already said by the block name. A dashed field
// would flatten to <field>-<id>-<id>, resolving to a provider that does not
// exist, so it is refused with the reason rather than accepted and lost.
func TestSettingsRefusesADashedFieldInsideABlock(t *testing.T) {
	_, err := decode(t, `
  vistaar-two:
    authScheme-vistaar-two: query
`)
	if err == nil {
		t.Fatal("a dashed field inside a block was accepted")
	}
	if !strings.Contains(err.Error(), "dash") {
		t.Errorf("error %q should name the dash", err)
	}
}

// The same setting written both ways is a contradiction with no right answer,
// so neither wins: it is reported.
func TestSettingsRefusesASettingGivenTwice(t *testing.T) {
	_, err := decode(t, `
  authScheme-vistaar-two: oauth2
  vistaar-two:
    authScheme: query
`)
	if err == nil {
		t.Fatal("the same setting given twice was accepted")
	}
	if !strings.Contains(err.Error(), "set twice") {
		t.Errorf("error %q should say it is set twice", err)
	}
}

// An absent config block is not an error: most plugins take none.
func TestSettingsAcceptsNoConfigAtAll(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("id: X\n"), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(c.Config) != 0 {
		t.Errorf("Config = %v, want empty", c.Config)
	}
}

// Settings must stay usable exactly where map[string]string was, because every
// plugin's New takes one and none of them changed.
func TestSettingsPassesWhereAPlainMapIsWanted(t *testing.T) {
	got, err := decode(t, "  authScheme: oauth2\n")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	takesPlainMap := func(m map[string]string) string { return m["authScheme"] }
	if takesPlainMap(got) != "oauth2" {
		t.Error("Settings no longer passes as a map[string]string")
	}
	var assigned map[string]string = got
	if assigned["authScheme"] != "oauth2" {
		t.Error("Settings no longer assigns to a map[string]string")
	}
}
