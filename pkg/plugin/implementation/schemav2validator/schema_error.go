package schemav2validator

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"

	"github.com/getkin/kin-openapi/openapi3"
)

// This file turns a kin-openapi validation failure into the model.Error list
// that goes back on the wire. kin-openapi reports a failed oneOf as a single
// error carrying every branch's complaints, so the cause a caller can act on
// has to be found by walking the error tree, and the branches the payload was
// never claiming to be have to be dropped before anything is reported.

var schemaFieldCodes = map[string]string{
	"required": "SCH_REQUIRED_FIELD_MISSING",
	// kin-openapi uses "properties" for the additionalProperties-disallowed
	// case ("property %q is unsupported").
	"properties": "SCH_FIELD_NOT_ALLOWED",
	"enum":       "SCH_INVALID_ENUM",
	// "const" is sugar for a single-value enum and has no code of its own.
	"const":  "SCH_INVALID_ENUM",
	"format": "SCH_INVALID_FORMAT",
	"type":   "SCH_TYPE_NOT_SUPPORTED",
}

func schemaFieldToCode(schemaField string) string {
	if code, ok := schemaFieldCodes[schemaField]; ok {
		return code
	}
	return "SCH_SCHEMA_VALIDATION_FAILED"
}

// isDiscriminator matches the keywords that pin a value to say which
// alternative a payload is for. Inside a failed composite they usually mean
// "not this branch" rather than "this value is wrong".
func isDiscriminator(schemaField string) bool {
	return schemaField == "enum" || schemaField == "const"
}

// isGuard is narrower: it rules an alternative out rather than merely ranking
// its causes low. Packs write a guard as a const; ordinary value constraints
// use an enum. Reading an enum as a guard makes a payload whose only fault is
// a bad enum value look like a payload meant for some other alternative.
func isGuard(schemaField string) bool { return schemaField == "const" }

// isForeignField matches a closed branch shown a property it does not declare
// -- the weakest cause among alternatives, since a payload for one alternative
// makes every other branch call its fields unsupported.
func isForeignField(schemaField string) bool { return schemaField == "properties" }

// isComposite excludes "not" deliberately: it has nothing underneath by
// construction, but its pointer names a field and so is a cause in its own right.
func isComposite(schemaField string) bool {
	return schemaField == "allOf" || schemaField == "anyOf" || schemaField == "oneOf"
}

func isDisjunctive(schemaField string) bool {
	return schemaField == "oneOf" || schemaField == "anyOf"
}

func (v *schemav2Validator) formatValidationError(err error) error {
	var schemaErrors []model.Error
	if multiErr, ok := err.(openapi3.MultiError); ok {
		for _, e := range multiErr {
			v.extractSchemaErrors(e, &schemaErrors)
		}
	} else {
		v.extractSchemaErrors(err, &schemaErrors)
	}
	return &model.SchemaValidationErr{Errors: schemaErrors}
}

func (v *schemav2Validator) extractSchemaErrors(err error, schemaErrors *[]model.Error) {
	switch e := err.(type) {
	case *model.Error:
		// Already classified upstream by the domain and JSON-LD checks.
		*schemaErrors = append(*schemaErrors, *e)
	case *openapi3.SchemaError:
		// Report a composite's branch causes rather than the composite, so the
		// caller is told "property X is missing" instead of that some unnamed
		// branch did not match. anyOf and not discard theirs, leaving none.
		if leaves := schemaErrorLeaves(e); len(leaves) > 0 {
			for _, leaf := range rankSchemaLeaves(leaves) {
				*schemaErrors = append(*schemaErrors, *codedSchemaError(leaf))
			}
			return
		}
		*schemaErrors = append(*schemaErrors, *codedSchemaError(e))
	case openapi3.MultiError:
		for _, sub := range e {
			v.extractSchemaErrors(sub, schemaErrors)
		}
	default:
		*schemaErrors = append(*schemaErrors, *model.NewCodedError("SCH_SCHEMA_VALIDATION_FAILED", err.Error()))
	}
}

