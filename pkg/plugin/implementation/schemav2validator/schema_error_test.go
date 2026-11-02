package schemav2validator

import (
	"errors"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// Pins the one assumption errorBranches makes: that a composite's branch
// errors sit in something whose underlying kind is a slice.
func TestErrorBranches_KinOpenAPIShapes(t *testing.T) {
	t.Run("exported MultiError", func(t *testing.T) {
		branches, ok := errorBranches(openapi3.MultiError{errors.New("first"), errors.New("second")})
		if !ok {
			t.Fatal("openapi3.MultiError is no longer a slice")
		}
		if len(branches) != 2 {
			t.Fatalf("len(branches) = %d, want 2", len(branches))
		}
	})

	t.Run("unexported oneOf multierror", func(t *testing.T) {
		alpha := openapi3.NewObjectSchema()
		alpha.Required = []string{"alpha"}
		beta := openapi3.NewObjectSchema()
		beta.Required = []string{"beta"}

		parent := openapi3.NewObjectSchema()
		parent.OneOf = openapi3.SchemaRefs{
			openapi3.NewSchemaRef("", alpha),
			openapi3.NewSchemaRef("", beta),
		}

		err := parent.VisitJSON(map[string]interface{}{}, openapi3.MultiErrors())
		if err == nil {
			t.Fatal("an empty object should satisfy neither branch")
		}

		leaves := schemaErrorLeaves(err)
		if len(leaves) == 0 {
			t.Fatal("no leaves reached: kin-openapi's oneOf multierror is no longer " +
				"a slice, so errorBranches can no longer read it")
		}
		for _, leaf := range leaves {
			if leaf.SchemaField != "required" {
				t.Errorf("leaf SchemaField = %q, want \"required\"", leaf.SchemaField)
			}
		}
	})
}

// When every branch of a oneOf failed, only the closest is reported.
func TestNearest_KeepsOnlyTheClosestAlternative(t *testing.T) {
	leaf := func(field, pointer string) *openapi3.SchemaError {
		return &openapi3.SchemaError{SchemaField: field, Reason: pointer, Value: pointer}
	}
	missingAlpha := leaf("required", "alpha")
	missingBeta := leaf("required", "beta")

	closest := []*openapi3.SchemaError{missingAlpha}
	further := []*openapi3.SchemaError{missingAlpha, missingBeta}

	kept := nearest([][]*openapi3.SchemaError{closest, further})
	if len(kept) != 1 {
		t.Fatalf("len(kept) = %d, want 1; got %v", len(kept), kept)
	}
	if len(kept[0]) != 1 || kept[0][0] != missingAlpha {
		t.Errorf("kept the wrong alternative: %v", kept[0])
	}
}

func TestStrictSubset(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]bool
		want bool
	}{
		{"fewer reasons, all shared", map[string]bool{"x": true}, map[string]bool{"x": true, "y": true}, true},
		{"identical", map[string]bool{"x": true}, map[string]bool{"x": true}, false},
		{"more reasons", map[string]bool{"x": true, "y": true}, map[string]bool{"x": true}, false},
		{"fewer reasons, not shared", map[string]bool{"z": true}, map[string]bool{"x": true, "y": true}, false},
		{"no reachable cause", map[string]bool{}, map[string]bool{"x": true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strictSubset(tt.a, tt.b); got != tt.want {
				t.Errorf("strictSubset(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// The order the wire-level code is taken from.
func TestRankSchemaLeaves_OrdersByActionability(t *testing.T) {
	unsupported := &openapi3.SchemaError{SchemaField: "properties"}
	discriminator := &openapi3.SchemaError{SchemaField: "const"}
	missing := &openapi3.SchemaError{SchemaField: "required"}

	ranked := rankSchemaLeaves([]*openapi3.SchemaError{unsupported, discriminator, missing})

	want := []string{"required", "const", "properties"}
	if len(ranked) != len(want) {
		t.Fatalf("len(ranked) = %d, want %d", len(ranked), len(want))
	}
	for i, field := range want {
		if ranked[i].SchemaField != field {
			t.Errorf("ranked[%d] = %q, want %q", i, ranked[i].SchemaField, field)
		}
	}
}
