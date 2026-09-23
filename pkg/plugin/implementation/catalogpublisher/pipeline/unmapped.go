package pipeline

// unmapped.go answers "is this pipeline file saying anything the code cannot
// hear?".
//
// yaml.v3 drops unknown keys silently, so a block present in a pipeline file
// but absent from Spec parses "successfully" while vanishing. A rule written
// in the file -- an exclusion, a refusal, a catalogId template -- is then
// believed to be in force while no code ever sees it. This has already
// happened once here: Step was missing eight keys (when, else, forEach, as,
// concurrency, onError, onEmptyOutput, failWhenEmpty), carrying the
// conditional, the deliberate sequentiality and the "no rows is not a failure"
// classification, all parsing into nothing.
//
// This lives in the frame rather than in one pipeline's tests because every
// pipeline file has the same exposure, and each should assert against its own
// file (see the conformance test in any capability's publish package).

import (
	"embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnmappedKeys returns the dotted paths the file declares that Spec has no
// field for, deepest included -- an empty result means every key in the file
// reaches code.
//
// It works by round-tripping: Spec marshalled back to YAML yields exactly the
// keys Spec knows how to hold, which the file's keys must be a subset of.
//
// A key is reported only when its PARENT survived the round trip. A value the
// file leaves empty, or a field carrying omitempty, legitimately does not come
// back, and flagging those would drown the real finding in noise.
func UnmappedKeys(files embed.FS, path string) ([]string, error) {
	raw, err := files.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var fromFile map[string]any
	if err := yaml.Unmarshal(raw, &fromFile); err != nil {
		return nil, fmt.Errorf("parsing %s as a bare map: %w", path, err)
	}

	spec, err := LoadSpec(files, path)
	if err != nil {
		return nil, err
	}
	encoded, err := yaml.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("re-marshalling the parsed spec: %w", err)
	}
	var fromSpec map[string]any
	if err := yaml.Unmarshal(encoded, &fromSpec); err != nil {
		return nil, fmt.Errorf("parsing the re-marshalled spec: %w", err)
	}

	return missingKeys(nil, fromFile, fromSpec), nil
}

// missingKeys walks the file's structure against the round-tripped Spec's and
// returns the dotted paths the Spec cannot hold.
func missingKeys(path []string, file, spec any) []string {
	var missing []string

	switch fileNode := file.(type) {
	case map[string]any:
		specNode, ok := spec.(map[string]any)
		if !ok {
			return nil // parent did not survive; its children are not the finding
		}
		for key, fileChild := range fileNode {
			here := append(append([]string{}, path...), key)
			specChild, ok := specNode[key]
			if !ok {
				missing = append(missing, strings.Join(here, "."))
				continue
			}
			missing = append(missing, missingKeys(here, fileChild, specChild)...)
		}

	case []any:
		specNode, ok := spec.([]any)
		if !ok {
			return nil
		}
		// Sequence entries are heterogeneous -- one step declares `when`,
		// another declares `onError` -- so each entry is compared against the
		// Spec entry in the same position rather than against the first.
		for i, fileChild := range fileNode {
			here := append(append([]string{}, path...), fmt.Sprintf("[%d]", i))
			if i < len(specNode) {
				missing = append(missing, missingKeys(here, fileChild, specNode[i])...)
			}
		}
	}

	return missing
}
