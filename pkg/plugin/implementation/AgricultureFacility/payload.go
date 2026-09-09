// payload.go is the one place in this repo that reads a field out of a Beckn
// payload in Go rather than in a mapping.
//
// It exists because the facility types have to be known BEFORE any mapping
// runs: they decide how many single-type payloads one inbound payload becomes,
// and each of those is then mapped and called on its own. Reading them here is
// what keeps splitting out of jsonmapper, out of pkg/plugin/definition and out
// of internal/upstream -- see search.go.
//
// The cost, stated plainly because it is the one real cost of that: this path
// is compiled in. The mapping's required: checks read the SAME path in
// JSONata, so a Beckn payload shape change touches both, and only one of them
// is a config edit. Keep the two in step; a mismatch means a payload the
// checks accept and this refuses, which reads as an adapter bug rather than as
// a bad request.
//
// Separated from the package clause by a blank line on purpose: the package's
// own doc comment is in AgricultureFacility.go, and this is a note about one
// file.

package AgricultureFacility

import (
	"fmt"
	"slices"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// DefaultFacilityTypesAt is where a Beckn v2 select carries the facility types
// a search asks for.
//
// A PATH rather than a walk written out in Go, for the same reason
// common.Config's providerIdAt and capabilityCodeAt are paths: the payload's
// shape is the network's convention, not this adapter's. Compiled in, a spec
// change needs a rebuild; as a path, it needs a config edit. The default is
// what every deployment should be using, and facilityTypesAt exists so a spec
// change can be tracked without waiting for a release.
const DefaultFacilityTypesAt = "message.contract.commitments[].resources[]." +
	"resourceAttributes.supportedFacilityTypes[]"

// facilityTypesFrom returns the facility types a payload asks for, in the
// order the payload wrote them -- one upstream call each.
//
// The reading is common.ValuesAt's, the same walker that finds a binding key,
// so the type assertions and the absent-field handling are in one place rather
// than repeated here. It returns only the strings the path reaches: a missing
// field, an object where a list belongs, or a number among the types all
// arrive as "no value at that path", which is a bad request either way.
//
// Every failure is a bad request rather than an adapter fault: the payload is
// the thing that is wrong, and the caller is the only one who can fix it. The
// mapping's own required: checks refuse most of these first, with better
// messages; these are what is left if a check is relaxed.
func facilityTypesFrom(beckn any, path string) ([]string, error) {
	if path == "" {
		path = DefaultFacilityTypesAt
	}

	// LeavesAt, not ValuesAt: a non-string here is the caller's mistake and has
	// to be reported. ValuesAt would drop it, and dropping one entry of a
	// hand-written list means searching for the rest and reporting success --
	// a partial answer with nothing recording what was lost.
	leaves := common.LeavesAt(beckn, path)

	// A bare string is one value: the same instruction as a one-element list,
	// which is what a caller writing a single type may well send. The path's
	// "[]" suffix wants a list, so the scalar form is a second read rather than
	// a looser walker -- keeping "[]" meaning exactly "a list" everywhere.
	//
	// A list that is PRESENT and empty reads as zero leaves too, and the second
	// read would then hand back the empty list itself as one value -- reported
	// as "[] is not a facility type", which is true and useless. Skipping a
	// leaf that is itself a list keeps that case on the "names no facility
	// type" message, which is what an empty list means.
	if len(leaves) == 0 {
		for _, leaf := range common.LeavesAt(beckn, strings.TrimSuffix(path, arrayMarker)) {
			if _, isList := leaf.([]any); isList {
				continue
			}
			leaves = append(leaves, leaf)
		}
	}

	if len(leaves) == 0 {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload names no facility type at %s, "+
				"so there is nothing to ask the provider for", path))
	}

	// Each value becomes a category code in the request half's $codes lookup,
	// so a non-string is a request this capability cannot build -- refused
	// here rather than sent as a null category POCRA answers with everything.
	values := make([]string, 0, len(leaves))
	for _, leaf := range leaves {
		text, ok := leaf.(string)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: supportedFacilityTypes contains %v (%T), which is not a facility type",
				leaf, leaf))
		}
		values = append(values, text)
	}

	// A repeated type is refused rather than absorbed. It would otherwise pass
	// every governed-type check and make one duplicate upstream call per
	// repeat for nothing, up to MaxFacilityTypes.
	//
	// Checked here rather than left to the mapping, which states the same rule
	// in its own required: block: the split happens before either half of the
	// mapping runs, and each part it produces names exactly one type, so the
	// mapping's version can no longer see a repeat. The wording matches so a
	// caller cannot tell which guard answered.
	for index, value := range values {
		if slices.Contains(values[:index], value) {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: supportedFacilityTypes must not repeat a facility type, and %q appears twice",
				value))
		}
	}
	return values, nil
}

// arrayMarker is common.ValuesAt's "look in each element" suffix.
const arrayMarker = "[]"

// dig walks a chain of object keys, returning nil the moment one is absent or
// is not an object. Written out rather than reached for from a library because
// a mistyped key must read as an absent field, which is a bad request, and not
// as a panic.
func dig(document map[string]any, keys ...string) any {
	var current any = document
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}
