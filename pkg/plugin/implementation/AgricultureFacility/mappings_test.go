package AgricultureFacility_test

// mappings_test.go runs the shipped agriculture facility mapping through the
// real mapper and the real provider step. It is the only test that proves the
// three pieces fit: a mapping is JSONata inside YAML fetched over HTTP, and
// nothing but running it establishes that what is published actually produces
// valid Beckn.
//
// An external test package on purpose -- it uses the plugins exactly as the
// adapter does, through their exported surface and nothing else.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/AgricultureFacility"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// mappingsDir is where the shipped mappings live, relative to this package.
// Reading the published file rather than a fixture is the whole point: this test
// breaks when what is deployed breaks.
const mappingsDir = "../../../../config/mappings/pocra"

// shippedMapping is the file this binding-action publishes: one file, both
// directions. The action segment of the name must match the action the registry
// entry declares -- a mismatch would apply a correct mapping to the wrong call,
// silently.
const shippedMapping = "agriculture-facility.select.yaml"

// shippedCapability is what the pack calls this capability, and the second half
// of the binding key the registry indexes the provider's record by.
const shippedCapability = "openagrinet:AgricultureFacility"

const shippedBindingKey = "pocra|" + shippedCapability

// selectRequest is a facility search in OnDemand mode.
//
// The query lives in the Beckn layer, not in resourceAttributes: the pack
// forbids an OnDemand resource from carrying location or facilityType, so the
// search origin is a fulfillment stop and the requested type is
// supportedFacilityTypes.
//
// PROVISIONAL: this convention was chosen on the design and has not yet been
// confirmed against a payload captured from the network.
// declaredContext is the @context the fixture declares, and the one the answer
// must echo back.
//
// It is the pack's published URL because that URL resolves. The identifier the
// pack names in its own x-jsonld, https://schemas.openagrinet.global/..., does
// not: the host has no DNS record, so a JSON-LD processor handed it can fetch
// nothing. Both are exercised in TestShippedMappingEchoesTheCallersContext,
// since the mapping must not care which it is given.
const declaredContext = "https://raw.githubusercontent.com/OpenAgriNet/network-specs/schema-packs-v0.1/schema/AgricultureFacility/v0.1/context.jsonld"

// callerMessageID is the messageId selectRequest declares. Named so the
// fan-out assertion that no upstream call reuses it reads from one place rather
// than repeating the literal.
const callerMessageID = "a1b2c3d4-e5f6-4789-abcd-ef1234567890"

const selectRequest = `{
  "context": { "version": "2.0.0", "action": "select",
    "networkId": "oan-dev",
    "transactionId": "f9e8d7c6-b5a4-4321-9876-543210fedcba",
    "messageId": "a1b2c3d4-e5f6-4789-abcd-ef1234567890",
    "timestamp": "2026-09-02T06:12:01.330Z" },
  "message": { "contract": { "commitments": [{
    "status": { "descriptor": { "code": "ACTIVE", "name": "active" } },
    "resources": [{
      "id": "res:pocra:facility-search",
      "quantity": { "count": 1 },
      "resourceAttributes": {
        "@context": "https://raw.githubusercontent.com/OpenAgriNet/network-specs/schema-packs-v0.1/schema/AgricultureFacility/v0.1/context.jsonld",
        "@type": "openagrinet:AgricultureFacility",
        "informationMode": "OnDemand",
        "subjectCategories": ["Facility"],
        "supportedFacilityTypes": ["KrishiVigyanKendra"]
      }
    }],
    "offer": {
      "id": "offer:pocra:common-services",
      "resourceIds": ["res:pocra:facility-search"],
      "provider": { "id": "pocra",
                    "descriptor": { "code": "POCRA-01", "name": "PoCRA Provider Aggregator" } }
    },
    "fulfillment": {
      "stops": [{
        "location": { "geo": { "type": "Point", "coordinates": [74.5321, 19.5132] } },
        "time": { "range": { "start": "2026-09-19T08:00:00.108Z" } }
      }]
    }
  }] } }
}`

// providerResponse is POCRA's own shape, trimmed from a body captured against
// the live API (not checked into this repo).
//
// TWO responses[] entries, because POCRA returns one per answering BPP and the
// captured sample carried four. The second repeats COMMON-55043, which is what
// the dedupe has to collapse.
//
// Distances are deliberately out of order, and both items carry the placeholder
// values POCRA really sends: "Unknown", "N/A" and "000000".
const providerResponse = `{
  "context": { "action": "search", "version": "1.1.0" },
  "responses": [
    {
      "context": { "action": "on_search", "version": "1.1.0" },
      "message": { "catalog": {
        "descriptor": { "name": "Pocra Provider Aggregator Services" },
        "providers": [{
          "id": "COMMON_PROVIDER_KVK",
          "fulfillments": [{ "id": "f1_kvk", "type": "Service",
            "locations": { "id": "l1", "gps": "17.385,78.4867" },
            "categories": [{ "id": "c_kvk", "name": "KVK",
                             "descriptor": { "code": "kvk", "name": "KVK" } }] }],
          "items": [
            {
              "id": "COMMON-55043",
              "descriptor": { "name": "Krishi Vigyan Kendra, Biloli" },
              "address": { "address": "Krishi Vigyan Kendra, Village- Sagroli, Taluka- Biloli, Distt.-Nanded",
                           "district": "Nanded", "region": "Unknown", "taluka": "Unknown",
                           "vilage": "Unknown", "pinCode": "000000" },
              "contact": { "person": "Chairman, Sanskriti Samvardhan Mandal, Sagroli",
                           "email": "N/A", "phone": "N/A",
                           "webUrl": "https://provider.mahapocra.gov.in" },
              "fulfillment_ids": ["f1_kvk"],
              "category_ids": ["c_kvk"],
              "tags": [{ "list": [
                { "descriptor": { "code": "distance" }, "value": "165 Km" },
                { "descriptor": { "code": "organization" }, "value": "Krishi Vigyan Kendra" },
                { "descriptor": { "code": "category" }, "value": "kvk" } ] }]
            },
            {
              "id": "COMMON-55007",
              "descriptor": { "name": "Krishi Vigyan Kendra, Latur" },
              "address": { "address": "Krishi Vigyan Kendra, Chincholirao Wadi, MIDC, Latur-413512",
                           "district": "Latur", "region": "Unknown", "taluka": "Unknown",
                           "vilage": "Unknown", "pinCode": "000000" },
              "contact": { "person": "N/A", "email": "N/A", "phone": "N/A",
                           "webUrl": "https://provider.mahapocra.gov.in" },
              "fulfillment_ids": ["f1_kvk"],
              "category_ids": ["c_kvk"],
              "tags": [{ "list": [
                { "descriptor": { "code": "distance" }, "value": "98 Km" },
                { "descriptor": { "code": "category" }, "value": "kvk" } ] }]
            }
          ]
        }]
      } }
    },
    {
      "context": { "action": "on_search", "version": "1.1.0" },
      "message": { "catalog": {
        "descriptor": { "name": "Pocra Provider Aggregator Services" },
        "providers": [{
          "id": "COMMON_PROVIDER_KVK",
          "fulfillments": [{ "id": "f1_kvk", "type": "Service",
            "locations": { "id": "l1", "gps": "17.385,78.4867" },
            "categories": [{ "id": "c_kvk", "name": "KVK",
                             "descriptor": { "code": "kvk", "name": "KVK" } }] }],
          "items": [
            {
              "id": "COMMON-55043",
              "descriptor": { "name": "Krishi Vigyan Kendra, Biloli" },
              "address": { "address": "Krishi Vigyan Kendra, Village- Sagroli, Taluka- Biloli, Distt.-Nanded",
                           "district": "Nanded", "region": "Unknown", "taluka": "Unknown",
                           "vilage": "Unknown", "pinCode": "000000" },
              "contact": { "person": "Chairman, Sanskriti Samvardhan Mandal, Sagroli",
                           "email": "N/A", "phone": "N/A",
                           "webUrl": "https://provider.mahapocra.gov.in" },
              "fulfillment_ids": ["f1_kvk"],
              "category_ids": ["c_kvk"],
              "tags": [{ "list": [
                { "descriptor": { "code": "distance" }, "value": "165 Km" },
                { "descriptor": { "code": "category" }, "value": "kvk" } ] }]
            }
          ]
        }]
      } }
    }
  ]
}`

// serveMappings publishes the shipped mapping files over HTTP, which is how the
// mapper fetches them in production.
func serveMappings(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(filepath.Join(mappingsDir, filepath.Base(r.URL.Path)))
		if err != nil {
			t.Errorf("could not read the mapping %q: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, string(body))
	}))
}

