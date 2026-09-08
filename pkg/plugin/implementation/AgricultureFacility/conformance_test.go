package AgricultureFacility_test

// conformance_test.go validates what the mapping produces against the
// openagrinet:AgricultureFacility v0.1 schema pack, with a real JSON Schema
// validator.
//
// Every other test in this package asserts what the pack requires by hand, from
// a reading of attributes.yaml. That is the weakest link in the suite: a
// misreading produces a test that agrees with the mistake. This one hands the
// answer and the published schema to a validator and lets it decide.
//
// The schemas are fetched from the ref they are published under and cached
// under testdata/schema-cache, so this is offline and deterministic after a
// cold run -- see schemacache_test.go for the loader and how to refresh.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// facilitySchemaURI is the AgricultureFacility schema inside its OpenAPI
// document, addressed by the URL that actually serves it.
//
// The URI matters more than the bytes: the packs $ref each other by URL, and a
// validator resolves a ref against the base URI of the document it is written
// in. AgricultureFacility refs "../../AgricultureResource/v0.1/attributes.yaml"
// relative to its own base, so compiling under the published URL is what makes
// that resolve -- and, unlike the schemas.openagrinet.global path this used to
// be registered under, it is an address something answers.
//
// The packs are OpenAPI 3.1.1, whose schema objects are JSON Schema 2020-12, so
// a validator can compile one directly out of components.schemas.
const facilitySchemaURI = facilityPackURL + "#/components/schemas/AgricultureFacility"

// facilitySchema compiles the pack once per test.
func facilitySchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	loader := newPackLoader()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{"https": loader})

	schema, err := compiler.Compile(facilitySchemaURI)
	if err != nil {
		loader.skipIfOffline(t, err)
		t.Fatalf("could not compile the AgricultureFacility schema: %v", err)
	}

	// Both OAN documents have to have been loaded. A compile that satisfied
	// every ref without reading AgricultureResource would mean the composed
	// half of the schema went unenforced, and every result below would be
	// measured against half a pack.
	for _, required := range []string{facilityPackURL, resourcePackURL} {
		if !loader.sawLoaded(required) {
			t.Fatalf("the schema compiled without loading %s -- the pack is not the one "+
				"being validated against", required)
		}
	}
	return schema
}

// validateFacilities checks every resource in an answer against the pack, and
// reports the validator's own explanation when one fails.
func validateFacilities(t *testing.T, schema *jsonschema.Schema, answer map[string]any) int {
	t.Helper()

	resources := answerResources(t, answer)
	if len(resources) == 0 {
		t.Fatal("the answer carries no resources to validate")
	}
	for _, entry := range resources {
		id, _ := dig(entry, "id").(string)
		attributes := dig(entry, "resourceAttributes")
		if attributes == nil {
			t.Fatalf("resource %s carries no resourceAttributes", id)
		}
		if err := schema.Validate(attributes); err != nil {
			// The validator's message names the failing keyword and the
			// instance location, which is the whole reason for this test.
			t.Errorf("resource %s does not satisfy AgricultureFacility v0.1:\n%v\n%s",
				id, err, mustIndent(t, attributes))
		}
	}
	return len(resources)
}

func TestAnswerSatisfiesTheSchemaPack(t *testing.T) {
	schema := facilitySchema(t)

	for _, tc := range []struct {
		name         string
		facilityType string
		provider     string
	}{
		// The three that carry their own category tag.
		{"a KVK search", "KrishiVigyanKendra", providerResponse},
		// The one that does not, and whose type is derived from its provider's
		// GSW fulfillment. This is the case that was silently omitting a
		// REQUIRED field, so it is the reason this test exists.
		{"a warehouse search", "Warehouse", warehouseResponse},
		// A mixed answer, so the filtered survivors are validated too.
		{"a filtered mixed answer", "CustomHiringCentre", mixedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := runAgainst(t, requestFor(t, tc.facilityType), tc.provider)
			count := validateFacilities(t, schema, answer)
			t.Logf("%d %s facilities validated against AgricultureFacility v0.1",
				count, tc.facilityType)
		})
	}
}

