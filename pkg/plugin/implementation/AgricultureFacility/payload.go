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

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// facilityTypesFrom returns the facility types a payload asks for, in the
// order the payload wrote them -- one upstream call each.
//
// Every failure is a bad request rather than an adapter fault: the payload is
// the thing that is wrong, and the caller is the only one who can fix it. The
// mapping's own required: checks refuse most of these first, with better
// messages; these are what is left if a check is relaxed or a payload reaches
// here another way, and they exist so that path is a refusal rather than a
// panic on a nil map.
func facilityTypesFrom(beckn any) ([]string, error) {
	document, ok := beckn.(map[string]any)
	if !ok {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload is %T, not an object, so it names no commitment to search from", beckn))
	}

	commitments, _ := dig(document, "message", "contract", "commitments").([]any)
	if len(commitments) == 0 {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload carries no commitment to read a facility search from"))
	}
	commitment, ok := commitments[0].(map[string]any)
	if !ok {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload's first commitment is %T, not an object", commitments[0]))
	}

	resources, _ := commitment["resources"].([]any)
	if len(resources) == 0 {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload's commitment carries no resource to read a facility search from"))
	}
	resource, ok := resources[0].(map[string]any)
	if !ok {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload's first resource is %T, not an object", resources[0]))
	}

	declared := dig(resource, "resourceAttributes", "supportedFacilityTypes")

	// A bare string is one value: the same instruction as a one-element list,
	// which is what a caller writing a single type may well send.
	var declaredList []any
	switch typed := declared.(type) {
	case nil:
		declaredList = nil
	case []any:
		declaredList = typed
	default:
		declaredList = []any{typed}
	}
	if len(declaredList) == 0 {
		return nil, model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload names no facility type in supportedFacilityTypes, "+
				"so there is nothing to ask the provider for"))
	}

	// Each value becomes a category code in the request half's $codes lookup,
	// so a non-string is a request this capability cannot build -- refused
	// here rather than sent as a null category POCRA answers with everything.
	values := make([]string, 0, len(declaredList))
	for _, value := range declaredList {
		text, ok := value.(string)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: supportedFacilityTypes contains %v (%T), which is not a facility type",
				value, value))
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
