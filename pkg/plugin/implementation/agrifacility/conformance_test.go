package agrifacility_test

// conformance_test.go validates what the mapping produces against the
// openagrinet:AgricultureFacility v0.1 schema pack, with a real JSON Schema
// validator.
//
// Every other test in this package asserts what the pack requires by hand, from
// a reading of attributes.yaml. That is the weakest link in the suite: a
// misreading produces a test that agrees with the mistake. This one hands the
// answer and the published schema to a validator and lets it decide.
//
// The schemas are vendored under testdata/schemas so this is offline and
// deterministic -- see the README there for provenance and how to refresh.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// schemaURIs maps a schema document's own info.title to every URI it must be
// registered under.
//
// Keyed on the title rather than on position in the file, so the documents can
// be reordered or one added without silently pairing a schema with the wrong
// URI.
//
// The URI matters more than the file: the packs $ref each other by URL, and a
// validator resolves a ref by the URL written rather than the one finally
// served. AgricultureFacility refs "../../AgricultureResource/v0.1/..."
// relative to its own base, so registering it under the schemas.openagrinet
// path is what makes that resolve.
//
// The beckn schemas need BOTH forms. schema.beckn.io 301-redirects to
// schema.nfh.global, and the two sides disagree about which to write: the OAN
// packs ref beckn.io, while Location refs its own siblings by nfh.global. Each
// document is therefore registered under both, so a ref resolves whichever name
// it was written with rather than reaching for the network.
var schemaURIs = map[string][]string{
	"OpenAgriNet - Agriculture Facility Attributes": {
		"https://schemas.openagrinet.global/schema/AgricultureFacility/v0.1/attributes.yaml"},
	"OpenAgriNet - Agriculture Resource Attributes": {
		"https://schemas.openagrinet.global/schema/AgricultureResource/v0.1/attributes.yaml"},
	"Address": {
		"https://schema.beckn.io/Address/v2.0/attributes.yaml",
		"https://schema.nfh.global/Address/v2.0/attributes.yaml"},
	"Contact": {
		"https://schema.beckn.io/Contact/v2.0/attributes.yaml",
		"https://schema.nfh.global/Contact/v2.0/attributes.yaml"},
	"Descriptor": {
		"https://schema.beckn.io/Descriptor/v2.1/attributes.yaml",
		"https://schema.nfh.global/Descriptor/v2.1/attributes.yaml"},
	// Reached transitively from Descriptor, which refs images and documents.
	"MediaFile": {
		"https://schema.beckn.io/MediaFile/v2.0/attributes.yaml",
		"https://schema.nfh.global/MediaFile/v2.0/attributes.yaml"},
	"Document": {
		"https://schema.beckn.io/Document/v2.0/attributes.yaml",
		"https://schema.nfh.global/Document/v2.0/attributes.yaml"},
	"GeoJSONGeometry": {
		"https://schema.beckn.io/GeoJSONGeometry/v2.0/attributes.yaml",
		"https://schema.nfh.global/GeoJSONGeometry/v2.0/attributes.yaml"},
	"Location": {
		"https://schema.beckn.io/Location/v2.0/attributes.yaml",
		"https://schema.nfh.global/Location/v2.0/attributes.yaml"},
}

// schemaStream is every schema the pack reaches, as one multi-document YAML
// file. Each document in it is verbatim; only the "---" separators were added.
const schemaStream = "testdata/schemas.yaml"

// facilitySchemaURI is the AgricultureFacility schema inside its OpenAPI
// document. The packs are OpenAPI 3.1.1, whose schema objects are JSON Schema
// 2020-12, so a validator can compile one directly out of components.schemas.
const facilitySchemaURI = "https://schemas.openagrinet.global/schema/AgricultureFacility/v0.1/attributes.yaml#/components/schemas/AgricultureFacility"

// facilitySchema compiles the pack once for the whole test file.
func facilitySchema(t *testing.T) *jsonschema.Schema {
	t.Helper()

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)

	file, err := os.Open(schemaStream)
	if err != nil {
		t.Fatalf("could not open %s: %v", schemaStream, err)
	}
	defer file.Close()

	registered := 0
	decoder := yaml.NewDecoder(file)
	for index := 0; ; index++ {
		var parsed any
		err := decoder.Decode(&parsed)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s document %d is not YAML: %v", schemaStream, index, err)
		}
		if parsed == nil {
			continue
		}

		title := documentTitle(t, index, parsed)
		uris, known := schemaURIs[title]
		if !known {
			t.Fatalf("%s document %d has info.title %q, which no URI is registered for. "+
				"Add it to schemaURIs, or the schema it defines will be fetched over "+
				"the network instead", schemaStream, index, title)
		}

		// The packs are published as YAML and the validator takes JSON, so this
		// is a format conversion and nothing more -- no key is renamed and no
		// value is touched.
		asJSON, err := json.Marshal(parsed)
		if err != nil {
			t.Fatalf("%s (%s) could not be converted to JSON: %v", title, schemaStream, err)
		}
		for _, uri := range uris {
			// Unmarshalled per URI: AddResource takes ownership of the value,
			// so two registrations must not share one.
			resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
			if err != nil {
				t.Fatalf("%s could not be read as a JSON document: %v", title, err)
			}
			if err := compiler.AddResource(uri, resource); err != nil {
				t.Fatalf("could not register %s as %s: %v", title, uri, err)
			}
		}
		registered++
	}

	// Every schema in the map has to have been found. A document silently
	// dropped from the stream would otherwise surface as a network fetch, or as
	// a compile error naming a URL rather than the missing document.
	if registered != len(schemaURIs) {
		t.Fatalf("%s carried %d documents, want %d -- one has been dropped",
			schemaStream, registered, len(schemaURIs))
	}

	schema, err := compiler.Compile(facilitySchemaURI)
	if err != nil {
		t.Fatalf("could not compile the AgricultureFacility schema: %v", err)
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

// documentTitle reads a schema document's own info.title, which is how the
// stream self-identifies.
func documentTitle(t *testing.T, index int, document any) string {
	t.Helper()
	object, ok := document.(map[string]any)
	if !ok {
		t.Fatalf("%s document %d is not a mapping", schemaStream, index)
	}
	info, ok := object["info"].(map[string]any)
	if !ok {
		t.Fatalf("%s document %d carries no info block", schemaStream, index)
	}
	title, ok := info["title"].(string)
	if !ok || title == "" {
		t.Fatalf("%s document %d carries no info.title", schemaStream, index)
	}
	return title
}
