package agmarket

// inputs_test.go pins resolveInputs' precedence and its rendering, both of
// which are silent when they go wrong: a default that quietly beats a set env
// var runs the pipeline against the wrong host, and a non-string default
// rendered with the wrong verb reaches the upstream as "%!s(int=7)" rather
// than "7". The last test runs the real declared inputs through it, so this
// stays wired to mandi-price-agmarket.yaml rather than to a fixture that can
// drift away from it.

import (
	"strings"
	"testing"
)

// lookupFrom makes an os.LookupEnv-shaped function out of a map, so a test
// states the environment it means instead of mutating the process's.
func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestResolveInputsEnvBeatsDefault(t *testing.T) {
	inputs := map[string]Input{
		"baseUrl": {Env: "MANDI_API_URI", Default: "http://default:8080"},
	}

	got, err := resolveInputs(inputs, lookupFrom(map[string]string{
		"MANDI_API_URI": "http://set-by-env:9090",
	}))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if want := "http://set-by-env:9090"; got["baseUrl"] != want {
		t.Errorf("baseUrl = %q, want %q -- a set env var must beat the default", got["baseUrl"], want)
	}
}

// An env var that is present but empty is the shape a misconfigured
// deployment takes (`MANDI_API_URI=` in a compose file), and treating it as a
// deliberate empty override would point the run at nothing at all. It falls
// back to the default instead.
func TestResolveInputsDefaultWhenEnvAbsentOrEmpty(t *testing.T) {
	inputs := map[string]Input{
		"baseUrl": {Env: "MANDI_API_URI", Default: "http://default:8080"},
	}

	for name, env := range map[string]map[string]string{
		"absent": {},
		"empty":  {"MANDI_API_URI": ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolveInputs(inputs, lookupFrom(env))
			if err != nil {
				t.Fatalf("resolveInputs: %v", err)
			}
			if want := "http://default:8080"; got["baseUrl"] != want {
				t.Errorf("baseUrl = %q, want %q", got["baseUrl"], want)
			}
		})
	}
}