// A resource missing facilityType must be REFUSED by the validator. Without
// this, a green suite would not distinguish "the pack is satisfied" from "the
// validator is not actually checking anything".
func TestTheValidatorRefusesAnIncompleteFacility(t *testing.T) {
	schema := facilitySchema(t)

	answer := runAgainst(t, requestFor(t, "Warehouse"), warehouseResponse)
	attributes, ok := dig(answerResources(t, answer)[0], "resourceAttributes").(map[string]any)
	if !ok {
		t.Fatal("the first resource carries no resourceAttributes")
	}
	if err := schema.Validate(attributes); err != nil {
		t.Fatalf("the unmodified answer should validate: %v", err)
	}

	for _, required := range []string{"facilityType", "informationMode", "source"} {
		t.Run("without "+required, func(t *testing.T) {
			broken := map[string]any{}
			for key, value := range attributes {
				if key != required {
					broken[key] = value
				}
			}
			if err := schema.Validate(broken); err == nil {
				t.Errorf("the validator accepted a Direct resource with no %s, "+
					"so it is not enforcing the pack", required)
			}
		})
	}

	// And the whole point of the GSW fallback: the value has to be one the pack
	// governs, not merely present.
	t.Run("with an ungoverned facilityType", func(t *testing.T) {
		broken := map[string]any{}
		for key, value := range attributes {
			broken[key] = value
		}
		broken["facilityType"] = "TractorShed"
		if err := schema.Validate(broken); err == nil {
			t.Error("the validator accepted a facilityType outside the governed enum")
		}
	})
}

func mustIndent(t *testing.T, value any) string {
	t.Helper()
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(pretty)
}

// packExamples are the schema pack's own published examples, by file name.
//
// Listed rather than discovered, so a lost example is a failure rather than a
// shorter loop. Both information modes are published and they have disjoint
// required sets -- OnDemand forbids most of what Direct requires -- so the set
// is what gives the check below its reach.
var packExamples = []string{
	"custom-hiring-centre.json",
	"krishi-vigyan-kendra.json",
	"on-demand-facility-discovery.json",
	"soil-testing-facility.json",
	"warehouse.json",
}

// The pack's OWN examples must satisfy the schema as this test compiles it.
//
// This checks the compilation rather than the mapping. Everything else here
// validates what this plugin produces, which cannot distinguish "the answer is
// right" from "the schema was compiled too loosely to reject anything". If the
// pack's published examples fail, the compilation is wrong and every other
// conformance result in this file is worth nothing.
//
// It is also a drift alarm: the examples are fetched from the same ref as the
// schemas, so one that stops validating means the pack moved under this plugin.
func TestThePacksOwnExamplesValidate(t *testing.T) {
	schema := facilitySchema(t)
	loader := newPackLoader()

	for _, name := range packExamples {
		t.Run(name, func(t *testing.T) {
			url := packExampleBase + "/" + name
			instance, err := loader.Load(url)
			if err != nil {
				loader.skipIfOffline(t, err)
				t.Fatalf("could not read %s: %v", url, err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Errorf("the pack's own example %s does not satisfy the schema as compiled here, "+
					"so the compilation is wrong:\n%v", name, err)
			}
		})
	}
}

// governedFields are the property names AgricultureFacility v0.1 declares,
// across its own branch and the AgricultureResource it composes with.
//
// @context is in the list and is NOT a schema property -- it appears only under
// x-jsonld, which a validator ignores. Every one of the pack's own examples
// carries it, so it is conventional rather than governed, and it is listed here
// deliberately rather than by oversight.
var governedFields = map[string]bool{
	"@context":               true, // conventional, see above
	"@type":                  true,
	"informationMode":        true,
	"subjectCategories":      true,
	"agricultureSubjects":    true,
	"languages":              true,
	"coverageAreas":          true,
	"supportedFacilityTypes": true,
	"facilityType":           true,
	"location":               true,
	"address":                true,
	"services":               true,
	"capacity":               true,
	"publicContact":          true,
	"website":                true,
	"source":                 true,
	"lastUpdatedAt":          true,
}

// Nothing this mapping emits may be a field the pack does not declare.
//
// The validator cannot check this. Neither AgricultureFacility nor
// AgricultureResource sets additionalProperties: false at the top level, so an
// invented field passes validation in silence -- and publishing an ungoverned
// field under a governed schema is how a private convention leaks onto the
// network and becomes load-bearing for somebody else.
//
// The nested types DO forbid extras -- SourceReference, Address,
// FacilityCapacity, Contact and Descriptor all set additionalProperties: false
// -- so the validator already covers everything inside source, address,
// capacity and publicContact. This test covers the one level it cannot.
func TestTheAnswerInventsNoUngovernedField(t *testing.T) {
	for _, tc := range []struct {
		name         string
		facilityType string
		provider     string
	}{
		{"a KVK search", "KrishiVigyanKendra", providerResponse},
		{"a warehouse search", "Warehouse", warehouseResponse},
		{"a filtered mixed answer", "CustomHiringCentre", mixedResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := runAgainst(t, requestFor(t, tc.facilityType), tc.provider)
			for _, entry := range answerResources(t, answer) {
				id, _ := dig(entry, "id").(string)
				attributes, ok := dig(entry, "resourceAttributes").(map[string]any)
				if !ok {
					t.Fatalf("resource %s carries no resourceAttributes", id)
				}
				for field := range attributes {
					if !governedFields[field] {
						t.Errorf("%s carries %q, which AgricultureFacility v0.1 does not declare. "+
							"The top level sets no additionalProperties, so the validator will not "+
							"catch it -- either the pack governs it and this list is stale, or the "+
							"mapping is inventing a field", id, field)
					}
				}
			}
		})
	}
}