// rankSchemaLeaves keeps every cause and only reorders them, because a
// SchemaValidationErr reports all of them but takes its wire-level code from
// the first. It is presentation only: which branch of a failed oneOf the
// caller meant cannot be recovered here, since kin-openapi evaluates composite
// keywords before properties and returns as soon as they fail.
func rankSchemaLeaves(leaves []*openapi3.SchemaError) []*openapi3.SchemaError {
	rank := func(leaf *openapi3.SchemaError) int {
		switch {
		case isForeignField(leaf.SchemaField):
			return 2
		case isDiscriminator(leaf.SchemaField):
			return 1
		default:
			return 0
		}
	}
	ranked := slices.Clone(leaves)
	slices.SortStableFunc(ranked, func(a, b *openapi3.SchemaError) int {
		return rank(a) - rank(b)
	})
	return ranked
}

func codedSchemaError(se *openapi3.SchemaError) *model.Error {
	path := strings.Join(se.JSONPointer(), "/")
	if path == "" {
		path = se.SchemaField
	}
	// SchemaError.Error() appends a full schema-and-value listing that made
	// NACK bodies unreadable, so only Reason is used. "not" is the one keyword
	// kin-openapi leaves without one: it fires when the subschema it negates
	// matched, and a match has nothing to say.
	message := se.Reason
	if message == "" {
		message = "value is not allowed here"
		if se.SchemaField != "not" {
			message = fmt.Sprintf("value does not satisfy %q", se.SchemaField)
		}
	}
	errItem := model.NewCodedError(schemaFieldToCode(se.SchemaField), message)
	if path != "" {
		errItem.Details = &model.ErrorDetails{Path: path}
	}
	return errItem
}

// schemaErrorLeaves returns the concrete assertion failures under err, in
// encounter order and deduplicated.
//
// kin-openapi stores a composite's causes as Origin = fmt.Errorf("...: %w",
// multiErr), so the per-branch slice sits one errors.Unwrap below Origin.
// Reading Origin.Error() as text instead of walking it loses per-branch
// attribution: the messages are concatenated, and picking one out of the
// string selects by position rather than by relevance.
func schemaErrorLeaves(err error) []*openapi3.SchemaError {
	se, ok := err.(*openapi3.SchemaError)
	if !ok || se.Origin == nil {
		return nil
	}
	// Starting below the composite lets one with no reachable cause yield
	// nothing, so the caller falls back to reporting the composite itself.
	return dedupeSchemaLeaves(leavesUnder(se.Origin, isDisjunctive(se.SchemaField)))
}

// leavesUnder returns the reportable leaves under one node. alternatives says
// whether e holds the branches of a oneOf or anyOf, in which case the ones the
// schema ruled out are dropped; otherwise they are conjuncts and each counts.
func leavesUnder(e error, alternatives bool) []*openapi3.SchemaError {
	if e == nil {
		return nil
	}

	if se, ok := e.(*openapi3.SchemaError); ok {
		if se.Origin != nil {
			// Something more specific underneath; the wrapper adds nothing.
			if sub := leavesUnder(se.Origin, isDisjunctive(se.SchemaField)); len(sub) > 0 {
				return sub
			}
		}
		if isComposite(se.SchemaField) {
			return nil
		}
		// A "not" on the object itself rather than on a member has no pointer
		// and no reason, so it says only that something somewhere is wrong.
		if se.SchemaField == "not" && len(se.JSONPointer()) == 0 {
			return nil
		}
		return []*openapi3.SchemaError{se}
	}

	branches, ok := errorBranches(e)
	if !ok {
		if u := errors.Unwrap(e); u != nil {
			return leavesUnder(u, alternatives)
		}
		return nil
	}

	arms := make([][]*openapi3.SchemaError, len(branches))
	pinned := make([][]*openapi3.SchemaError, len(branches))
	ruledOut := make([]bool, len(branches))
	allRuledOut := alternatives
	for i, branch := range branches {
		arms[i] = leavesUnder(branch, false)
		pinned[i] = discriminatorLeaves(arms[i])
		ruledOut[i] = alternatives && discriminatedAway(branch)
		allRuledOut = allRuledOut && ruledOut[i]
	}

	// An alternative still standing on its own terms is the one the caller was
	// aiming at, so the ones their own pinned values ruled out add nothing.
	// Once every alternative is out, keep those that failed on a pinned value
	// and nothing else: the caller all but sent that one, so the pinned value
	// is the field they got wrong. One wanting more besides is a different
	// reading, and reporting it sends them to fill in fields that are not theirs.
	var kept [][]*openapi3.SchemaError
	for i, arm := range arms {
		onlyPinned := len(arm) > 0 && len(pinned[i]) == len(arm)
		if !ruledOut[i] || (allRuledOut && onlyPinned) {
			kept = append(kept, arm)
		}
	}

	// Nothing survived, so no alternative says which was meant. Most of what
	// they reported is about shapes the payload was never going to be; what
	// ruled each one out is the whole of the news.
	if len(kept) == 0 {
		for _, why := range pinned {
			if len(why) > 0 {
				kept = append(kept, why)
			}
		}
		if len(kept) == 0 {
			kept = arms
		}
	}

	if alternatives {
		kept = nearest(kept)
	}
	return slices.Concat(kept...)
}