// Default is `interface{}` because YAML scalars are not all strings. A %s on
// an int yields "%!s(int=256)", which is not a value any upstream understands
// and is not obviously wrong when read in a log line.
func TestResolveInputsRendersNonStringDefaults(t *testing.T) {
	inputs := map[string]Input{
		"budget":  {Default: 256},
		"enabled": {Default: true},
		"ratio":   {Default: 1.5},
	}

	got, err := resolveInputs(inputs, lookupFrom(nil))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	for key, want := range map[string]string{
		"budget":  "256",
		"enabled": "true",
		"ratio":   "1.5",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// states declares `default: []` with the comment "empty means every state the
// upstream reports". Rendered as "[]" that becomes a state literally named
// "[]"; it has to come out empty for the caller's "no states named" branch to
// be reachable.
func TestResolveInputsRendersListDefaults(t *testing.T) {
	inputs := map[string]Input{
		"none": {Type: "list", Default: []interface{}{}},
		"some": {Type: "list", Default: []interface{}{"Kerala", "Punjab"}},
	}

	got, err := resolveInputs(inputs, lookupFrom(nil))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if got["none"] != "" {
		t.Errorf("none = %q, want %q", got["none"], "")
	}
	if want := "Kerala,Punjab"; got["some"] != want {
		t.Errorf("some = %q, want %q", got["some"], want)
	}
}

// An input with neither an env value nor a default is NOT an error here. The
// caller knows which inputs its run actually needs, so it can fail with
// "publishUrl is not set (MANDI_PUBLISH_URL)" -- a message naming the thing
// that is missing -- where this function could only say "an input".
func TestResolveInputsUnsetIsEmptyNotAnError(t *testing.T) {
	inputs := map[string]Input{
		"publishUrl":  {Env: "MANDI_PUBLISH_URL"},
		"tokenSecret": {Env: "MANDI_TOKEN_SECRET", Secret: true},
	}

	got, err := resolveInputs(inputs, lookupFrom(nil))
	if err != nil {
		t.Fatalf("resolveInputs returned an error for unset inputs: %v -- "+
			"requiredness is the caller's call, not this function's", err)
	}
	for _, key := range []string{"publishUrl", "tokenSecret"} {
		value, ok := got[key]
		if !ok {
			t.Errorf("%q missing from the result; an unset input must still be present", key)
		}
		if value != "" {
			t.Errorf("%s = %q, want %q", key, value, "")
		}
	}
}

// Callers index the result by name. A key dropped because it resolved to
// nothing turns a configuration mistake into a lookup miss somewhere else.
func TestResolveInputsReturnsEveryKey(t *testing.T) {
	inputs := map[string]Input{
		"withEnv":     {Env: "SET", Default: "d"},
		"withDefault": {Default: "d"},
		"withNeither": {},
	}

	got, err := resolveInputs(inputs, lookupFrom(map[string]string{"SET": "v"}))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if len(got) != len(inputs) {
		t.Errorf("got %d resolved inputs, want %d: %v", len(got), len(inputs), got)
	}
	for key := range inputs {
		if _, ok := got[key]; !ok {
			t.Errorf("%q missing from the result", key)
		}
	}
}

// The real file, not a fixture: this is what catches an input being renamed or
// dropped in the YAML while code still reads the old name.
func TestResolveInputsAgainstRealSpec(t *testing.T) {
	spec, err := LoadSpec(Files, PipelinePath)
	if err != nil {
		t.Fatalf("LoadSpec(%q): %v", PipelinePath, err)
	}

	got, err := resolveInputs(spec.Inputs, lookupFrom(map[string]string{
		"MANDI_PUBLISH_URL":    "http://publish.test/catalog",
		"MANDI_TOKEN_USER":     "user",
		"MANDI_TOKEN_SECRET":   "secret",
		"MANDI_API_URI":        "http://upstream.test:8080",
		"MANDI_PARTICIPANT_ID": "",
	}))
	if err != nil {
		t.Fatalf("resolveInputs against the real spec: %v", err)
	}

	for _, key := range []string{
		"registryUrl", "baseUrl", "states", "fromDate", "toDate",
		"participantId", "networkId", "catalogOut", "withoutGeometry",
		"publishUrl", "tokenUser", "tokenSecret",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s declares input %q but it is missing from the resolved map", PipelinePath, key)
		}
	}

	for key, want := range map[string]string{
		"publishUrl":      "http://publish.test/catalog", // env only, no default
		"tokenSecret":     "secret",                      // secret, env only
		"baseUrl":         "http://upstream.test:8080",   // env beats default
		"participantId":   "agmarknet",                   // empty env falls back
		"registryUrl":     "http://registry:8081/api/v1", // no env set, default
		"withoutGeometry": "publish",                     // enum default
		"states":          "",                            // `default: []` means all states
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// TestResolveInputsEnforcesEnum covers the one constraint the file states that
// resolution can check on its own. An unenforced enum is worse than none:
// withoutGeometry reaching the catalog build as anything but publish or skip
// takes the "publish" branch by default, so a typo would ship geometry-less
// markets an operator believed were being skipped.
func TestResolveInputsEnforcesEnum(t *testing.T) {
	inputs := map[string]Input{
		"withoutGeometry": {Env: "WITHOUT_GEOMETRY", Enum: []string{"publish", "skip"}, Default: "publish"},
	}

	t.Run("allowed value passes", func(t *testing.T) {
		got, err := resolveInputs(inputs, func(string) (string, bool) { return "skip", true })
		if err != nil {
			t.Fatalf("resolveInputs: %v", err)
		}
		if got["withoutGeometry"] != "skip" {
			t.Errorf("withoutGeometry = %q, want skip", got["withoutGeometry"])
		}
	})

	t.Run("value outside the enum is refused", func(t *testing.T) {
		_, err := resolveInputs(inputs, func(string) (string, bool) { return "publsh", true })
		if err == nil {
			t.Fatal("a value outside the declared enum resolved without error")
		}
		for _, want := range []string{"withoutGeometry", "publsh", "publish", "skip"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	t.Run("unresolved value is not an enum violation", func(t *testing.T) {
		noDefault := map[string]Input{
			"withoutGeometry": {Env: "WITHOUT_GEOMETRY", Enum: []string{"publish", "skip"}},
		}
		got, err := resolveInputs(noDefault, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("an unresolved input was treated as an enum violation: %v", err)
		}
		if got["withoutGeometry"] != "" {
			t.Errorf("withoutGeometry = %q, want empty", got["withoutGeometry"])
		}
	})
}