// stubRegistry answers with the call plan the live registry holds for this
// capability.
type stubRegistry struct{ plan *model.ProviderRecord }

func (s *stubRegistry) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	return s.plan, nil
}

// runSelect drives the real step over the real mapping against a fake POCRA, and
// returns the body POCRA was sent alongside the answer.
//
// It returns the error rather than failing on it, because refusing a payload is
// as much of this mapping's job as serving one.
func runSelect(t *testing.T, request string) (sent map[string]any, answer map[string]any, runErr error) {
	t.Helper()

	mappings := serveMappings(t)
	defer mappings.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("POCRA was sent something that is not JSON: %v", err)
		}
		fmt.Fprint(w, providerResponse)
	}))
	defer upstream.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey:     shippedBindingKey,
		ParticipantID:  "pocra",
		CapabilityCode: shippedCapability,
		BaseURL:        upstream.URL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodPost, Path: "/search",
				Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000, RetryMax: 2},
		},
	}}

	step, closeStep, err := AgricultureFacility.New(context.Background(), registry, mapper,
		&AgricultureFacility.Config{
			BindingKeys: []string{shippedBindingKey},
			// Auth is per provider now; POCRA needs no credential and says so.
			AuthByProvider: map[string]*common.AuthProfile{
				strings.Split(shippedBindingKey, "|")[0]: {Scheme: util.AuthSchemeNone},
			},
		})
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	defer closeStep()

	stepCtx := &model.StepContext{Context: t.Context(), Body: []byte(request)}
	runErr = step.Run(stepCtx)
	if runErr != nil {
		return sent, nil, runErr
	}
	if len(stepCtx.ResponseBody) == 0 {
		t.Fatal("the step produced no answer")
	}
	if err := json.Unmarshal(stepCtx.ResponseBody, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}
	return sent, answer, nil
}

// dig walks a decoded JSON object by key, returning nil at the first miss so a
// failing assertion reports the path rather than panicking.
func dig(node any, path ...string) any {
	for _, key := range path {
		object, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = object[key]
	}
	return node
}

// answerResources returns the resources of the single commitment in an answer.
func answerResources(t *testing.T, answer map[string]any) []any {
	t.Helper()
	commitments, ok := dig(answer, "message", "contract", "commitments").([]any)
	if !ok || len(commitments) != 1 {
		t.Fatalf("got %d commitments, want 1", len(commitments))
	}
	resources, ok := dig(commitments[0], "resources").([]any)
	if !ok {
		t.Fatal("the quoted commitment carries no resources")
	}
	return resources
}

func TestShippedMappingSendsWhatPocraExpects(t *testing.T) {
	sent, _, err := runSelect(t, selectRequest)
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	stops, _ := dig(sent, "message", "intent", "fulfillment", "stops").([]any)
	if len(stops) != 1 {
		t.Fatalf("got %d stops, want 1", len(stops))
	}
	// GPS is "lat,lon" and OAN GeoJSON is [lon, lat]. Reading them the same way
	// round gives a valid request for a point in the wrong place, so it fails as
	// wrong data rather than as an error.
	if gps := dig(stops[0], "location", "gps"); gps != "19.5132,74.5321" {
		t.Errorf("gps = %v, want 19.5132,74.5321 in lat,lon order", gps)
	}
	if start := dig(stops[0], "time", "range", "start"); start != "2026-09-19T08:00:00.108Z" {
		t.Errorf("time.range.start = %v, want the stop's own start", start)
	}

	// The governed type is translated into POCRA's private vocabulary here and
	// nowhere else.
	if code := dig(sent, "message", "intent", "category", "descriptor", "code"); code != "kvk" {
		t.Errorf("category code = %v, want kvk for KrishiVigyanKendra", code)
	}
	if name := dig(sent, "message", "intent", "item", "descriptor", "name"); name != "service-locations" {
		t.Errorf("item name = %v, want service-locations", name)
	}

	// POCRA's request contract, pinned. These are not this adapter's identity,
	// which travels in the signed Authorization header.
	for _, f := range []struct{ key, want string }{
		{"domain", "advisory:mh-vistaar"},
		{"action", "search"},
		{"version", "1.1.0"},
		{"bap_id", "bap.mahapocra.gov.in"},
		{"bap_uri", "https://middleware.mahapocra.gov.in/bap/"},
	} {
		if got := dig(sent, "context", f.key); got != f.want {
			t.Errorf("context.%s = %v, want %v", f.key, got, f.want)
		}
	}

	// Correlation survives, so POCRA's logs can be matched to ours.
	if got := dig(sent, "context", "transaction_id"); got != "f9e8d7c6-b5a4-4321-9876-543210fedcba" {
		t.Errorf("transaction_id = %v, want the one from the request", got)
	}

	// Epoch seconds as a string, unlike every other timestamp in either
	// protocol. A wrong shape here is one of POCRA's schema NACKs.
	timestamp, ok := dig(sent, "context", "timestamp").(string)
	if !ok {
		t.Fatalf("context.timestamp = %v, want a string", dig(sent, "context", "timestamp"))
	}
	if strings.ContainsAny(timestamp, "-:") {
		t.Errorf("context.timestamp = %q, want epoch seconds rather than ISO 8601", timestamp)
	}
	if len(timestamp) < 10 {
		t.Errorf("context.timestamp = %q, want at least 10 digits of epoch seconds", timestamp)
	}
}

// commitment reaches into a decoded payload, which is how the refusal cases are
// built.
//
// Editing the fixture's TEXT instead would be a trap: removing the last member
// of an object leaves a trailing comma, and the resulting parse error would look
// like a mapping failure rather than the case under test.
func commitment(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	message, ok := payload["message"].(map[string]any)
	if !ok {
		t.Fatal("the fixture carries no message")
	}
	contract, ok := message["contract"].(map[string]any)
	if !ok {
		t.Fatal("the fixture carries no contract")
	}
	commitments, ok := contract["commitments"].([]any)
	if !ok || len(commitments) == 0 {
		t.Fatal("the fixture carries no commitments")
	}
	first, ok := commitments[0].(map[string]any)
	if !ok {
		t.Fatal("the first commitment is not an object")
	}
	return first
}

// attributes reaches the resourceAttributes of a decoded payload.
func attributes(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	resources, ok := commitment(t, payload)["resources"].([]any)
	if !ok || len(resources) == 0 {
		t.Fatal("the fixture carries no resources")
	}
	resource, ok := resources[0].(map[string]any)
	if !ok {
		t.Fatal("the first resource is not an object")
	}
	found, ok := resource["resourceAttributes"].(map[string]any)
	if !ok {
		t.Fatal("the first resource carries no resourceAttributes")
	}
	return found
}

func TestShippedMappingRefusesWhatItCannotServe(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(t *testing.T, payload map[string]any)
		wants  string
	}{
		{
			name: "no fulfillment stop at all",
			mutate: func(t *testing.T, payload map[string]any) {
				commitment(t, payload)["fulfillment"] = map[string]any{"stops": []any{}}
			},
			wants: "Point",
		},
		{
			name: "a polygon rather than a point",
			mutate: func(t *testing.T, payload map[string]any) {
				fulfillment := commitment(t, payload)["fulfillment"].(map[string]any)
				stop := fulfillment["stops"].([]any)[0].(map[string]any)
				stop["location"].(map[string]any)["geo"].(map[string]any)["type"] = "Polygon"
			},
			wants: "Point",
		},
		{
			name: "a facility type nobody governs",
			mutate: func(t *testing.T, payload map[string]any) {
				attributes(t, payload)["supportedFacilityTypes"] = []any{"TractorShed"}
			},
			wants: "facility type",
		},
		{
			name: "no facility type at all",
			mutate: func(t *testing.T, payload map[string]any) {
				delete(attributes(t, payload), "supportedFacilityTypes")
			},
			wants: "facility type",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			tc.mutate(t, payload)
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("could not rebuild the payload: %v", err)
			}

			sent, _, runErr := runSelect(t, string(body))
			if runErr == nil {
				t.Fatal("expected the mapping's precondition to refuse this payload")
			}
			if !strings.Contains(runErr.Error(), tc.wants) {
				t.Errorf("error %q should explain the problem by mentioning %q", runErr, tc.wants)
			}
			// The refusal has to happen before the call. Reaching POCRA with a
			// payload we already know it cannot serve wastes its budget and
			// turns our own bad request into its error message.
			if sent != nil {
				t.Errorf("POCRA was called with %v; a precondition must refuse before the call", sent)
			}
		})
	}
}