// discriminatedAway reports whether an alternative was ruled out on its own
// terms rather than by a fault in the payload. A tripped "not" does not count
// even though an alternative can be guarded with one, because the same keyword
// is how every pack says a field is not allowed here. The search stops at a
// nested oneOf or anyOf: a discriminator inside one of *its* alternatives
// rules that one out, not the branch holding the choice.
func discriminatedAway(e error) bool {
	if e == nil {
		return false
	}

	if se, ok := e.(*openapi3.SchemaError); ok {
		if isDisjunctive(se.SchemaField) {
			return false
		}
		if isGuard(se.SchemaField) {
			return true
		}
		if se.Origin == nil {
			return false
		}
		e = se.Origin
	}

	if branches, ok := errorBranches(e); ok {
		return slices.ContainsFunc(branches, discriminatedAway)
	}
	if u := errors.Unwrap(e); u != nil {
		return discriminatedAway(u)
	}
	return false
}

// errorBranches reads a multierror by reflection: openapi3.MultiError and the
// unexported multiErrorForOneOf / multiErrorForAllOf are all []error
// underneath but share no interface. TestErrorBranches_KinOpenAPIShapes fails
// if an upgrade changes that.
func errorBranches(e error) ([]error, bool) {
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Slice {
		return nil, false
	}
	branches := make([]error, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		if sub, ok := v.Index(i).Interface().(error); ok {
			branches = append(branches, sub)
		}
	}
	return branches, true
}

func discriminatorLeaves(leaves []*openapi3.SchemaError) []*openapi3.SchemaError {
	var kept []*openapi3.SchemaError
	for _, leaf := range leaves {
		if isDiscriminator(leaf.SchemaField) {
			kept = append(kept, leaf)
		}
	}
	return kept
}

// nearest drops the alternatives asking for everything another asks for and
// more. All of them failed and the schema cannot say which was meant; the one
// that failed for fewest reasons is the one the caller came closest to
// sending, and the extra demands of the others hold only if a different
// alternative was meant.
func nearest(arms [][]*openapi3.SchemaError) [][]*openapi3.SchemaError {
	reasons := make([]map[string]bool, len(arms))
	for i, arm := range arms {
		reasons[i] = make(map[string]bool, len(arm))
		for _, leaf := range arm {
			reasons[i][leafKey(leaf)] = true
		}
	}

	var kept [][]*openapi3.SchemaError
	for i, arm := range arms {
		// strictSubset is false against itself, so i needs no special case.
		outdone := slices.ContainsFunc(reasons, func(other map[string]bool) bool {
			return strictSubset(other, reasons[i])
		})
		if !outdone {
			kept = append(kept, arm)
		}
	}
	return kept
}

// strictSubset treats an empty a as no subset: an alternative with no
// reachable cause is a gap in what the validator reported, not an easier way
// to satisfy the schema.
func strictSubset(a, b map[string]bool) bool {
	if len(a) == 0 || len(a) >= len(b) {
		return false
	}
	for reason := range a {
		if !b[reason] {
			return false
		}
	}
	return true
}

// leafKey identifies a leaf by what it says, so the same complaint reached by
// two routes counts once.
func leafKey(leaf *openapi3.SchemaError) string {
	return leaf.SchemaField + "\x00" + strings.Join(leaf.JSONPointer(), "/") + "\x00" + leaf.Reason
}

func dedupeSchemaLeaves(leaves []*openapi3.SchemaError) []*openapi3.SchemaError {
	out := make([]*openapi3.SchemaError, 0, len(leaves))
	seen := make(map[string]bool, len(leaves))
	for _, leaf := range leaves {
		key := leafKey(leaf)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, leaf)
	}
	return out
}
