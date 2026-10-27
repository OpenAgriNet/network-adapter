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

// Turns a kin-openapi validation failure into the model.Error list sent on the
// wire. A failed oneOf arrives as one error, so its real cause must be walked out.
//
// The path a failure takes through this file:
//
//	1. formatValidationError  entry point; unwraps a top-level MultiError
//	2. extractSchemaErrors    per error: its leaves if any, else the error itself
//	3. schemaErrorLeaves      walks below Origin, then dedupes what it finds
//	4. leavesUnder            recurses; drops the alternatives the schema ruled out
//	5. nearest                of the survivors, keeps the one closest to passing
//	6. rankSchemaLeaves       orders them -- the first sets the SCH_* code
//	7. codedSchemaError       renders one leaf as a model.Error
//
// Chasing a wrong NACK: step 4 shows each branch's leaves, step 5 shows which
// of them survived. Those two decide what the caller is told.

var schemaFieldCodes = map[string]string{
	"required": "SCH_REQUIRED_FIELD_MISSING",
	// kin-openapi uses "properties" for the additionalProperties case.
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

// Keywords pinning a value to say which alternative a payload is for; inside a
// failed composite they mean "not this branch", not "this value is wrong".
func isDiscriminator(schemaField string) bool {
	return schemaField == "enum" || schemaField == "const"
}

// Narrower: rules an alternative out. Packs guard with const, and constrain
// ordinary values with enum, so reading an enum as a guard confuses the two.
func isGuard(schemaField string) bool { return schemaField == "const" }

// A closed branch shown a property it does not declare -- the weakest cause
// among alternatives, since every other branch reports the same thing.
func isForeignField(schemaField string) bool { return schemaField == "properties" }

// "not" is excluded: it has nothing underneath, but its pointer names a field
// and so is a cause in its own right.
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
		// Already classified by the domain and JSON-LD checks.
		*schemaErrors = append(*schemaErrors, *e)
	case *openapi3.SchemaError:
		// Prefer a composite's branch causes, which name the field. anyOf and
		// not discard theirs, leaving the composite as all there is to report.
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

// Reorders causes without dropping any: SchemaValidationErr reports them all
// but takes its wire-level code from the first.
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
	// Only Reason: SchemaError.Error() appends a schema-and-value listing that
	// made NACK bodies unreadable. "not" is the one keyword leaving it empty.
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

// Concrete assertion failures under err, in encounter order, deduplicated.
// Walking the tree rather than reading Origin.Error() keeps branch attribution.
func schemaErrorLeaves(err error) []*openapi3.SchemaError {
	se, ok := err.(*openapi3.SchemaError)
	if !ok || se.Origin == nil {
		return nil
	}
	// Below the composite, so one with no reachable cause yields nothing.
	return dedupeSchemaLeaves(leavesUnder(se.Origin, isDisjunctive(se.SchemaField)))
}

// alternatives says e holds oneOf/anyOf branches, so the ones the schema ruled
// out are dropped; otherwise they are conjuncts and each counts.
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
		// A "not" on the object itself has no pointer and no reason.
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

	// Keep the alternatives still standing. Once all are out, keep those that
	// failed on a pinned value alone: that is the field the caller got wrong.
	var kept [][]*openapi3.SchemaError
	for i, arm := range arms {
		onlyPinned := len(arm) > 0 && len(pinned[i]) == len(arm)
		if !ruledOut[i] || (allRuledOut && onlyPinned) {
			kept = append(kept, arm)
		}
	}

	// Nothing survived, so report what ruled each one out.
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

// Whether an alternative was ruled out on its own terms rather than by a fault
// in the payload. Stops at a nested oneOf or anyOf, and never counts a "not".
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

// Reflection because the multierror types are unexported slices sharing no
// interface. TestErrorBranches_KinOpenAPIShapes fails if that ever changes.
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

// Drops alternatives asking for everything another asks for and more: the one
// that failed for fewest reasons is the one the caller came closest to sending.
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
		// False against itself, so i needs no special case.
		outdone := slices.ContainsFunc(reasons, func(other map[string]bool) bool {
			return strictSubset(other, reasons[i])
		})
		if !outdone {
			kept = append(kept, arm)
		}
	}
	return kept
}

// An empty a is no subset: no reachable cause is a gap in what the validator
// reported, not an easier way to satisfy the schema.
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

// Identifies a leaf by what it says, so one complaint reached twice counts once.
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