// The list above has to match the pack, or the test above proves nothing.
//
// Read straight out of the vendored schema rather than trusted: a field added
// upstream should widen what the mapping may emit, and a field removed should
// narrow it, without either going unnoticed.
func TestTheGovernedFieldListMatchesThePack(t *testing.T) {
	loader := newPackLoader()

	declared := map[string]bool{"@context": true} // conventional, not a property
	for _, section := range []struct{ url, schema, until string }{
		{facilityPackURL, "    AgricultureFacility:", "    FacilityType:"},
		{resourcePackURL, "    AgricultureResource:", "    AdministrativeAreaReference:"},
	} {
		// Read as text, not as a compiled schema: the point is which property
		// names the document declares, and a compiled schema has already
		// flattened its allOf branches together.
		raw, err := loader.text(section.url)
		if err != nil {
			loader.skipIfOffline(t, err)
			t.Fatalf("could not read %s: %v", section.url, err)
		}

		text := string(raw)
		start := strings.Index(text, section.schema)
		end := strings.Index(text, section.until)
		if start < 0 || end < 0 || end < start {
			t.Fatalf("could not locate %s in %s", section.schema, section.url)
		}
		for _, name := range propertyNames(text[start:end]) {
			declared[name] = true
		}
	}

	for field := range governedFields {
		if !declared[field] {
			t.Errorf("governedFields lists %q, which the pack does not declare", field)
		}
	}
	for field := range declared {
		if !governedFields[field] {
			t.Errorf("the pack declares %q, which governedFields omits -- the mapping is "+
				"permitted less than the pack allows", field)
		}
	}
}

// propertyNames pulls the property names out of a schema section's properties
// blocks, keyed off indentation so nested object properties are not mistaken
// for top-level ones.
func propertyNames(section string) []string {
	var names []string
	lines := strings.Split(section, "\n")
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "properties:" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for _, next := range lines[index+1:] {
			if strings.TrimSpace(next) == "" {
				continue
			}
			nextIndent := len(next) - len(strings.TrimLeft(next, " "))
			if nextIndent <= indent {
				break
			}
			if nextIndent != indent+2 {
				continue
			}
			name := strings.TrimSuffix(strings.TrimSpace(next), ":")
			name = strings.Trim(name, `"`)
			if name != "" && !strings.Contains(name, " ") {
				names = append(names, name)
			}
		}
	}
	return names
}

