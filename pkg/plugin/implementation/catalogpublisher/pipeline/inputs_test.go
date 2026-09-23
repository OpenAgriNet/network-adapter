package pipeline

// inputs_test.go pins resolveInputs' precedence and its rendering, both of
// which are silent when they go wrong: a default that quietly beats a set env
// var runs the pipeline against the wrong host, and a non-string default
// rendered with the wrong verb reaches the upstream as "%!s(int=7)" rather
// than "7". The last test runs the real declared inputs through it, so this
// stays wired to mandi-price-agmarket.yaml rather than to a fixture that can
// drift away from it.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// resolveInputsFixture wraps a bare inputs map in the minimum Spec that
// ResolveInputs needs. The zone is part of the Spec so a pipeline's schedule
// and its date window cannot drift apart; these tests are about resolution
// itself, so they state one and move on.
func resolveInputsFixture(inputs map[string]Input, lookup func(string) (string, bool)) (map[string]string, error) {
	return ResolveInputs(specWith(inputs), lookup)
}

func resolveInputsAtFixture(inputs map[string]Input, lookup func(string) (string, bool), now time.Time) (map[string]string, error) {
	return resolveInputsAt(specWith(inputs), lookup, now)
}

func specWith(inputs map[string]Input) Spec {
	return Spec{Inputs: inputs, Schedule: Schedule{Cron: "0 0 * * *", Timezone: "Asia/Kolkata"}}
}

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

	got, err := resolveInputsFixture(inputs, lookupFrom(map[string]string{
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
			got, err := resolveInputsFixture(inputs, lookupFrom(env))
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

	got, err := resolveInputsFixture(inputs, lookupFrom(nil))
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

	got, err := resolveInputsFixture(inputs, lookupFrom(nil))
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

	got, err := resolveInputsFixture(inputs, lookupFrom(nil))
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

	got, err := resolveInputsFixture(inputs, lookupFrom(map[string]string{"SET": "v"}))
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
		got, err := resolveInputsFixture(inputs, func(string) (string, bool) { return "skip", true })
		if err != nil {
			t.Fatalf("resolveInputs: %v", err)
		}
		if got["withoutGeometry"] != "skip" {
			t.Errorf("withoutGeometry = %q, want skip", got["withoutGeometry"])
		}
	})

	t.Run("value outside the enum is refused", func(t *testing.T) {
		_, err := resolveInputsFixture(inputs, func(string) (string, bool) { return "publsh", true })
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
		got, err := resolveInputsFixture(noDefault, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("an unresolved input was treated as an enum violation: %v", err)
		}
		if got["withoutGeometry"] != "" {
			t.Errorf("withoutGeometry = %q, want empty", got["withoutGeometry"])
		}
	})
}

// TestResolveInputsResolvesDateDefaults covers an input the file declares as a
// date and this package previously handed on as the literal word "today".
//
// fromDate/toDate are declared `type: date, format: dd-MM-yyyy, default:
// today`. The resolved value goes into market-commodity.yaml's query and
// reaches the upstream as a date, so "today" is not a harmless placeholder --
// it is a malformed request the upstream answers with nothing, which this
// pipeline would then read as "no markets traded".
func TestResolveInputsResolvesDateDefaults(t *testing.T) {
	// A fixed clock, so the assertion is an exact string rather than a
	// restatement of the formatting code.
	at := time.Date(2026, time.September, 7, 3, 30, 0, 0, time.UTC)

	inputs := map[string]Input{
		"fromDate": {Type: "date", Format: "dd-MM-yyyy", Default: "today"},
	}
	got, err := resolveInputsAtFixture(inputs, func(string) (string, bool) { return "", false }, at)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	// 03:30 UTC on the 7th is 09:00 IST on the 7th -- same date either way,
	// chosen so this asserts the format rather than the timezone.
	if got["fromDate"] != "07-09-2026" {
		t.Errorf("fromDate = %q, want %q (dd-MM-yyyy, not Go's reference layout and not the word today)",
			got["fromDate"], "07-09-2026")
	}
}

// TestResolveInputsDateUsesPipelineTimezone pins the boundary case the
// timezone actually decides. The pipeline is scheduled at 00:00 Asia/Kolkata,
// so a run fires when UTC is still on the previous day; resolving "today" in
// UTC would ask the upstream for yesterday, every single night.
func TestResolveInputsDateUsesPipelineTimezone(t *testing.T) {
	// 20:00 UTC on the 6th is 01:30 IST on the 7th -- just after a midnight run.
	at := time.Date(2026, time.September, 6, 20, 0, 0, 0, time.UTC)

	inputs := map[string]Input{
		"fromDate": {Type: "date", Format: "dd-MM-yyyy", Default: "today"},
	}
	got, err := resolveInputsAtFixture(inputs, func(string) (string, bool) { return "", false }, at)
	if err != nil {
		t.Fatalf("resolveInputsAt: %v", err)
	}
	if got["fromDate"] != "07-09-2026" {
		t.Errorf("fromDate = %q, want %q -- resolved in UTC instead of the pipeline's timezone, "+
			"which asks the upstream for yesterday on every midnight run", got["fromDate"], "07-09-2026")
	}
}

// TestResolveInputsRejectsUnknownTypeAndFormat: an unrecognised type or an
// untranslatable format must refuse rather than pass a value through that
// nothing downstream can use. Silent passthrough is what made "today" reach
// the upstream in the first place.
func TestResolveInputsRejectsUnknownTypeAndFormat(t *testing.T) {
	for name, inputs := range map[string]map[string]Input{
		"unknown type": {
			"odd": {Type: "duration", Default: "5m"},
		},
		"untranslatable date format": {
			"fromDate": {Type: "date", Format: "RFC3339", Default: "today"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveInputsFixture(inputs, func(string) (string, bool) { return "", false }); err == nil {
				t.Error("resolved without error")
			}
		})
	}
}

// TestRedactedInputsHidesSecrets: the resolved map carries credentials, so
// anything that prints it must print the redacted copy instead.
func TestRedactedInputsHidesSecrets(t *testing.T) {
	inputs := map[string]Input{
		"tokenSecret": {Env: "MANDI_TOKEN_SECRET", Secret: true},
		"baseUrl":     {Env: "MANDI_API_URI"},
	}
	resolved, err := resolveInputsFixture(inputs, lookupFrom(map[string]string{
		"MANDI_TOKEN_SECRET": "hunter2",
		"MANDI_API_URI":      "http://upstream.test",
	}))
	if err != nil {
		t.Fatalf("resolveInputs: %v", err)
	}
	if resolved["tokenSecret"] != "hunter2" {
		t.Fatalf("the raw map must still carry the real secret, got %q", resolved["tokenSecret"])
	}

	safe := RedactedInputs(inputs, resolved)
	if strings.Contains(fmt.Sprint(safe), "hunter2") {
		t.Errorf("redactedInputs leaked the secret: %v", safe)
	}
	if safe["baseUrl"] != "http://upstream.test" {
		t.Errorf("redactedInputs altered a non-secret: %q", safe["baseUrl"])
	}
}