func TestShippedMappingAnswersWithBeckn(t *testing.T) {
	_, answer, err := runSelect(t, selectRequest)
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	if got := dig(answer, "context", "action"); got != "on_select" {
		t.Errorf("action = %v, want on_select", got)
	}
	if got := dig(answer, "context", "transactionId"); got != "f9e8d7c6-b5a4-4321-9876-543210fedcba" {
		t.Errorf("transactionId = %v, want the one from the request", got)
	}
	if resources := answerResources(t, answer); len(resources) == 0 {
		t.Fatal("the answer carries no resources")
	}
}

// POCRA returns one responses[] entry per answering BPP, and the same facility
// appears in more than one of them. A repeated id in the answer would be a
// broken reference the moment anything resolved it, so they collapse to one.
//
// Ordering is POCRA's own ranking, which arrives only as a distance string on
// each item. It is used to sort and then dropped: the schema pack states that
// query-relative distance is not an intrinsic facility attribute.
func TestShippedMappingDedupesAndOrdersByDistance(t *testing.T) {
	_, answer, err := runSelect(t, selectRequest)
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	resources := answerResources(t, answer)

	// Three items arrive across two responses; COMMON-55043 appears twice.
	if len(resources) != 2 {
		t.Fatalf("got %d resources, want 2 -- COMMON-55043 arrives twice and must collapse", len(resources))
	}

	ids := make([]string, 0, len(resources))
	for _, entry := range resources {
		id, _ := dig(entry, "id").(string)
		ids = append(ids, id)
	}

	// 98 Km before 165 Km. Sorting the strings rather than the numbers would put
	// "165 Km" first, which nothing downstream could detect.
	want := []string{"res:pocra:facility:COMMON-55007", "res:pocra:facility:COMMON-55043"}
	for i, expected := range want {
		if ids[i] != expected {
			t.Errorf("resource %d = %q, want %q -- nearest first", i, ids[i], expected)
		}
	}

	// Distance must not survive into the answer in any form.
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("could not re-encode the answer: %v", err)
	}
	if strings.Contains(string(raw), "165 Km") || strings.Contains(string(raw), `"distance"`) {
		t.Error("the answer carries a distance; the pack says query-relative distance is not a facility attribute")
	}
}

// AgricultureFacility v0.1 Direct mode requires @type, informationMode,
// facilityType and source, plus at least one of location or address.
//
// It is address rather than location, and that is not a shortcut: POCRA returns
// no verified per-facility geometry -- the one gps in its answer is a fixed stub
// unrelated to what was asked for -- and the pack states that an adapter must
// not copy a request coordinate into location unless the provider confirms the
// coordinate belongs to the facility.
func TestShippedMappingFollowsTheAgricultureFacilityPack(t *testing.T) {
	_, answer, err := runSelect(t, selectRequest)
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	for _, entry := range answerResources(t, answer) {
		id, _ := dig(entry, "id").(string)
		attrs, ok := dig(entry, "resourceAttributes").(map[string]any)
		if !ok {
			t.Fatalf("resource %s carries no resourceAttributes", id)
		}

		for _, f := range []struct{ key, want string }{
			{"@context", declaredContext},
			{"@type", "openagrinet:AgricultureFacility"},
			{"informationMode", "Direct"},
			{"facilityType", "KrishiVigyanKendra"},
		} {
			if attrs[f.key] != f.want {
				t.Errorf("%s: %s = %v, want %v", id, f.key, attrs[f.key], f.want)
			}
		}

		// Direct mode requires source, and anyOf location or address.
		if attrs["source"] == nil {
			t.Errorf("%s: Direct mode requires source", id)
		}
		if attrs["address"] == nil {
			t.Errorf("%s: no verified geometry is available, so address is required", id)
		}
		if _, present := attrs["location"]; present {
			t.Errorf("%s: carries location; POCRA supplies no verified facility geometry", id)
		}

		// Not supplied truthfully by POCRA, so not asserted. Deriving services
		// from the facility type, or lastUpdatedAt from fetch time, would be
		// inventing data under a governed schema.
		for _, absent := range []string{"services", "lastUpdatedAt", "capacity", "website"} {
			if _, present := attrs[absent]; present {
				t.Errorf("%s: carries %q, which POCRA does not supply", id, absent)
			}
		}
	}
}

// POCRA sends "Unknown", "N/A" and "000000" where it has no value. Publishing
// them would put junk on the network under a governed schema.
func TestShippedMappingScrubsPocraPlaceholders(t *testing.T) {
	_, answer, err := runSelect(t, selectRequest)
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("could not re-encode the answer: %v", err)
	}
	for _, placeholder := range []string{"Unknown", "N/A", "000000"} {
		if strings.Contains(string(raw), placeholder) {
			t.Errorf("the answer carries the placeholder %q", placeholder)
		}
	}

	resources := answerResources(t, answer)

	// COMMON-55007 sorts first and has no usable contact person at all, so it
	// must carry no publicContact rather than an empty one.
	nearest, ok := dig(resources[0], "resourceAttributes").(map[string]any)
	if !ok {
		t.Fatal("the nearest resource carries no resourceAttributes")
	}
	if id := dig(resources[0], "id"); id != "res:pocra:facility:COMMON-55007" {
		t.Fatalf("resource 0 = %v, want COMMON-55007", id)
	}
	if _, present := nearest["publicContact"]; present {
		t.Error("COMMON-55007 has no usable contact, so publicContact must be absent")
	}

	// COMMON-55043 does have one, and it is carried as Contact.name rather than
	// Contact.company. POCRA gives one `person` field and says nothing about
	// which it holds: this KVK's is an office, but every CHC's is a named
	// individual with a personal mobile. company is "Company name if B2B
	// contact" in Beckn v2.0, so routing all three through it mislabels the
	// personal ones instead of making them less personal.
	farther, ok := dig(resources[1], "resourceAttributes").(map[string]any)
	if !ok {
		t.Fatal("the farther resource carries no resourceAttributes")
	}
	contact, ok := farther["publicContact"].(map[string]any)
	if !ok {
		t.Fatal("COMMON-55043 has a contact person and must carry publicContact")
	}
	if name, _ := contact["name"].(string); !strings.Contains(name, "Sanskriti") {
		t.Errorf("publicContact.name = %v, want the contact POCRA published", contact["name"])
	}
	if _, present := contact["company"]; present {
		t.Error("POCRA's person field is not known to be a company, so company must be absent")
	}

	// The district survives scrubbing even though region, taluka and village do
	// not, so the address is still useful.
	address, ok := farther["address"].(map[string]any)
	if !ok {
		t.Fatal("COMMON-55043 carries no address")
	}
	if extended, _ := address["extendedAddress"].(string); !strings.Contains(extended, "Nanded") {
		t.Errorf("extendedAddress = %v, want the district that survived scrubbing", address["extendedAddress"])
	}
	if _, present := address["postalCode"]; present {
		t.Error("pinCode was 000000, so postalCode must be absent rather than zeroed")
	}
	if address["addressCountry"] != "IN" || address["addressRegion"] != "Maharashtra" {
		t.Errorf("address = %v, want IN / Maharashtra", address)
	}

	// sourceId and sourceName identify the binding's upstream, which POCRA does
	// not restate per item. sourceUri is the URL POCRA answered with, published
	// as sent: substituting one it did not return would assert something it
	// never said.
	source, ok := farther["source"].(map[string]any)
	if !ok {
		t.Fatal("COMMON-55043 carries no source")
	}
	if source["sourceId"] != "pocra" {
		t.Errorf("source.sourceId = %v, want pocra", source["sourceId"])
	}
	if source["sourceUri"] != "https://provider.mahapocra.gov.in" {
		t.Errorf("source.sourceUri = %v, want the webUrl POCRA sent", source["sourceUri"])
	}
}