// The INBOUND query resource must satisfy the pack too, in OnDemand mode.
//
// Every other conformance test here validates the answer. Nothing validated the
// request, and the request is where this plugin made its most debatable choice:
// the pack forbids an OnDemand resource from carrying location, address or
// facilityType, so the search origin had to go somewhere else -- it is read from
// the Beckn fulfillment stop.
//
// If the fixture the whole suite is built on is not a valid OnDemand
// AgricultureFacility, then the convention is wrong and every test that uses it
// is testing the wrong shape.
func TestTheRequestResourceSatisfiesOnDemandMode(t *testing.T) {
	schema := facilitySchema(t)

	var payload map[string]any
	if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	attributes := attributes(t, payload)

	if attributes["informationMode"] != "OnDemand" {
		t.Fatalf("informationMode = %v, want OnDemand for a query resource",
			attributes["informationMode"])
	}
	if err := schema.Validate(attributes); err != nil {
		t.Fatalf("the request resource does not satisfy AgricultureFacility v0.1 in OnDemand mode:\n%v\n%s",
			err, mustIndent(t, attributes))
	}

	// And the reason the query lives in the fulfillment stop rather than here.
	// OnDemand forbids each of these outright, so putting the search origin in
	// resourceAttributes would make every request invalid.
	for _, forbidden := range []string{"facilityType", "location", "address", "services",
		"capacity", "publicContact", "website", "source", "lastUpdatedAt"} {
		broken := map[string]any{}
		for key, value := range attributes {
			broken[key] = value
		}
		broken[forbidden] = map[string]any{"type": "Point", "coordinates": []any{74.5321, 19.5132}}
		if err := schema.Validate(broken); err == nil {
			t.Errorf("OnDemand accepted %q, but the pack forbids it -- if that is now allowed, "+
				"the search origin could move into resourceAttributes", forbidden)
		}
	}
}

// The pack's README carries two mapping rules a validator cannot enforce, and
// both bind this plugin directly. They are asserted here because the schema is
// silent on them: an answer that breaks either still validates.
//
//	"An adapter must not copy a request coordinate into location unless the
//	 Provider confirms that the returned coordinate belongs to the facility."
//
//	"Query-relative distance, ranking, and price are not intrinsic facility
//	 attributes. Distance belongs in result metadata. Price and booking terms
//	 belong in the applicable Beckn offer or transaction contract."
//
// POCRA supplies material for every one of these -- a stub gps, a distance
// string, and a price and rating on warehouse items -- so each is a live
// temptation rather than a hypothetical.
func TestTheAnswerFollowsThePacksMappingRules(t *testing.T) {
	for _, tc := range []struct {
		name         string
		facilityType string
		provider     string
	}{
		{"a KVK search", "KrishiVigyanKendra", providerResponse},
		// The warehouse fixture is the one carrying price and rating.
		{"a warehouse search", "Warehouse", warehouseResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := runAgainst(t, requestFor(t, tc.facilityType), tc.provider)

			for _, entry := range answerResources(t, answer) {
				id, _ := dig(entry, "id").(string)
				attributes, ok := dig(entry, "resourceAttributes").(map[string]any)
				if !ok {
					t.Fatalf("resource %s carries no resourceAttributes", id)
				}

				// No coordinate. POCRA publishes one gps per answer and it is a
				// fixed stub unrelated to the point asked for, so there is no
				// verified facility geometry to carry -- and the search origin
				// must not be substituted for one.
				if _, present := attributes["location"]; present {
					t.Errorf("%s carries location; the pack forbids substituting the search origin "+
						"and POCRA confirms no facility coordinate", id)
				}

				// No price, no rating, no distance, under any spelling.
				encoded, err := json.Marshal(attributes)
				if err != nil {
					t.Fatalf("could not re-encode %s: %v", id, err)
				}
				for _, forbidden := range []string{"price", "rating", "distance",
					"estimated_value", "minimum_value", "Km", " km"} {
					if strings.Contains(string(encoded), forbidden) {
						t.Errorf("%s carries %q; query-relative distance, ranking and price are not "+
							"facility attributes and belong in the Beckn offer or in result metadata",
							id, forbidden)
					}
				}
			}
		})
	}
}
