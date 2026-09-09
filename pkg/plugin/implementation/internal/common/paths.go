package common

import (
	"fmt"
	"strings"
)

// Paths says where the two halves of a binding key live in a payload.
//
// A NETWORK convention, not a deployment's preference: every participant must
// agree or requests silently fail to match. BecknV2 is the answer, and absent
// means correct. The override exists only for tracking a spec change without
// waiting for a release.
type Paths struct {
	ProviderID     string
	CapabilityCode string
}

// BecknV2 is where core-v2.0.0-lts puts them.
var BecknV2 = Paths{
	ProviderID:     "message.contract.commitments[].offer.provider.id",
	CapabilityCode: "message.contract.commitments[].resources[].resourceAttributes.@type",
}

// arrayMarker flattens an array at that segment -- the only operator walk
// understands.
const arrayMarker = "[]"

// Validate refuses a pair that could never match, so a mistake surfaces at
// startup rather than as every request quietly going unserved.
func (p Paths) Validate() error {
	for name, path := range map[string]string{
		"providerIdAt":     p.ProviderID,
		"capabilityCodeAt": p.CapabilityCode,
	} {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("binding path %s is empty", name)
		}
		for _, segment := range strings.Split(path, ".") {
			if strings.TrimSpace(strings.TrimSuffix(segment, arrayMarker)) == "" {
				return fmt.Errorf("binding path %s (%q) has a blank segment", name, path)
			}
		}
	}
	return nil
}

// ValuesAt collects every string the path reaches.
//
// The grammar is two things: segments separated by ".", and a "[]" suffix
// meaning "look in each element". No wildcards, filters or indices -- each is
// another way to write something subtly wrong in config nobody reviews.
//
// EXPORTED for a domain package that has to read its own field out of a
// payload -- AgricultureFacility reads supportedFacilityTypes this way. The
// alternative is hand-walking the document in Go, which compiles the payload's
// shape into the binary: a spec change then needs a rebuild rather than a
// config edit, and the traversal has to re-check every type assertion the
// walker already checks.
func ValuesAt(node any, path string) []string {
	var found []string
	for _, leaf := range walk(node, strings.Split(path, ".")) {
		// Only strings are binding-key material; anything else means the path
		// landed somewhere unintended.
		if value, ok := leaf.(string); ok {
			found = append(found, value)
		}
	}
	return found
}

// countAt reports how many elements the first array segment holds, whether or
// not the leaf beyond it resolves.
//
// valuesAt cannot answer this: it returns resolved STRINGS and drops a missing
// leaf, so two commitments where one has no provider id yield one value and
// count as one commitment -- silently answering half a request instead of
// refusing it.
func countAt(node any, path string) int {
	for _, segment := range strings.Split(path, ".") {
		fields, ok := node.(map[string]any)
		if !ok {
			return 0
		}
		child, present := fields[strings.TrimSuffix(segment, arrayMarker)]
		if !present {
			return 0
		}
		if strings.HasSuffix(segment, arrayMarker) {
			elements, ok := child.([]any)
			if !ok {
				return 0
			}
			return len(elements)
		}
		node = child
	}
	return 0
}

// LeavesAt collects every value the path reaches, whatever its type.
//
// ValuesAt's sibling, and the one a caller wants when a non-string at the leaf
// is a MISTAKE to report rather than a leaf to skip. ValuesAt drops it, which
// is right for a binding key -- a number where a capability code belongs means
// the path landed somewhere unintended, and there is nothing to say about it
// beyond "no binding here". It is wrong for a list the caller wrote by hand:
// dropping one entry of supportedFacilityTypes would search for the rest and
// report success, which is a partial answer with nothing recording the loss.
func LeavesAt(node any, path string) []any {
	return walk(node, strings.Split(path, "."))
}

// walk collects the leaves a path reaches. Type policy is the caller's: this
// returns what is there, ValuesAt keeps the strings, LeavesAt keeps everything.
func walk(node any, segments []string) []any {
	if len(segments) == 0 {
		return []any{node}
	}

	segment := segments[0]
	rest := segments[1:]

	fields, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	child, present := fields[strings.TrimSuffix(segment, arrayMarker)]
	if !present {
		return nil
	}

	if !strings.HasSuffix(segment, arrayMarker) {
		return walk(child, rest)
	}

	// An array segment: every element contributes.
	elements, ok := child.([]any)
	if !ok {
		return nil
	}
	var found []any
	for _, element := range elements {
		found = append(found, walk(element, rest)...)
	}
	return found
}