// The request names an abstract search resource and the answer returns the
// concrete facilities that satisfy it, so the offer arrives referencing an id
// that appears nowhere in the answer. Echoing the offer unchanged leaves that
// dangling reference behind.
func TestShippedMappingRewritesTheOffersReferences(t *testing.T) {
	_, answer, err := runSelect(t, selectRequest)
	if err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	commitments, _ := dig(answer, "message", "contract", "commitments").([]any)
	resources := answerResources(t, answer)
	offer, ok := dig(commitments[0], "offer").(map[string]any)
	if !ok {
		t.Fatal("the quoted commitment carries no offer")
	}

	returned := make([]string, 0, len(resources))
	for _, entry := range resources {
		id, _ := dig(entry, "id").(string)
		returned = append(returned, id)
	}

	referenced, _ := offer["resourceIds"].([]any)
	if len(referenced) != len(returned) {
		t.Fatalf("offer.resourceIds has %d entries, want %d -- one per resource returned",
			len(referenced), len(returned))
	}
	for _, reference := range referenced {
		if !slices.Contains(returned, reference.(string)) {
			t.Errorf("offer references %v, which is not among the resources returned", reference)
		}
		if reference == "res:pocra:facility-search" {
			t.Error("the offer still references the abstract resource the request asked for")
		}
	}

	// Only the references are rewritten. Everything else the request offered
	// survives.
	if offer["id"] != "offer:pocra:common-services" {
		t.Errorf("offer id = %v, want the one the request offered", offer["id"])
	}
	if dig(offer, "provider", "id") != "pocra" {
		t.Errorf("offer provider = %v, want the one the request offered", offer["provider"])
	}

	// A mapping transforms a payload; it does not assert who anyone is. The two
	// Uri fields in particular are only whatever the caller sent -- in a deployed
	// stack a container-internal address -- so echoing them would republish
	// another party's routing details as ours.
	beckncontext, _ := dig(answer, "context").(map[string]any)
	for _, field := range []string{"bapId", "bapUri", "bppId", "bppUri"} {
		if _, present := beckncontext[field]; present {
			t.Errorf("response context carries %q; a mapping must not assert identity", field)
		}
	}

	// The v2 status enum is DRAFT, ACTIVE and CLOSED. QUOTED reads better and is
	// refused by base schema validation. ACTIVE rather than DRAFT: the answer
	// stands on its own, naming facilities available now.
	if code := dig(commitments[0], "status", "descriptor", "code"); code != "ACTIVE" {
		t.Errorf("status = %v, want ACTIVE -- QUOTED is not in the spec's enum", code)
	}

	// Required by Commitment.resources even though Resource itself declares no
	// quantity property and the spec carries no Quantity schema. The only shape
	// the spec states is the one its own error example implies --
	// $.message.order.items[0].quantity.count -- so it is an object with a
	// count, not the bare number this used to accept.
	for _, entry := range resources {
		if _, present := dig(entry, "quantity", "count").(float64); !present {
			t.Errorf("resource %v carries no quantity.count; the spec requires a quantity on every commitment resource",
				dig(entry, "id"))
		}
	}
}

// warehouseResponse is a verbatim POCRA answer to a warehouse search, captured
// on 4 Sep 2026. It is here because warehouses come from a DIFFERENT BPP than
// the other three facility types, and its items are not the same shape:
//
//   - no "category" tag at all, so facilityType cannot be read off the item
//   - a "capacity_estimate" tag, which the other three never carry
//   - price and rating fields the others do not have
//   - a fully populated address rather than "Unknown" and "000000"
//   - a real email, and "-" as a phone placeholder rather than "N/A"
//
// The doc's sample covered kvk alone, which is why none of this was visible
// until the mapping was run against the live API.
const warehouseResponse = `{
  "context": { "action": "search", "version": "1.1.0" },
  "responses": [{
    "context": { "action": "on_search", "version": "1.1.0" },
    "message": { "catalog": {
      "descriptor": { "name": "Warehouse Services" },
      "providers": [{
        "id": "WAREHOUSE001",
        "fulfillments": [{ "id": "f1", "type": "Service",
          "categories": [{ "id": "c1", "descriptor": { "code": "GSW", "name": "General Storage Warehouse" } }] }],
        "items": [
          {
            "id": "WARE-1433",
            "descriptor": { "name": "Shrirampur Midc Warehouse",
                            "short_desc": "Warehouse in Shrirampur, Shrirampur" },
            "address": { "address": "Mswc, Shrirampurc Midc, Plot No. X-33 Khandala",
                         "district": "Ahmednagar", "region": "Pune",
                         "taluka": "Shrirampur", "vilage": "Shrirampur", "pinCode": "413720" },
            "contact": { "person": "Warehouse Manager", "email": "shrirampur.wh@mswc.in",
                         "phone": "-", "webUrl": "https://warehouse.com" },
            "price": { "currency": "INR", "value": "12" },
            "rating": "4.2",
            "category_ids": ["c1"],
            "fulfillment_ids": ["f1"],
            "tags": [{ "list": [
              { "descriptor": { "code": "distance" }, "value": "15.2093 km" },
              { "descriptor": { "code": "capacity_estimate" }, "value": "500 tons" } ] }]
          },
          {
            "id": "WARE-1406",
            "descriptor": { "name": "Shrirampur Warehouse" },
            "address": { "address": "Mswc, Krushi Utpanna Bazar Samiti, Market Yard, Shrirampur",
                         "district": "Ahmednagar", "region": "Pune",
                         "taluka": "Shrirampur", "vilage": "Shrirampur", "pinCode": "413709" },
            "contact": { "person": "Warehouse Manager", "email": "shrirampur.wh@mswc.in",
                         "phone": "02422-222735", "webUrl": "https://warehouse.com" },
            "category_ids": ["c1"],
            "fulfillment_ids": ["f1"],
            "tags": [{ "list": [
              { "descriptor": { "code": "distance" }, "value": "18.8434 km" },
              { "descriptor": { "code": "capacity_estimate" }, "value": "1200 tons" } ] }]
          }
        ]
      }]
    } }
  }]
}`

// warehouseRequest asks for the one facility type whose answer is shaped
// differently from the rest.
func warehouseRequest(t *testing.T) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	attributes(t, payload)["supportedFacilityTypes"] = []any{"Warehouse"}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("could not rebuild the payload: %v", err)
	}
	return string(body)
}

// runAgainst drives the step over a provider answering with the given body.
func runAgainst(t *testing.T, request, providerBody string) map[string]any {
	t.Helper()

	mappings := serveMappings(t)
	defer mappings.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, providerBody)
	}))
	defer upstream.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey: shippedBindingKey, BaseURL: upstream.URL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodPost, Path: "/search",
				Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000},
		},
	}}

	step, closeStep, err := AgricultureFacility.New(context.Background(), registry, mapper,
		&AgricultureFacility.Config{
			BindingKeys: []string{shippedBindingKey},
			// Auth is per provider now; POCRA needs no credential and says so.
			AuthByProvider: map[string]*common.AuthProfile{
				strings.Split(shippedBindingKey, "|")[0]: {Scheme: util.AuthSchemeNone},
			},
		})
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	defer closeStep()

	stepCtx := &model.StepContext{Context: t.Context(), Body: []byte(request)}
	if err := step.Run(stepCtx); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal(stepCtx.ResponseBody, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}
	return answer
}

// A warehouse item carries no category tag, so facilityType cannot be read off
// the item the way it is for the other three types. Direct mode REQUIRES
// facilityType, so without a fallback every warehouse answer would violate the
// pack -- signed and delivered, because nothing in the adapter validates the
// answer against the pack.
func TestShippedMappingLabelsAWarehouseWithNoCategoryTag(t *testing.T) {
	answer := runAgainst(t, warehouseRequest(t), warehouseResponse)

	resources := answerResources(t, answer)
	if len(resources) != 2 {
		t.Fatalf("got %d resources, want 2", len(resources))
	}
	for _, entry := range resources {
		id, _ := dig(entry, "id").(string)
		attrs, ok := dig(entry, "resourceAttributes").(map[string]any)
		if !ok {
			t.Fatalf("resource %s carries no resourceAttributes", id)
		}
		if attrs["facilityType"] != "Warehouse" {
			t.Errorf("%s: facilityType = %v, want Warehouse from the requested type",
				id, attrs["facilityType"])
		}
	}
}

// The pack has a home for capacity -- value, unit and an optional basis -- and a
// warehouse is the one facility type POCRA reports it for. Dropping it would
// discard the single most useful thing a warehouse search returns.
func TestShippedMappingCarriesWarehouseCapacity(t *testing.T) {
	answer := runAgainst(t, warehouseRequest(t), warehouseResponse)

	first, ok := dig(answerResources(t, answer)[0], "resourceAttributes").(map[string]any)
	if !ok {
		t.Fatal("the nearest resource carries no resourceAttributes")
	}
	capacity, ok := first["capacity"].(map[string]any)
	if !ok {
		t.Fatalf("the nearest warehouse carries no capacity; POCRA reported one")
	}
	if value, _ := capacity["value"].(float64); value != 500 {
		t.Errorf("capacity.value = %v, want 500 as a number", capacity["value"])
	}
	if capacity["unit"] != "tons" {
		t.Errorf("capacity.unit = %v, want tons", capacity["unit"])
	}
}

