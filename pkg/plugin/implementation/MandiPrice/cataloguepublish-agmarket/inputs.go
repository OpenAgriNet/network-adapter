// inputs.go turns the file's `inputs:` block into the flat name->value map the
// rest of a run reads, resolving env over default.
//
// Flag is deliberately NOT consulted. tools/publish/mandi_publish resolved
// flag > env > default because it was a CLI; this package is loaded into a
// long-lived server process that has no command line of its own, so the Flag
// field is retained only to keep Input a faithful mapping of the file (see
// spec.go's header) -- its absence from this resolution is the design, not an
// oversight.
package agmarket

import (
	"fmt"
	"reflect"
	"strings"
)

// resolveInputs resolves every declared input to a string, env value first and
// the file's default second. lookup is os.LookupEnv's shape, passed in so a
// caller (and a test) can state the environment rather than mutate the
// process's.
//
// An env var that is present but EMPTY falls back to the default. An empty
// value is what a half-filled compose file or k8s manifest produces, and
// honouring it as a deliberate override points the run at nothing at all;
// nothing in this file wants "" as a considered choice.
//
// An input with neither an env value nor a default resolves to "" and is NOT
// an error. Requiredness lives with the caller, which knows what the run it is
// about to make actually needs -- it can refuse with "publishUrl is not set
// (MANDI_PUBLISH_URL)", naming the missing thing, where this function could
// only report that "an input" was unresolved. publishUrl, tokenUser and
// tokenSecret all reach here unresolved on a machine that has not been
// configured, and all three are the caller's to demand.
//
// Every key in inputs appears in the result, including the unresolved ones, so
// callers index the map instead of testing for presence.
//
// An input declaring `enum:` is checked against it. This is the one thing the
// file states about a value that resolution can enforce on its own, and an
// unenforced enum is worse than none: withoutGeometry reaching the catalog
// build as anything but publish or skip silently takes the "publish" branch,
// so a typo in a manifest would ship geometry-less markets that the operator
// believed were being skipped. An empty value is not checked -- unresolved is
// the caller's to demand (see above), not a wrong choice.
func resolveInputs(inputs map[string]Input, lookup func(string) (string, bool)) (map[string]string, error) {
	resolved := make(map[string]string, len(inputs))

	for name, input := range inputs {
		if input.Env != "" {
			if value, ok := lookup(input.Env); ok && value != "" {
				resolved[name] = value
				continue
			}
		}
		resolved[name] = renderDefault(input.Default)
	}

	for name, input := range inputs {
		if err := checkEnum(name, resolved[name], input.Enum); err != nil {
			return nil, err
		}
	}

	return resolved, nil
}

// checkEnum rejects a resolved value the file does not allow, naming the input,
// the value and the permitted set so the fix is obvious from the message alone.
func checkEnum(name, value string, allowed []string) error {
	if len(allowed) == 0 || value == "" {
		return nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("input %q is %q, which is not one of %s",
		name, value, strings.Join(allowed, ", "))
}

// renderDefault stringifies a YAML default, which is any scalar the file cares
// to write.
//
// fmt.Sprint rather than a %s: %s on an int yields "%!s(int=256)", which no
// upstream understands and which reads in a log line like a value rather than
// like a bug.
//
// A sequence default is joined with commas, not printed as a Go slice. `states`
// declares `default: []` meaning "every state the upstream reports", and
// fmt.Sprint would hand the caller the two-character string "[]" -- a state
// name, as far as any later filter can tell.
func renderDefault(value interface{}) string {
	if value == nil {
		return ""
	}

	if v := reflect.ValueOf(value); v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = fmt.Sprint(v.Index(i).Interface())
		}
		return strings.Join(parts, ",")
	}

	return fmt.Sprint(value)
}
