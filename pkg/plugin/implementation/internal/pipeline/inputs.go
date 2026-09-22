// inputs.go turns the file's `inputs:` block into the flat name->value map the
// rest of a run reads, resolving env over default.
//
// Flag is deliberately NOT consulted. tools/publish/mandi_publish resolved
// flag > env > default because it was a CLI; this package is loaded into a
// long-lived server process that has no command line of its own, so the Flag
// field is retained only to keep Input a faithful mapping of the file (see
// spec.go's header) -- its absence from this resolution is the design, not an
// oversight.
package pipeline

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// The zone "today" is resolved in comes from the pipeline's own
// schedule.timezone, and is NOT a display preference. A pipeline scheduled at
// 00:00 Asia/Kolkata fires while UTC is still on the previous day; resolving
// "today" in UTC would ask the upstream for yesterday's data, every night, and
// the result would look like a successful run.
//
// It is read from the Spec rather than passed separately so the schedule and
// the date window cannot drift apart -- there is only one place to state it.

// Input types the file uses. An input declaring anything else is refused
// rather than passed through: passing an unrecognised type through verbatim is
// exactly how `today` reached the upstream as a date.
const (
	inputTypeDate = "date"
	inputTypeList = "list"
)

// dateFormats maps the upstream's own date notation to Go's reference layout.
// The file writes dd-MM-yyyy because that is what this upstream reads -- it is
// not ISO, and it is not Go's 02-01-2006 either, so the translation has to be
// explicit and a format not in this table is an error rather than a guess.
var dateFormats = map[string]string{
	"dd-MM-yyyy": "02-01-2006",
	"yyyy-MM-dd": "2006-01-02",
}

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
func ResolveInputs(spec Spec, lookup func(string) (string, bool)) (map[string]string, error) {
	return resolveInputsAt(spec, lookup, time.Now())
}

// resolveInputsAt is resolveInputs with the clock supplied, so a test can fix
// "today" and assert an exact date rather than restate the formatting code.
func resolveInputsAt(spec Spec, lookup func(string) (string, bool), now time.Time) (map[string]string, error) {
	inputs := spec.Inputs

	// An empty zone would be read by time.LoadLocation as UTC, which is the
	// one wrong answer that looks like a working one.
	timezone := strings.TrimSpace(spec.Schedule.Timezone)
	if timezone == "" {
		return nil, fmt.Errorf("the pipeline states no schedule.timezone, so \"today\" cannot be resolved; " +
			"a date resolved in the wrong zone asks the upstream for the wrong day and still looks successful")
	}

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

	// Names sorted so two runs of the same broken config report the same
	// input first; ranging a map would pick an arbitrary one each time.
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		input := inputs[name]

		value, err := applyType(name, resolved[name], input, now, timezone)
		if err != nil {
			return nil, err
		}
		resolved[name] = value

		if err := checkEnum(name, resolved[name], input); err != nil {
			return nil, err
		}
	}

	return resolved, nil
}

// applyType turns a resolved string into what the input's declared type says
// it is. Only the types the file actually uses are understood; anything else
// is refused, because a type nobody implemented is a value nobody validated.
func applyType(name, value string, input Input, now time.Time, timezone string) (string, error) {
	switch input.Type {
	case "":
		return value, nil

	case inputTypeList:
		// Already flattened to a comma-separated string by renderDefault, and
		// an env override is written the same way.
		return value, nil

	case inputTypeDate:
		layout, ok := dateFormats[input.Format]
		if !ok {
			return "", fmt.Errorf("input %q declares date format %q, which this pipeline cannot translate "+
				"(known: %s)", name, input.Format, strings.Join(knownDateFormats(), ", "))
		}
		if value != "today" {
			// Already a date, from an env var or a literal default. Checked
			// against the declared format rather than trusted, so a
			// wrong-format override fails here and not at the upstream.
			if _, err := time.Parse(layout, value); err != nil {
				return "", fmt.Errorf("input %q is %q, which is not %s", name, value, input.Format)
			}
			return value, nil
		}

		location, err := time.LoadLocation(timezone)
		if err != nil {
			return "", fmt.Errorf("input %q: loading %s: %w", name, timezone, err)
		}
		return now.In(location).Format(layout), nil

	default:
		return "", fmt.Errorf("input %q declares type %q, which this pipeline does not implement", name, input.Type)
	}
}

// knownDateFormats lists the translatable formats for an error message.
func knownDateFormats() []string {
	formats := make([]string, 0, len(dateFormats))
	for format := range dateFormats {
		formats = append(formats, format)
	}
	sort.Strings(formats)
	return formats
}

// redactedInputs is the copy to print. resolveInputs' result carries
// credentials (tokenUser, tokenSecret) in the same plain map as everything
// else, so any caller that logs a config dump, writes a run report or wraps
// the map in an error must use this instead. client.go is careful never to
// echo a credential; a logged config map would undo all of it in one line.
func RedactedInputs(inputs map[string]Input, resolved map[string]string) map[string]string {
	safe := make(map[string]string, len(resolved))
	for name, value := range resolved {
		if inputs[name].Secret && value != "" {
			safe[name] = "[redacted]"
			continue
		}
		safe[name] = value
	}
	return safe
}

// checkEnum rejects a resolved value the file does not allow, naming the input,
// the value and the permitted set so the fix is obvious from the message alone.
//
// A secret's value is redacted even here. No secret declares an enum today, so
// this is defensive rather than live -- but an error message is the most
// likely place for a credential to escape, and the guard costs one branch.
func checkEnum(name, value string, input Input) error {
	if len(input.Enum) == 0 || value == "" {
		return nil
	}
	for _, candidate := range input.Enum {
		if value == candidate {
			return nil
		}
	}

	shown := value
	if input.Secret {
		shown = "[redacted]"
	}
	return fmt.Errorf("input %q is %q, which is not one of %s",
		name, shown, strings.Join(input.Enum, ", "))
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