// "-" is a placeholder POCRA uses for a phone it does not have, alongside the
// "N/A", "Unknown" and "000000" the other providers use.
func TestShippedMappingScrubsTheWarehousePlaceholders(t *testing.T) {
	answer := runAgainst(t, warehouseRequest(t), warehouseResponse)

	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("could not re-encode the answer: %v", err)
	}
	if strings.Contains(string(raw), `"-"`) {
		t.Error(`the answer carries "-", which is POCRA's placeholder for an absent phone`)
	}

	resources := answerResources(t, answer)
	first, _ := dig(resources[0], "resourceAttributes").(map[string]any)
	contact, ok := first["publicContact"].(map[string]any)
	if !ok {
		t.Fatal("the nearest warehouse has a real email and must carry publicContact")
	}
	if contact["email"] != "shrirampur.wh@mswc.in" {
		t.Errorf("publicContact.email = %v, want the address POCRA published", contact["email"])
	}
	if _, present := contact["phone"]; present {
		t.Error(`publicContact carries a phone, but POCRA sent "-" for it`)
	}

	// The second warehouse has a real phone, so it must survive.
	second, _ := dig(resources[1], "resourceAttributes").(map[string]any)
	secondContact, ok := second["publicContact"].(map[string]any)
	if !ok {
		t.Fatal("the second warehouse has a real phone and must carry publicContact")
	}
	if secondContact["phone"] != "02422-222735" {
		t.Errorf("publicContact.phone = %v, want the number POCRA published", secondContact["phone"])
	}
}

// emptyResponse is what POCRA returns when the BPP for a category answers with
// nothing: HTTP 200, and responses[] empty. Observed live against chc on
// 4 Sep 2026, minutes after the same request returned five facilities.
//
// It is not an error. "No facilities near you" is a legitimate answer to a
// search, and the caller has to be able to tell it apart from a failure.
const emptyResponse = `{
  "context": { "action": "search", "version": "1.1.0" },
  "responses": []
}`

func TestShippedMappingAnswersAnEmptySearch(t *testing.T) {
	answer := runAgainst(t, selectRequest, emptyResponse)

	// The answer still has to be a well-formed on_select that correlates, or
	// the caller cannot tell "none found" from "something broke".
	if got := dig(answer, "context", "action"); got != "on_select" {
		t.Errorf("action = %v, want on_select", got)
	}
	if got := dig(answer, "context", "transactionId"); got != "f9e8d7c6-b5a4-4321-9876-543210fedcba" {
		t.Errorf("transactionId = %v, want the one from the request", got)
	}

	commitments, ok := dig(answer, "message", "contract", "commitments").([]any)
	if !ok || len(commitments) != 1 {
		t.Fatalf("got %d commitments, want 1", len(commitments))
	}

	// Whatever shape it takes, it must not claim facilities it does not have,
	// and the offer must not reference resources that are not there.
	resources, _ := dig(commitments[0], "resources").([]any)
	offer, _ := dig(commitments[0], "offer").(map[string]any)
	referenced, _ := offer["resourceIds"].([]any)
	if len(resources) != 0 {
		t.Errorf("got %d resources for an empty search, want none", len(resources))
	}
	if len(referenced) != len(resources) {
		t.Errorf("offer references %d resources but the answer carries %d",
			len(referenced), len(resources))
	}
}

// mixedResponse is POCRA answering a chc search with kvk facilities in it too.
//
// This is not hypothetical. POCRA caches per message_id for PT10M and ACCUMULATES
// across searches that share one: on 4 Sep 2026, two searches with the same
// message_id and different transaction_ids returned {kvk:5} and then
// {kvk:5, chc:5}. Beckn requires a fresh messageId per message, but an adapter
// cannot assume every caller is well behaved, and a retry that reuses one is the
// obvious way to trip it.
//
// The warehouse provider is in here too, carrying no category tag on its items --
// only its fulfillment says GSW. It is the case that makes filtering on the item
// tag alone insufficient: with a tag-or-requested-type fallback, a leaked
// warehouse would be labelled as whatever was asked for and returned as one.
//
// And two providers that are not facilities at all. POCRA fans a search out to
// every BPP on its network, so a live warehouse search came back with 20
// responses[] holding 182 items: 5 warehouses, 132 mandi price records, 6
// administrative hierarchy rows and 4 weather station readings. apmcMandi is
// the awkward one -- its items have no tags key at all, so every tag lookup on
// them is a miss rather than a value.
const mixedResponse = `{
  "context": { "action": "search", "version": "1.1.0" },
  "responses": [
    {
      "context": { "action": "on_search" },
      "message": { "catalog": { "providers": [{
        "id": "COMMON_PROVIDER_CHC",
        "fulfillments": [{ "id": "f1_chc",
          "categories": [{ "id": "c_chc", "descriptor": { "code": "chc", "name": "CHC" } }] }],
        "items": [{
          "id": "COMMON-185",
          "descriptor": { "name": "Custom Hiring Center" },
          "address": { "address": "Gogalgaon, Rahta", "district": "Ahmednagar",
                       "region": "Unknown", "taluka": "Rahta", "vilage": "Gogalgaon",
                       "pinCode": "000000" },
          "contact": { "person": "Rajesh Thoke", "email": "N/A", "phone": "9765007429",
                       "webUrl": "https://provider.mahapocra.gov.in" },
          "tags": [{ "list": [
            { "descriptor": { "code": "distance" }, "value": "14 Km" },
            { "descriptor": { "code": "category" }, "value": "chc" } ] }]
        }]
      }] } }
    },
    {
      "context": { "action": "on_search" },
      "message": { "catalog": { "providers": [{
        "id": "COMMON_PROVIDER_KVK",
        "fulfillments": [{ "id": "f1_kvk",
          "categories": [{ "id": "c_kvk", "descriptor": { "code": "kvk", "name": "KVK" } }] }],
        "items": [{
          "id": "COMMON-55031",
          "descriptor": { "name": "Krishi Vigyan Kendra, Babhleshwar" },
          "address": { "address": "KVK Babhleshwar", "district": "Ahmednagar",
                       "region": "Unknown", "taluka": "Unknown", "vilage": "Unknown",
                       "pinCode": "000000" },
          "contact": { "person": "President", "email": "N/A", "phone": "N/A",
                       "webUrl": "https://provider.mahapocra.gov.in" },
          "tags": [{ "list": [
            { "descriptor": { "code": "distance" }, "value": "11 Km" },
            { "descriptor": { "code": "category" }, "value": "kvk" } ] }]
        }]
      }] } }
    },
    {
      "context": { "action": "on_search" },
      "message": { "catalog": { "providers": [{
        "id": "apmcMandi",
        "descriptor": { "name": "Maharashtra Mandi" },
        "items": [{
          "id": "mandi-6001",
          "descriptor": { "name": "Tomato" },
          "location_ids": ["loc1"],
          "price": { "minimum_value": "6000", "maximum_value": "13000", "estimated_value": "9000" },
          "time": { "label": "2026-09-03" }
        }]
      }] } }
    },
    {
      "context": { "action": "on_search" },
      "message": { "catalog": { "providers": [{
        "id": "ADMIN001",
        "descriptor": { "name": "Administrative Information" },
        "fulfillments": [{ "id": "f_adm",
          "categories": [{ "id": "c_adm", "descriptor": { "code": "ADM", "name": "Administrative Hierarchy" } }] }],
        "items": [{
          "id": "ADM-802776",
          "descriptor": { "name": "Nashik (M Corp.)" },
          "address": { "address": "Nashik", "district": "Nashik", "region": "Nashik",
                       "taluka": "Nashik", "vilage": "Nashik (M Corp.)", "pinCode": "422001" },
          "tags": [{ "list": [
            { "descriptor": { "code": "district_code" }, "value": "516" },
            { "descriptor": { "code": "village_code" }, "value": "802776" } ] }]
        }]
      }] } }
    },
    {
      "context": { "action": "on_search" },
      "message": { "catalog": { "providers": [{
        "id": "WAREHOUSE001",
        "fulfillments": [{ "id": "f1",
          "categories": [{ "id": "c1", "descriptor": { "code": "GSW", "name": "General Storage Warehouse" } }] }],
        "items": [{
          "id": "WARE-1433",
          "descriptor": { "name": "Shrirampur Midc Warehouse" },
          "address": { "address": "Mswc Shrirampur", "district": "Ahmednagar",
                       "region": "Pune", "taluka": "Shrirampur", "vilage": "Shrirampur",
                       "pinCode": "413720" },
          "contact": { "person": "Warehouse Manager", "email": "shrirampur.wh@mswc.in",
                       "phone": "-", "webUrl": "https://warehouse.com" },
          "tags": [{ "list": [
            { "descriptor": { "code": "distance" }, "value": "15.2093 km" },
            { "descriptor": { "code": "capacity_estimate" }, "value": "500 tons" } ] }]
        }]
      }] } }
    }
  ]
}`

