package AgricultureFacility

// payload_test.go covers the one path read this package does in Go rather
// than in a mapping. Table-driven because the interesting cases are all
// malformed payloads, and each one has to say something a caller can act on.
//
// An INTERNAL test package, unlike every other test file here: what it tests
// is unexported, and exporting a payload path read so a test can reach it
// would publish it as something another package may depend on.

import (
	"encoding/json"
	"strings"
	"testing"
)

// decode is what Step.Run works from: the payload, already decoded into
// map[string]any / []any by encoding/json.
func decode(t *testing.T, body string) any {
	t.Helper()
	var beckn any
	if err := json.Unmarshal([]byte(body), &beckn); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	return beckn
}

// A payload naming three types yields three values, in the order the payload
// wrote them -- the order the answers come back in, which is the only thing
// making the result reproducible.
func TestFacilityTypesFromReadsThemInPayloadOrder(t *testing.T) {
	t.Parallel()

	beckn := decode(t, `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
		"supportedFacilityTypes":["KrishiVigyanKendra","Warehouse","SoilTestingFacility"]}}]}]}}}`)

	values, err := facilityTypesFrom(beckn)
	if err != nil {
		t.Fatalf("facilityTypesFrom() returned an unexpected error: %v", err)
	}
	want := []string{"KrishiVigyanKendra", "Warehouse", "SoilTestingFacility"}
	if len(values) != len(want) {
		t.Fatalf("values = %v, want %v", values, want)
	}
	for index := range want {
		if values[index] != want[index] {
			t.Errorf("values[%d] = %v, want %q -- payload order, not any other", index, values[index], want[index])
		}
	}
}

// One type is one value and one call, not a special case.
func TestFacilityTypesFromAcceptsASingleType(t *testing.T) {
	t.Parallel()

	beckn := decode(t, `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
		"supportedFacilityTypes":["Warehouse"]}}]}]}}}`)

	values, err := facilityTypesFrom(beckn)
	if err != nil {
		t.Fatalf("facilityTypesFrom() returned an unexpected error: %v", err)
	}
	if len(values) != 1 || values[0] != "Warehouse" {
		t.Errorf("values = %v, want [Warehouse]", values)
	}
}

// A payload that carries the field as a bare string rather than a list is
// accepted as one value. The mapping's own required checks wrap it in [] to
// compare it against the governed list, so a caller that satisfies those
// checks may still have written a scalar here.
func TestFacilityTypesFromAcceptsABareString(t *testing.T) {
	t.Parallel()

	beckn := decode(t, `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
		"supportedFacilityTypes":"KrishiVigyanKendra"}}]}]}}}`)

	values, err := facilityTypesFrom(beckn)
	if err != nil {
		t.Fatalf("facilityTypesFrom() returned an unexpected error: %v", err)
	}
	if len(values) != 1 || values[0] != "KrishiVigyanKendra" {
		t.Errorf("values = %v, want [KrishiVigyanKendra]", values)
	}
}

// Every shape that is not a payload this capability can serve is refused with
// a message naming what was missing -- not a nil-map panic, and not an empty
// list that would silently become zero calls.
func TestFacilityTypesFromRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body, wants string
	}{
		{"an empty object", `{}`, "commitment"},
		{"no commitments", `{"message":{"contract":{"commitments":[]}}}`, "commitment"},
		{"no resources", `{"message":{"contract":{"commitments":[{"resources":[]}]}}}`, "resource"},
		{"no resourceAttributes", `{"message":{"contract":{"commitments":[{"resources":[{}]}]}}}`, "no facility type"},
		{"an empty list", `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
			"supportedFacilityTypes":[]}}]}]}}}`, "no facility type"},
		{"a repeated type", `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
			"supportedFacilityTypes":["Warehouse","Warehouse"]}}]}]}}}`, "not repeat"},
		{"a number instead of a type", `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
			"supportedFacilityTypes":[42]}}]}]}}}`, "not a facility type"},
		{"a nested list", `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{
			"supportedFacilityTypes":[["Warehouse"]]}}]}]}}}`, "not a facility type"},
		// Two resources are refused, not half-answered. Everything downstream
		// reads resources[0] alone, so accepting this would answer for the
		// first resource's types and drop the second's with nothing anywhere
		// recording the loss.
		{"two resources", `{"message":{"contract":{"commitments":[{"resources":[
			{"resourceAttributes":{"supportedFacilityTypes":["KrishiVigyanKendra"]}},
			{"resourceAttributes":{"supportedFacilityTypes":["Warehouse"]}}]}]}}}`, "carries 2 resources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			values, err := facilityTypesFrom(decode(t, tc.body))
			if err == nil {
				t.Fatalf("facilityTypesFrom() returned %v, want %s refused", values, tc.name)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wants)
			}
		})
	}
}

// A payload that is not an object at all -- the shape a decoder handed
// something odd would produce -- is refused rather than panicking.
func TestFacilityTypesFromRefusesANonObjectPayload(t *testing.T) {
	t.Parallel()

	for _, beckn := range []any{nil, "a string", 42.0, []any{"a", "list"}} {
		if _, err := facilityTypesFrom(beckn); err == nil {
			t.Errorf("facilityTypesFrom(%v) was accepted, want it refused", beckn)
		}
	}
}
