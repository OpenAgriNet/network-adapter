// Deriving the capability binding a Beckn request is asking for, so a step can
// tell whether the request is its work and, if it is, which registry row
// describes the call.
//
// The binding is a property of the network's payloads, not of any provider,
// which is why it sits here rather than in one capability's package.
package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// separator joins a binding key's two halves.
const separator = "|"

// errNoBinding reports a payload that names no binding. Not a fault: requests
// for other capabilities reach this step too, and it answers by doing nothing.
var errNoBinding = errors.New("payload names no capability binding")

// Binding identifies one provider capability.
type Binding struct {
	ParticipantID  string
	CapabilityCode string
}

// Key renders the binding in the form the registry indexes on.
func (b Binding) Key() string {
	return b.ParticipantID + separator + b.CapabilityCode
}

// bindingFrom derives the capability binding a payload is asking for.
//
// Returns errNoBinding when the payload names no provider or type -- the
// ordinary case for a request this step is not meant to serve.
//
// More than one distinct provider or type is refused, not resolved to the
// first: the two halves index ONE registry row for ONE call, so a payload
// spanning several is asking for something this design cannot express, and
// guessing would silently serve part of it.
func bindingFrom(paths Paths, body []byte) (Binding, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return Binding{}, fmt.Errorf("payload could not be read: %w", err)
	}

	// Checked before distinctness: N commitments naming the SAME provider and
	// type collapse to one key, so they would pass unnoticed and the mapping
	// would answer commitments[0] and drop the rest -- a confident, signed,
	// spec-valid answer to part of what was asked.
	//
	// Counted at the array, not over the values it yields: valuesAt drops an
	// absent leaf, so two commitments where one has no provider id would count
	// as one.
	if commitments := countAt(payload, paths.ProviderID); commitments > 1 {
		return Binding{}, fmt.Errorf(
			"payload carries %d commitments; one request maps to one call, "+
				"so send them separately rather than have all but the first dropped",
			commitments)
	}

	providers := distinct(valuesAt(payload, paths.ProviderID))
	types := distinct(valuesAt(payload, paths.CapabilityCode))

	if len(providers) == 0 || len(types) == 0 {
		return Binding{}, errNoBinding
	}
	if len(providers) > 1 {
		return Binding{}, fmt.Errorf("payload names %d providers (%s); one request maps to one call",
			len(providers), strings.Join(providers, ", "))
	}
	if len(types) > 1 {
		return Binding{}, fmt.Errorf("payload names %d resource types (%s); one request maps to one call",
			len(types), strings.Join(types, ", "))
	}

	return Binding{ParticipantID: providers[0], CapabilityCode: types[0]}, nil
}

// distinct drops blanks and repeats, keeping payload order so a refusal names
// them the way they were written.
func distinct(values []string) []string {
	var out []string
	for _, value := range values {
		out = appendDistinct(out, value)
	}
	return out
}

// appendDistinct adds value if it is neither empty nor already present.
func appendDistinct(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