// requestFor builds a select for one governed facility type.
func requestFor(t *testing.T, facilityType string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	attributes(t, payload)["supportedFacilityTypes"] = []any{facilityType}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("could not rebuild the payload: %v", err)
	}
	return string(body)
}

// A request for one facility type must be answered with that type and nothing
// else, whatever POCRA chose to include.
func TestShippedMappingReturnsOnlyTheRequestedFacilityType(t *testing.T) {
	for _, tc := range []struct {
		facilityType string
		wantIDs      []string
	}{
		{"CustomHiringCentre", []string{"res:pocra:facility:COMMON-185"}},
		{"KrishiVigyanKendra", []string{"res:pocra:facility:COMMON-55031"}},
		{"Warehouse", []string{"res:pocra:facility:WARE-1433"}},
	} {
		t.Run(tc.facilityType, func(t *testing.T) {
			answer := runAgainst(t, requestFor(t, tc.facilityType), mixedResponse)
			resources := answerResources(t, answer)

			got := make([]string, 0, len(resources))
			for _, entry := range resources {
				id, _ := dig(entry, "id").(string)
				got = append(got, id)

				attrs, ok := dig(entry, "resourceAttributes").(map[string]any)
				if !ok {
					t.Fatalf("resource %s carries no resourceAttributes", id)
				}
				if attrs["facilityType"] != tc.facilityType {
					t.Errorf("%s: facilityType = %v, want %s -- a leaked facility must not be relabelled",
						id, attrs["facilityType"], tc.facilityType)
				}
			}
			if !slices.Equal(got, tc.wantIDs) {
				t.Errorf("returned %v, want exactly %v", got, tc.wantIDs)
			}

			// Nothing from a provider that is not a facility may reach the
			// answer -- not a mandi price, not an administrative code.
			raw, err := json.Marshal(answer)
			if err != nil {
				t.Fatalf("could not re-encode the answer: %v", err)
			}
			for _, leak := range []string{"mandi-6001", "estimated_value", "ADM-802776",
				"district_code", "802776", "Maharashtra Mandi"} {
				if strings.Contains(string(raw), leak) {
					t.Errorf("the answer leaked %q from a provider that is not a facility", leak)
				}
			}

			// The offer must not reference what was filtered out either.
			commitments, _ := dig(answer, "message", "contract", "commitments").([]any)
			offer, _ := dig(commitments[0], "offer").(map[string]any)
			referenced, _ := offer["resourceIds"].([]any)
			if len(referenced) != len(got) {
				t.Errorf("offer references %d resources but the answer carries %d",
					len(referenced), len(got))
			}
		})
	}
}

// The answer echoes whatever @context the request declared, rather than
// restating a pack URL of its own.
//
// Hardcoding it meant the mapping had to know which identifier is current, and
// could contradict what the caller actually sent. Both forms below are in use:
// the published pack URL, which is what resolves today and what the fixture
// declares, and the schemas.openagrinet.global identifier the packs name in
// their own x-jsonld -- which has no DNS record, so a caller cannot be assumed
// to send either one.
//
// The pack does not constrain @context -- it appears only under x-jsonld, which
// is annotation metadata a JSON Schema validator ignores -- so conformance
// cannot catch a wrong one. This test is the only thing that does.
func TestShippedMappingEchoesTheCallersContext(t *testing.T) {
	for _, declared := range []string{
		declaredContext,
		"https://schemas.openagrinet.global/schema/AgricultureFacility/v0.1/context.jsonld",
	} {
		t.Run(declared, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			attributes(t, payload)["@context"] = declared
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("could not rebuild the payload: %v", err)
			}

			_, answer, runErr := runSelect(t, string(body))
			if runErr != nil {
				t.Fatalf("Run() returned an unexpected error: %v", runErr)
			}
			for _, entry := range answerResources(t, answer) {
				id, _ := dig(entry, "id").(string)
				got := dig(entry, "resourceAttributes", "@context")
				if got != declared {
					t.Errorf("%s: @context = %v, want the one the request declared (%s)",
						id, got, declared)
				}
			}
		})
	}
}

// --------------------------------------------------------------------------
// Fan-out: one payload, one upstream call per facility type it asks for.
//
// POCRA's search takes exactly ONE category code -- a comma-separated pair
// answers 200 with no providers, and a category array is refused with "Schema
// validation failed", both verified against the live API. So a payload naming
// two types cannot be served by one call.
//
// Before fan-out existed the request half sent supportedFacilityTypes[0] and
// the response half filtered to that same entry, so a two-type search returned
// the first type and silently dropped the rest. Nothing in the answer said a
// type had been asked for and lost, which is what made it worth finding.
// --------------------------------------------------------------------------

// pocraByCategory is a fake POCRA that answers each category with its own
// captured response, and records what it was asked.
//
// Routing on the category code is the whole point: a fake that returns one
// canned body whatever it is asked cannot tell a fan-out that sent the right
// codes from one that sent the same code twice.
type pocraByCategory struct {
	mu       sync.Mutex
	byCode   map[string]string
	codes    []string
	messages []string
}

func (p *pocraByCategory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var sent map[string]any
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
		http.Error(w, "not JSON", http.StatusBadRequest)
		return
	}
	code, _ := dig(sent, "message", "intent", "category", "descriptor", "code").(string)
	message, _ := dig(sent, "context", "message_id").(string)

	p.mu.Lock()
	p.codes = append(p.codes, code)
	p.messages = append(p.messages, message)
	body, known := p.byCode[code]
	p.mu.Unlock()

	if !known {
		// A code with no fixture is a fan-out that sent something unasked for,
		// and answering it with anything would hide that.
		http.Error(w, "unexpected category "+code, http.StatusBadRequest)
		return
	}
	fmt.Fprint(w, body)
}

// asked returns the category codes POCRA was sent, sorted so an assertion does
// not depend on which concurrent call arrived first.
func (p *pocraByCategory) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	sorted := append([]string(nil), p.codes...)
	sort.Strings(sorted)
	return sorted
}

// runSplitSearch runs the shipped mapping against a fake POCRA that answers per
// category, and returns the answer with the fake for inspection.
func runSplitSearch(t *testing.T, types []string, byCode map[string]string) (map[string]any, *pocraByCategory) {
	t.Helper()

	mappings := serveMappings(t)
	defer mappings.Close()

	pocra := &pocraByCategory{byCode: byCode}
	upstream := httptest.NewServer(pocra)
	defer upstream.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey: shippedBindingKey, BaseURL: upstream.URL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodPost, Path: "/search",
				Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000},
		},
	}}

	step, closeStep, err := AgricultureFacility.New(context.Background(), registry, mapper,
		&AgricultureFacility.Config{
			BindingKeys: []string{shippedBindingKey},
			// Auth is per provider now; POCRA needs no credential and says so.
			AuthByProvider: map[string]*common.AuthProfile{
				strings.Split(shippedBindingKey, "|")[0]: {Scheme: util.AuthSchemeNone},
			},
		})
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	defer closeStep()

	var payload map[string]any
	if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	attributes(t, payload)["supportedFacilityTypes"] = toAny(types)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("could not rebuild the payload: %v", err)
	}

	stepCtx := &model.StepContext{Context: t.Context(), Body: body}
	if err := step.Run(stepCtx); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal(stepCtx.ResponseBody, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}
	return answer, pocra
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

// typesIn counts the answer's resources by facilityType.
func typesIn(t *testing.T, answer map[string]any) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, entry := range answerResources(t, answer) {
		got, _ := dig(entry, "resourceAttributes", "facilityType").(string)
		counts[got]++
	}
	return counts
}

// The whole point: two types asked for, two types answered.
//
// This is the case that was broken. It returned KrishiVigyanKendra alone, and
// the Warehouse the caller also asked for was dropped without a word.
func TestATwoTypeSearchIsAnsweredWithBothTypes(t *testing.T) {
	answer, pocra := runSplitSearch(t,
		[]string{"KrishiVigyanKendra", "Warehouse"},
		map[string]string{"kvk": providerResponse, "warehouse": warehouseResponse})

	if want := []string{"kvk", "warehouse"}; !slices.Equal(pocra.asked(), want) {
		t.Errorf("POCRA was asked for %v, want one call per type %v", pocra.asked(), want)
	}
	got := typesIn(t, answer)
	if got["KrishiVigyanKendra"] == 0 || got["Warehouse"] == 0 {
		t.Errorf("answer carries %v, want both requested types present", got)
	}
	for facilityType := range got {
		if facilityType != "KrishiVigyanKendra" && facilityType != "Warehouse" {
			t.Errorf("answer carries %q, which was not asked for", facilityType)
		}
	}
}

// All four, because they do not all state their type the same way -- three
// carry a category tag and a COMMON_PROVIDER_ id, a warehouse carries neither.
func TestEveryGovernedTypeCanBeAskedForAtOnce(t *testing.T) {
	answer, pocra := runSplitSearch(t,
		[]string{"KrishiVigyanKendra", "CustomHiringCentre", "SoilTestingFacility", "Warehouse"},
		map[string]string{
			"kvk":       providerResponse,
			"chc":       mixedResponse,
			"soil_lab":  mixedResponse,
			"warehouse": warehouseResponse,
		})

	if want := []string{"chc", "kvk", "soil_lab", "warehouse"}; !slices.Equal(pocra.asked(), want) {
		t.Errorf("POCRA was asked for %v, want %v", pocra.asked(), want)
	}
	got := typesIn(t, answer)
	// soil_lab is routed to a fixture that carries none, so three of the four
	// have facilities to return. The assertion is that nothing UNASKED appears
	// and that the types with data all do.
	for _, want := range []string{"KrishiVigyanKendra", "CustomHiringCentre", "Warehouse"} {
		if got[want] == 0 {
			t.Errorf("answer carries %v, want %s present", got, want)
		}
	}
	if got["SoilTestingFacility"] != 0 {
		t.Errorf("answer carries %d SoilTestingFacility, but the fixture has none",
			got["SoilTestingFacility"])
	}
}

// A leaked facility is still dropped when fanning out.
//
// mixedResponse is POCRA answering a chc search with kvk, mandi, administrative
// and warehouse providers in it too. Asking for chc and warehouse must return
// the chc facilities and the warehouse ones -- not the kvk that leaked into the
// chc answer, and not the mandi or administrative providers, which are not
// facilities at all.
func TestASplitSearchStillDropsWhatWasNotAskedFor(t *testing.T) {
	answer, _ := runSplitSearch(t,
		[]string{"CustomHiringCentre", "Warehouse"},
		map[string]string{"chc": mixedResponse, "warehouse": warehouseResponse})

	got := typesIn(t, answer)
	if got["KrishiVigyanKendra"] != 0 {
		t.Errorf("the kvk that leaked into the chc answer was returned: %v", got)
	}
	for facilityType, count := range got {
		if facilityType != "CustomHiringCentre" && facilityType != "Warehouse" {
			t.Errorf("answer carries %d of %q, which was not asked for", count, facilityType)
		}
	}
	if got["CustomHiringCentre"] == 0 || got["Warehouse"] == 0 {
		t.Errorf("answer carries %v, want both requested types", got)
	}
}

// Each fan-out call carries its OWN message_id, and none of them is the
// caller's.
//
// POCRA keeps a message_id's answers for ten minutes and returns the union of
// everything asked for under it -- verified against the live API, where a kvk
// search under a reused id came back carrying an earlier chc search's
// facilities. Sending one id across the fan-out would make every call return
// every other call's facilities, so this is what stops the adapter causing the
// leak it also filters.
func TestEachSearchCallCarriesItsOwnRequestId(t *testing.T) {
	_, pocra := runSplitSearch(t,
		[]string{"KrishiVigyanKendra", "Warehouse"},
		map[string]string{"kvk": providerResponse, "warehouse": warehouseResponse})

	if len(pocra.messages) != 2 {
		t.Fatalf("POCRA saw %d calls, want 2", len(pocra.messages))
	}
	if pocra.messages[0] == pocra.messages[1] {
		t.Errorf("both calls used message_id %q, so POCRA would blend their answers",
			pocra.messages[0])
	}
	for _, message := range pocra.messages {
		// POCRA's schema refuses a message_id that is not a UUID: a derived one
		// like "<caller-id>-kvk" is answered with 400 Schema validation failed.
		if _, err := uuid.Parse(message); err != nil {
			t.Errorf("message_id %q is not a UUID, which POCRA refuses: %v", message, err)
		}
		if message == callerMessageID {
			t.Errorf("a call reused the caller's messageId %q", message)
		}
	}
}

// An ungoverned type anywhere in the list is refused, not just in first place.
//
// The precondition used to read supportedFacilityTypes[0], so a payload naming
// a governed type first and nonsense second passed the check and then fanned
// out to a call POCRA would refuse.
func TestAnUngovernedTypeIsRefusedWhereverItAppears(t *testing.T) {
	for _, types := range [][]string{
		{"NotAFacility"},
		{"KrishiVigyanKendra", "NotAFacility"},
		{"NotAFacility", "KrishiVigyanKendra"},
	} {
		t.Run(strings.Join(types, "+"), func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			attributes(t, payload)["supportedFacilityTypes"] = toAny(types)
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("could not rebuild the payload: %v", err)
			}

			_, _, runErr := runSelect(t, string(body))
			if runErr == nil {
				t.Fatal("the payload was served, want it refused for naming an ungoverned type")
			}
			if !strings.Contains(runErr.Error(), "governed facility type") {
				t.Errorf("error = %v, want the mapping's own explanation", runErr)
			}
		})
	}
}

// --------------------------------------------------------------------------
// A placeholder value must not crash the whole search.
//
// $number() throws on anything it cannot parse, and the throw is not local to
// the item carrying the bad value -- it propagates out of the whole response
// mapping. One placeholder distance or capacity_estimate on one item then
// failed a search that also carried good facilities, which is worse than
// dropping the one bad field: it drops everything.
// --------------------------------------------------------------------------

// pocraSingleWith builds a minimal, valid POCRA envelope around one item's
// extra tags, so a test can vary just the field it is checking.
func pocraSingleWith(providerID, categoryCode, itemID, extraTags string) string {
	return `{ "responses": [ {
	  "context": { "action": "on_search", "version": "1.1.0" },
	  "message": { "catalog": { "descriptor": { "name": "x" },
	    "providers": [{
	      "id": "` + providerID + `",
	      "fulfillments": [{ "id": "f1", "categories": [{ "descriptor": { "code": "` + categoryCode + `" } }] }],
	      "items": [
	        { "id": "` + itemID + `", "descriptor": {"name":"a"}, "address": {}, "contact": {},
	          "fulfillment_ids": ["f1"], "category_ids": ["c1"],
	          "tags": [{ "list": [` + extraTags + `] }] }
	      ]
	    }]
	  } }
	} ] }`
}

// A placeholder distance is dropped from ranking, not fatal to the search.
//
// POCRA reports "165 Km" when it knows the distance and "Unknown" -- among the
// same placeholders this file already strips elsewhere -- when it does not.
// TestReadingTreatsAnAbsentDistanceAsUnranked (below, existing) covers the
// field being missing outright; this covers it being PRESENT but unparseable,
// which used to throw D3030 rather than return the resource unranked.
func TestAPlaceholderDistanceDoesNotFailTheSearch(t *testing.T) {
	body := `{ "responses": [ {
	  "context": { "action": "on_search", "version": "1.1.0" },
	  "message": { "catalog": { "descriptor": { "name": "x" },
	    "providers": [{
	      "id": "COMMON_PROVIDER_KVK",
	      "fulfillments": [{ "id": "f1", "categories": [{ "descriptor": { "code": "kvk" } }] }],
	      "items": [
	        { "id": "NEAR", "descriptor": {"name":"a"}, "address": {}, "contact": {},
	          "fulfillment_ids": ["f1"], "category_ids": ["c1"],
	          "tags": [{ "list": [{ "descriptor": {"code":"distance"}, "value": "12 Km" }] }] },
	        { "id": "PLACEHOLDER", "descriptor": {"name":"b"}, "address": {}, "contact": {},
	          "fulfillment_ids": ["f1"], "category_ids": ["c1"],
	          "tags": [{ "list": [{ "descriptor": {"code":"distance"}, "value": "Unknown" }] }] }
	      ]
	    }]
	  } }
	} ] }`

	answer := runAgainst(t, requestFor(t, "KrishiVigyanKendra"), body)
	resources := answerResources(t, answer)
	if len(resources) != 2 {
		t.Fatalf("got %d resources, want 2 -- a placeholder distance must not drop the item, "+
			"only its ranking", len(resources))
	}
	// The good reading sorts first; the placeholder, like an absent distance,
	// sorts last.
	if got := dig(resources[0], "id"); got != "res:pocra:facility:NEAR" {
		t.Errorf("resources[0] = %v, want NEAR (the one with a real distance) first", got)
	}
	if got := dig(resources[1], "id"); got != "res:pocra:facility:PLACEHOLDER" {
		t.Errorf("resources[1] = %v, want PLACEHOLDER last, same as an absent distance", got)
	}
}

// A non-numeric distance with no space -- "12Km" rather than "12 Km" -- is the
// same failure by a different route: $substringBefore returns the WHOLE string
// when there is no separator, so "12Km" reached $number unparsed.
func TestADistanceWithNoSpaceDoesNotFailTheSearch(t *testing.T) {
	body := pocraSingleWith("COMMON_PROVIDER_KVK", "kvk", "NOSPACE",
		`{ "descriptor": {"code":"distance"}, "value": "12Km" }`)
	answer := runAgainst(t, requestFor(t, "KrishiVigyanKendra"), body)
	if len(answerResources(t, answer)) != 1 {
		t.Fatalf("got %d resources, want 1 -- a malformed distance must not fail the search",
			len(answerResources(t, answer)))
	}
}

// A placeholder capacity_estimate is omitted, not fatal, on the one facility
// type that reports it.
func TestAPlaceholderCapacityDoesNotFailTheSearch(t *testing.T) {
	body := pocraSingleWith("WAREHOUSE001", "GSW", "WARE-PLACEHOLDER",
		`{ "descriptor": {"code":"capacity_estimate"}, "value": "Unknown" }`)
	answer := runAgainst(t, requestFor(t, "Warehouse"), body)
	resources := answerResources(t, answer)
	if len(resources) != 1 {
		t.Fatalf("got %d resources, want 1 -- a placeholder capacity must not fail the search",
			len(resources))
	}
	if capacity := dig(resources[0], "resourceAttributes", "capacity"); capacity != nil {
		t.Errorf("capacity = %v, want omitted for a placeholder value", capacity)
	}
}

// A real capacity still parses -- the guard added for the placeholder must not
// have broken the value it is meant to let through.
func TestARealCapacityStillParses(t *testing.T) {
	body := pocraSingleWith("WAREHOUSE001", "GSW", "WARE-REAL",
		`{ "descriptor": {"code":"capacity_estimate"}, "value": "500 tons" }`)
	answer := runAgainst(t, requestFor(t, "Warehouse"), body)
	resources := answerResources(t, answer)
	if len(resources) != 1 {
		t.Fatalf("got %d resources, want 1", len(resources))
	}
	capacity, ok := dig(resources[0], "resourceAttributes", "capacity").(map[string]any)
	if !ok {
		t.Fatalf("capacity = %v, want a parsed {value, unit}",
			dig(resources[0], "resourceAttributes", "capacity"))
	}
	if capacity["value"] != float64(500) || capacity["unit"] != "tons" {
		t.Errorf("capacity = %v, want {value: 500, unit: \"tons\"}", capacity)
	}
}

// A provider naming more than one fulfillment category must not crash the
// search either.
//
// $lookup throws T0410 on anything but a scalar key, and
// provider.fulfillments.categories.descriptor.code is a SEQUENCE the moment a
// provider has more than one fulfillment or a fulfillment has more than one
// category. Every shipped fixture has exactly one of each, so nothing else in
// this suite reaches the path.
//
// The provider id here deliberately does not match COMMON_PROVIDER_*, so type
// resolution falls all the way through to the fulfillment-category source this
// is testing -- the id and item-tag sources are tried first and would mask the
// bug otherwise.
func TestAProviderWithTwoFulfillmentCategoriesDoesNotFailTheSearch(t *testing.T) {
	body := `{ "responses": [ {
	  "context": { "action": "on_search", "version": "1.1.0" },
	  "message": { "catalog": { "descriptor": { "name": "x" },
	    "providers": [{
	      "id": "NOT_A_COMMON_PROVIDER",
	      "fulfillments": [
	        { "id": "f1", "categories": [{ "descriptor": { "code": "kvk" } }] },
	        { "id": "f2", "categories": [{ "descriptor": { "code": "chc" } }] }
	      ],
	      "items": [
	        { "id": "MIXED-1", "descriptor": {"name":"a"}, "address": {}, "contact": {},
	          "fulfillment_ids": ["f1","f2"], "category_ids": ["c1"],
	          "tags": [{ "list": [{ "descriptor": {"code":"distance"}, "value": "12 Km" }] }] }
	      ]
	    }]
	  } }
	} ] }`
	answer := runAgainst(t, requestFor(t, "KrishiVigyanKendra"), body)
	if len(answerResources(t, answer)) != 1 {
		t.Fatalf("got %d resources, want 1 -- a provider with two fulfillment categories "+
			"must not fail the search", len(answerResources(t, answer)))
	}
}

// An item carrying more than one category tag must not crash the search
// either -- same T0410 hazard as the fulfillment-categories case above, but
// for $item.tags.list[descriptor.code = "category"].value.
//
// providerID is deliberately not COMMON_PROVIDER_* and categoryCode is
// deliberately ungoverned, so type resolution falls through to the item's own
// tag -- the two sources tried first would otherwise mask the bug.
func TestADuplicateCategoryTagDoesNotFailTheSearch(t *testing.T) {
	body := pocraSingleWith("NOT_A_COMMON_PROVIDER", "not_governed", "DUP-CAT-1",
		`{ "descriptor": {"code":"category"}, "value": "kvk" },
		 { "descriptor": {"code":"category"}, "value": "chc" }`)
	answer := runAgainst(t, requestFor(t, "KrishiVigyanKendra"), body)
	resources := answerResources(t, answer)
	if len(resources) != 1 {
		t.Fatalf("got %d resources, want 1 -- a duplicated category tag must not fail the search",
			len(resources))
	}
	if got := dig(resources[0], "resourceAttributes", "facilityType"); got != "KrishiVigyanKendra" {
		t.Errorf("facilityType = %v, want the first tag that resolves", got)
	}
}

// A duplicated distance tag must not crash the search -- same T0410 hazard as
// the placeholder-distance case above, but from two VALID values rather than
// one placeholder.
func TestADuplicateDistanceTagDoesNotFailTheSearch(t *testing.T) {
	body := pocraSingleWith("COMMON_PROVIDER_KVK", "kvk", "DUP-DIST-1",
		`{ "descriptor": {"code":"distance"}, "value": "12 Km" },
		 { "descriptor": {"code":"distance"}, "value": "20 Km" }`)
	answer := runAgainst(t, requestFor(t, "KrishiVigyanKendra"), body)
	resources := answerResources(t, answer)
	if len(resources) != 1 {
		t.Fatalf("got %d resources, want 1 -- a duplicated distance tag must not fail the search",
			len(resources))
	}
}

// A duplicated capacity_estimate tag must not crash the search, and the value
// taken is the first, same as distance above.
func TestADuplicateCapacityTagDoesNotFailTheSearch(t *testing.T) {
	body := pocraSingleWith("WAREHOUSE001", "GSW", "DUP-CAP-1",
		`{ "descriptor": {"code":"capacity_estimate"}, "value": "500 tons" },
		 { "descriptor": {"code":"capacity_estimate"}, "value": "800 tons" }`)
	answer := runAgainst(t, requestFor(t, "Warehouse"), body)
	resources := answerResources(t, answer)
	if len(resources) != 1 {
		t.Fatalf("got %d resources, want 1 -- a duplicated capacity_estimate tag must not fail the search",
			len(resources))
	}
	capacity, ok := dig(resources[0], "resourceAttributes", "capacity").(map[string]any)
	if !ok {
		t.Fatalf("capacity = %v, want a parsed {value, unit}",
			dig(resources[0], "resourceAttributes", "capacity"))
	}
	if capacity["value"] != float64(500) || capacity["unit"] != "tons" {
		t.Errorf("capacity = %v, want the FIRST tag's {value: 500, unit: \"tons\"}", capacity)
	}
}

// supportedFacilityTypes repeating a type is refused rather than absorbed --
// it would otherwise pass both governed-type checks and make one duplicate
// upstream call per repeat for nothing, up to the fan-out ceiling.
func TestDuplicateFacilityTypesAreRefused(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
		t.Fatalf("the fixture is not JSON: %v", err)
	}
	attributes(t, payload)["supportedFacilityTypes"] = toAny([]string{"Warehouse", "Warehouse"})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("could not rebuild the payload: %v", err)
	}

	_, _, runErr := runSelect(t, string(body))
	if runErr == nil {
		t.Fatal("the payload was served, want it refused for repeating a facility type")
	}
	if !strings.Contains(runErr.Error(), "not repeat") {
		t.Errorf("error = %v, want the mapping's own explanation", runErr)
	}
}
