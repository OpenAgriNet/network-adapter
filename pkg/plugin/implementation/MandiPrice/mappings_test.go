package MandiPrice_test

// mappings_test.go runs the shipped mandi mapping through the real mapper and
// the real provider step. It is the only test that proves the three pieces fit:
// a mapping is JSONata inside YAML fetched over HTTP, and nothing but running
// it establishes that what is published actually produces valid Beckn.
//
// An external test package on purpose -- it uses the plugins exactly as the
// adapter does, through their exported surface and nothing else.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/MandiPrice"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// mappingsDir is where the shipped mappings live, relative to this package.
const mappingsDir = "../../../../config/mappings/agmarknet"

// shippedMapping is the file this binding-action publishes: one file, both
// directions. The action segment of the name must match the action the registry
// entry declares -- a mismatch would apply a correct mapping to the wrong call,
// silently.
const shippedMapping = "mandi-price.select.yaml"

// shippedCapability is what the pack calls this capability, and the second half
// of the binding key the registry indexes the provider's record by.
const shippedCapability = "openagrinet:MandiPrice"

const shippedBindingKey = "agmarknet|" + shippedCapability

// selectRequest is a MandiPrice select in OnDemand mode: it names the market and
// commodity it wants prices for, and carries no prices of its own -- the pack
// forbids that combination.
const selectRequest = `{
  "context": {
    "version": "2.0.0",
    "action": "select",
    "networkId": "oan-dev",
    "transactionId": "9f2c1a8e-4b70-4d31-9c55-6f2e0b1d7a44",
    "messageId": "7d41b9e0-52a6-4c18-8b73-1e9f0a4c6d22",
    "timestamp": "2026-09-03T06:12:01.330Z"
  },
  "message": {
    "contract": {
      "commitments": [
        {
          "status": { "descriptor": { "code": "DRAFT", "name": "Draft" } },
          "resources": [
            {
              "id": "res:agmarknet:price-enquiry",
              "quantity": { "count": 1 },
              "resourceAttributes": {
                "@context": "https://schemas.openagrinet.global/schema/MandiPrice/v0.1/context.jsonld",
                "@type": "openagrinet:MandiPrice",
                "informationMode": "OnDemand",
                "subjectCategories": ["Market"],
                "supportedCommodities": [{ "code": "2", "name": "Paddy(Common)" }],
                "supportedPriceFields": ["Minimum", "Maximum", "Modal"],
                "market": {
                  "marketName": "Kasdol APMC",
                  "marketCode": "2056",
                  "district": "96",
                  "state": "CG"
                },
                "validity": { "startsAt": "2025-08-20", "endsAt": "2025-08-21" }
              }
            }
          ],
          "offer": {
            "id": "offer:agmarknet:open-data",
            "resourceIds": ["res:agmarknet:price-enquiry"],
            "provider": {
              "id": "agmarknet",
              "descriptor": { "code": "AGMARKNET-01", "name": "Agmarknet Vistaar" }
            }
          }
        }
      ]
    }
  }
}`

// providerResponse is a verbatim Agmarknet Vistaar answer, taken from the
// working example in the provider backend's own documentation. Two records, so
// the mapping is exercised on a list rather than a single object.
//
// Note what it is: Title Case keys WITH SPACES, and prices as STRINGS. Both are
// the reason the mapping needs backticks and $number, and pinning a real
// capture here is what keeps that honest.
const providerResponse = `[
  {
    "Grade": "Non-FAQ",
    "Group": "Cereals",
    "State": "Chattisgarh",
    "Market": "Kasdol APMC",
    "Variety": "D.B.",
    "District": "Balodabazar",
    "Commodity": "Paddy(Common)",
    "Max Price": "2100",
    "Min Price": "1900",
    "Price Unit": "Rs./Qtl",
    "Modal Price": "2000",
    "Arrival Date": "20-08-2025"
  },
  {
    "Grade": "FAQ",
    "Group": "Cereals",
    "State": "Chattisgarh",
    "Market": "Kasdol APMC",
    "Variety": "Common",
    "District": "Balodabazar",
    "Commodity": "Paddy(Common)",
    "Price Unit": "Rs./Qtl",
    "Modal Price": "2050",
    "Arrival Date": "21-08-2025"
  }
]`

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

// runShipped drives the real step over the real mapping and returns the query
// the provider saw and the answer produced.
func runShipped(t *testing.T, request string) (url.Values, map[string]any) {
	return runShippedWith(t, request, providerResponse)
}

// runShippedWith is runShipped with the upstream's answer under the test's
// control, for the cases where what Agmarknet returns is the thing being
// exercised rather than the request that fetched it.
func runShippedWith(t *testing.T, request, providerBody string) (url.Values, map[string]any) {
	t.Helper()

	mappings := serveMappings(t)
	defer mappings.Close()

	var gotQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		fmt.Fprint(w, providerBody)
	}))
	defer upstream.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey:     shippedBindingKey,
		ParticipantID:  "agmarknet",
		CapabilityCode: shippedCapability,
		BaseURL:        upstream.URL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodGet, Path: "/v1/fetch-agmarknet-vistaar",
				Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000, RetryMax: 3},
		},
	}}

	step, closeStep, err := MandiPrice.New(context.Background(), registry, mapper,
		&MandiPrice.Config{
			BindingKeys: []string{shippedBindingKey},
			// Auth is per provider; these upstreams are stubs needing
			// no credential, and that is declared rather than defaulted.
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
	if len(stepCtx.ResponseBody) == 0 {
		t.Fatal("the step produced no answer")
	}
	var answer map[string]any
	if err := json.Unmarshal(stepCtx.ResponseBody, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}
	return gotQuery, answer
}

func TestShippedMappingServesARealSelect(t *testing.T) {
	gotQuery, answer := runShipped(t, selectRequest)

	// --- the request reached the provider as Agmarknet expects --------------
	// Every one of these comes off the payload. Nothing was resolved before the
	// call, which is the whole claim of this plugin having no prerequisites.
	for param, want := range map[string]string{
		"statecode":     "CG",
		"districtcode":  "96",
		"marketcode":    "2056",
		"commoditycode": "2",
		// dd-MM-yyyy, not the ISO the payload carried.
		"from_date": "20-08-2025",
		"to_date":   "21-08-2025",
	} {
		if got := gotQuery.Get(param); got != want {
			t.Errorf("upstream query %s = %q, want %q", param, got, want)
		}
	}
	// The credential is the adapter's business, never the mapping's.
	if gotQuery.Has("token") {
		t.Error("the mapping must not put a token in the query; authScheme does that")
	}

	// --- the answer is Beckn -----------------------------------------------
	beckncontext, _ := answer["context"].(map[string]any)
	if beckncontext["action"] != "on_select" {
		t.Errorf("action = %v, want on_select", beckncontext["action"])
	}
	if beckncontext["transactionId"] != "9f2c1a8e-4b70-4d31-9c55-6f2e0b1d7a44" {
		t.Errorf("transactionId = %v, want the one from the request", beckncontext["transactionId"])
	}
	// A mapping transforms a payload; it does not assert who anyone is.
	for _, field := range []string{"bapId", "bapUri", "bppId", "bppUri"} {
		if _, present := beckncontext[field]; present {
			t.Errorf("response context carries %q; a mapping must not assert identity", field)
		}
	}

	// Written out so the answer can be validated against the Beckn v2 spec and
	// the MandiPrice pack by tooling outside Go. Skipped unless asked for.
	if path := os.Getenv("MANDI_DUMP_ANSWER"); path != "" {
		raw, _ := json.MarshalIndent(answer, "", "  ")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("could not write the answer: %v", err)
		}
	}

	commitment := firstCommitment(t, answer)
	if status := commitment["status"].(map[string]any)["descriptor"].(map[string]any); status["code"] != "DRAFT" {
		t.Errorf("status = %v, want DRAFT -- the spec's enum is DRAFT, ACTIVE, CLOSED", status["code"])
	}

	// --- one resource per price record --------------------------------------
	resources, _ := commitment["resources"].([]any)
	if len(resources) != 2 {
		t.Fatalf("got %d resources, want 2 -- one per record the provider answered with", len(resources))
	}

	returned := make([]string, 0, len(resources))
	for _, entry := range resources {
		resource, _ := entry.(map[string]any)
		id, _ := resource["id"].(string)
		// Codes, not names: no spaces or brackets in an identifier.
		if !strings.HasPrefix(id, "res:agmarknet:2056:2:") {
			t.Errorf("resource id = %q, want one built from the market and commodity codes", id)
		}
		if strings.ContainsAny(id, " ()") {
			t.Errorf("resource id %q contains a space or bracket; use codes, not display names", id)
		}
		// Required by Commitment.resources in the spec even though the spec
		// defines no quantity property. The only shape it states is the one its
		// own error example implies: an object carrying a count.
		quantity, _ := resource["quantity"].(map[string]any)
		if _, present := quantity["count"].(float64); !present {
			t.Errorf("resource %s carries no quantity.count", id)
		}
		returned = append(returned, id)
	}

	// The offer must reference what was actually returned, not what was asked
	// for. This is the assertion that fails the moment the offer is echoed.
	offer, _ := commitment["offer"].(map[string]any)
	referenced, _ := offer["resourceIds"].([]any)
	if len(referenced) != len(returned) {
		t.Fatalf("offer references %d resources, want %d", len(referenced), len(returned))
	}
	for _, reference := range referenced {
		if !slices.Contains(returned, reference.(string)) {
			t.Errorf("offer references %v, which is not among the resources returned", reference)
		}
	}
	if offer["id"] != "offer:agmarknet:open-data" {
		t.Errorf("offer id = %v, want the one the request offered", offer["id"])
	}

	// --- the MandiPrice pack, Direct mode -----------------------------------
	first, _ := resources[0].(map[string]any)
	attributes, _ := first["resourceAttributes"].(map[string]any)
	for _, f := range []struct{ key, want string }{
		{"@type", "openagrinet:MandiPrice"},
		{"informationMode", "Direct"},
	} {
		if attributes[f.key] != f.want {
			t.Errorf("%s = %v, want %v", f.key, attributes[f.key], f.want)
		}
	}
	// Direct requires all six of these.
	for _, required := range []string{"source", "commodity", "market", "arrivalDate", "prices", "generatedAt"} {
		if attributes[required] == nil {
			t.Errorf("resourceAttributes carries no %q", required)
		}
	}
	// OnDemand's fields must NOT appear: the pack forbids prices alongside
	// them, and an answer advertising a capability is a category error.
	for _, absent := range []string{"supportedCommodities", "supportedPriceFields"} {
		if _, present := attributes[absent]; present {
			t.Errorf("a Direct answer must not carry %q", absent)
		}
	}

	// --- the prices, converted from strings ---------------------------------
	prices, _ := attributes["prices"].(map[string]any)
	for field, want := range map[string]float64{"minimum": 1900, "maximum": 2100, "modal": 2000} {
		got, ok := prices[field].(float64)
		if !ok {
			t.Errorf("prices.%s = %#v, want a number -- the upstream sends strings", field, prices[field])
			continue
		}
		if got != want {
			t.Errorf("prices.%s = %v, want %v", field, got, want)
		}
	}
	if prices["currency"] != "INR" || prices["unit"] != "Rs./Qtl" {
		t.Errorf("prices currency/unit = %v/%v, want INR/Rs./Qtl", prices["currency"], prices["unit"])
	}

	// arrivalDate is ISO in the answer, though the upstream reported dd-MM-yyyy.
	if attributes["arrivalDate"] != "2025-08-20" {
		t.Errorf("arrivalDate = %v, want 2025-08-20 in ISO", attributes["arrivalDate"])
	}

	// The pack's enum is Crop, Livestock, Weather, Market, Scheme, Knowledge,
	// Service -- so "Market", not "MarketPrice". Echoed from the request, which
	// is why getting it wrong there would produce an invalid answer here.
	categories, _ := attributes["subjectCategories"].([]any)
	if len(categories) != 1 || categories[0] != "Market" {
		t.Errorf("subjectCategories = %v, want [Market] from the pack's enum", categories)
	}

	market, _ := attributes["market"].(map[string]any)
	if market["marketName"] != "Kasdol APMC" || market["state"] != "Chattisgarh" {
		t.Errorf("market = %v, want the names the provider reported", market)
	}

	// --- a record the market reported only partially -------------------------
	// The second record has no Min or Max Price. Those must be absent, not zero:
	// a consumer must be able to tell "not reported" from "reported as zero".
	second, _ := resources[1].(map[string]any)
	secondPrices, _ := second["resourceAttributes"].(map[string]any)["prices"].(map[string]any)
	for _, absent := range []string{"minimum", "maximum"} {
		if _, present := secondPrices[absent]; present {
			t.Errorf("prices.%s is present for a record that did not report it", absent)
		}
	}
	if secondPrices["modal"] != float64(2050) {
		t.Errorf("the second record's modal price = %v, want 2050", secondPrices["modal"])
	}
}

// The pack leaves every field this upstream needs optional, so a spec-valid
// select can still be unanswerable. The mapping refuses those before the
// provider is called, with its own message.
func TestShippedMappingRefusesWhatItCannotServe(t *testing.T) {
	for _, tc := range []struct{ name, drop, expect string }{
		{"no commodity code", "supportedCommodities", "commodity code"},
		{"no market codes", "market", "codes in market"},
		{"no validity window", "validity", "validity window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Built by deleting a key from the decoded fixture rather than by
			// editing its text: removing the last member of an object leaves a
			// trailing comma, and the resulting parse error would look like a
			// mapping failure.
			var payload map[string]any
			if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			attributes := payload["message"].(map[string]any)["contract"].(map[string]any)["commitments"].([]any)[0].(map[string]any)["resources"].([]any)[0].(map[string]any)["resourceAttributes"].(map[string]any)
			if _, present := attributes[tc.drop]; !present {
				t.Fatalf("the fixture has no %q, so this case tests nothing", tc.drop)
			}
			delete(attributes, tc.drop)
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("could not rebuild the payload: %v", err)
			}

			mappings := serveMappings(t)
			defer mappings.Close()

			called := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				fmt.Fprint(w, providerResponse)
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
					"select": {Method: http.MethodGet, Path: "/v1/fetch-agmarknet-vistaar",
						Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000},
				},
			}}
			step, closeStep, err := MandiPrice.New(context.Background(), registry, mapper,
				&MandiPrice.Config{
					BindingKeys: []string{shippedBindingKey},
					// Auth is per provider; these upstreams are stubs needing
					// no credential, and that is declared rather than defaulted.
					AuthByProvider: map[string]*common.AuthProfile{
						strings.Split(shippedBindingKey, "|")[0]: {Scheme: util.AuthSchemeNone},
					},
				})
			if err != nil {
				t.Fatalf("failed to build the step: %v", err)
			}
			defer closeStep()

			stepCtx := &model.StepContext{Context: t.Context(), Body: body}
			if err := step.Run(stepCtx); err == nil {
				t.Fatal("expected an unserviceable payload to be refused")
			} else if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q should carry the mapping's own message about %q", err, tc.expect)
			}
			if called {
				t.Error("the provider was called for a payload the mapping refuses")
			}
		})
	}
}

// firstCommitment reaches the one commitment an answer carries.
func firstCommitment(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	message, _ := answer["message"].(map[string]any)
	contract, _ := message["contract"].(map[string]any)
	commitments, _ := contract["commitments"].([]any)
	if len(commitments) != 1 {
		t.Fatalf("got %d commitments, want 1", len(commitments))
	}
	commitment, _ := commitments[0].(map[string]any)
	return commitment
}

// resourcesOf returns the answer's resources as maps, so a test can look at
// each record the provider's rows became.
func resourcesOf(t *testing.T, answer map[string]any) []map[string]any {
	t.Helper()

	raw, _ := firstCommitment(t, answer)["resources"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("resource is not an object: %#v", r)
		}
		out = append(out, m)
	}
	return out
}

// Agmarknet writes an unreported price as a marker rather than omitting the
// field -- "NR", "-", "". $exists() is true for all of them, so an
// existence-only guard handed them to $number() and it threw D3030, which
// failed the WHOLE response: one unreported cell in one row turned a good
// multi-row answer into an adapter error.
func TestShippedMappingSurvivesUnreportedPrices(t *testing.T) {
	t.Parallel()

	// Row one has a marker in TWO price fields and a real modal, so it stays
	// and its markers must come back absent. Row two is ordinary. Row three
	// has markers in all three, so it has nothing to report and is dropped.
	const withMarkers = `[
	  {
	    "Grade": "Non-FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "D.B.", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "NR", "Max Price": "-", "Modal Price": "2000",
	    "Price Unit": "Rs./Qtl", "Arrival Date": "20-08-2025"
	  },
	  {
	    "Grade": "FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "Common", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "1900", "Max Price": "2100", "Modal Price": "2050",
	    "Price Unit": "Rs./Qtl", "Arrival Date": "21-08-2025"
	  },
	  {
	    "Grade": "FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "Common", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "NR", "Max Price": "NR", "Modal Price": "NR",
	    "Price Unit": "Rs./Qtl", "Arrival Date": "22-08-2025"
	  }
	]`

	_, answer := runShippedWith(t, selectRequest, withMarkers)

	byDate := map[string]map[string]any{}
	for _, r := range resourcesOf(t, answer) {
		ra := r["resourceAttributes"].(map[string]any)
		byDate[ra["arrivalDate"].(string)] = ra
	}

	// The row with a real modal survives -- one unreported cell must not
	// discard it, and must not discard the rows beside it either.
	partial, ok := byDate["2025-08-20"]
	if !ok {
		t.Fatal("the partially-priced row is missing; an unreported cell discarded it")
	}
	prices := partial["prices"].(map[string]any)
	if _, isNum := prices["modal"].(float64); !isNum {
		t.Errorf("prices.modal = %#v, want the reported number", prices["modal"])
	}
	// Absent, not zero: a consumer must tell "no minimum reported" from
	// "the minimum was zero".
	for _, field := range []string{"minimum", "maximum"} {
		if v, present := prices[field]; present {
			t.Errorf("prices.%s = %#v for an unreported price; absent is honest, zero is a lie", field, v)
		}
	}

	if _, ok := byDate["2025-08-21"]; !ok {
		t.Error("the fully-priced row is missing from the answer")
	}

	// Nothing to report at all: the pack's prices.anyOf cannot be satisfied,
	// so the row is dropped rather than emitted as an invalid resource.
	if _, ok := byDate["2025-08-22"]; ok {
		t.Error("a row whose every price is unreported was emitted; it cannot satisfy prices.anyOf")
	}
}

// A record that cannot produce a conformant resource is dropped, not emitted
// with a degenerate value. Absent is honest; present-and-wrong is a lie in the
// shape of an answer, and it is signed.
func TestShippedMappingDropsRecordsItCannotMakeConformant(t *testing.T) {
	t.Parallel()

	// One good row, then one for each way a record fails the pack.
	const mixed = `[
	  {
	    "Grade": "FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "Common", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "1900", "Max Price": "2100", "Modal Price": "2000",
	    "Price Unit": "Rs./Qtl", "Arrival Date": "20-08-2025"
	  },
	  {
	    "Grade": "FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "Common", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "1800", "Max Price": "2000", "Modal Price": "1900",
	    "Price Unit": "Rs./Qtl"
	  },
	  {
	    "Grade": "FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "Common", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "1700", "Max Price": "1900", "Modal Price": "1800",
	    "Arrival Date": "23-08-2025"
	  }
	]`

	_, answer := runShippedWith(t, selectRequest, mixed)

	resources := resourcesOf(t, answer)
	if len(resources) != 1 {
		var dates []string
		for _, r := range resources {
			ra := r["resourceAttributes"].(map[string]any)
			dates = append(dates, fmt.Sprint(ra["arrivalDate"]))
		}
		t.Fatalf("got %d resources with dates %v, want only the conformant one", len(resources), dates)
	}

	ra := resources[0]["resourceAttributes"].(map[string]any)
	if ra["arrivalDate"] != "2025-08-20" {
		t.Errorf("arrivalDate = %v, want the one good record", ra["arrivalDate"])
	}
	// The degenerate date the old mapping produced, specifically.
	if ra["arrivalDate"] == "--" {
		t.Error(`arrivalDate = "--", which the pack refuses as format: date`)
	}
	if _, ok := ra["prices"].(map[string]any)["unit"]; !ok {
		t.Error("prices.unit is missing, which the pack requires")
	}

	// The offer must not reference a resource the filter removed -- dropping a
	// record and leaving its id in resourceIds would trade an invalid resource
	// for a dangling reference.
	ids := map[string]bool{}
	for _, r := range resources {
		ids[r["id"].(string)] = true
	}
	offer, _ := firstCommitment(t, answer)["offer"].(map[string]any)
	referenced, _ := offer["resourceIds"].([]any)
	if len(referenced) != len(resources) {
		t.Errorf("offer references %d resources, want %d", len(referenced), len(resources))
	}
	for _, ref := range referenced {
		if !ids[fmt.Sprint(ref)] {
			t.Errorf("the offer references %q, which is not among the answer's resources", ref)
		}
	}
}

// The answer must assert what it knows, not repeat what it was told. Both of
// these were echoes of the request, and an echo is a claim this adapter signs.
func TestShippedMappingStatesWhatItKnowsRatherThanEchoing(t *testing.T) {
	t.Parallel()

	// A caller asking a MandiPrice question with the WRONG category. It is
	// enum-legal, so nothing downstream would reject it -- which is exactly
	// why echoing it was dangerous.
	wrongCategory := strings.Replace(selectRequest,
		`"subjectCategories": ["Market"]`, `"subjectCategories": ["Weather"]`, 1)
	if wrongCategory == selectRequest {
		t.Fatal("the fixture no longer states subjectCategories; this test needs updating")
	}

	_, answer := runShippedWith(t, wrongCategory, providerResponse)

	for _, r := range resourcesOf(t, answer) {
		ra := r["resourceAttributes"].(map[string]any)

		// Stated from the pack, not taken from the caller.
		cats, _ := ra["subjectCategories"].([]any)
		if len(cats) != 1 || cats[0] != "Market" {
			t.Errorf(`subjectCategories = %#v, want ["Market"] regardless of what the request said`, ra["subjectCategories"])
		}

		market, _ := ra["market"].(map[string]any)
		// The upstream reports no market code, so the answer must not claim one.
		if code, present := market["marketCode"]; present {
			t.Errorf("market.marketCode = %#v; this upstream reports no code, so asserting one is unfounded", code)
		}
		// And what is there comes from the record.
		if market["marketName"] != "Kasdol APMC" || market["state"] != "Chattisgarh" {
			t.Errorf("market = %#v, want the values the provider reported", market)
		}
		// marketName is the one member the pack requires.
		if _, ok := market["marketName"]; !ok {
			t.Error("market.marketName is missing, which the pack requires")
		}
	}
}

// The guards now check what they claim to. Both of these payloads are legal
// against the pack and unanswerable by this upstream, which is the gap between
// "valid" and "serviceable" the required block exists to close.
func TestShippedMappingRefusesPayloadsItCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		expect string
	}{
		{
			// The pack describes district and state as "name or governed
			// code", so this validates -- and went to Agmarknet verbatim as
			// codes, which answered nothing. The caller then received a
			// signed, spec-valid "no prices" for a market that had prices.
			name: "names where the upstream wants codes",
			mutate: func(ra map[string]any) {
				ra["market"] = map[string]any{
					"marketName": "Kasdol APMC",
					"district":   "Balodabazar",
					"state":      "Chattisgarh",
				}
			},
			expect: "codes in market",
		},
		{
			// Everything downstream reads supportedCommodities[0], so this
			// used to be queried for Paddy alone and answered confidently.
			name: "more commodities than one request can serve",
			mutate: func(ra map[string]any) {
				ra["supportedCommodities"] = []any{
					map[string]any{"code": "2", "name": "Paddy(Common)"},
					map[string]any{"code": "3", "name": "Wheat"},
				}
			},
			expect: "exactly one commodity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(selectRequest), &payload); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			ra := payload["message"].(map[string]any)["contract"].(map[string]any)["commitments"].([]any)[0].(map[string]any)["resources"].([]any)[0].(map[string]any)["resourceAttributes"].(map[string]any)
			tc.mutate(ra)
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("could not rebuild the payload: %v", err)
			}

			mappings := serveMappings(t)
			defer mappings.Close()

			called := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				fmt.Fprint(w, providerResponse)
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
					"select": {Method: http.MethodGet, Path: "/v1/fetch-agmarknet-vistaar",
						Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000},
				},
			}}
			step, closeStep, err := MandiPrice.New(context.Background(), registry, mapper,
				&MandiPrice.Config{
					BindingKeys: []string{shippedBindingKey},
					// Auth is per provider; these upstreams are stubs needing
					// no credential, and that is declared rather than defaulted.
					AuthByProvider: map[string]*common.AuthProfile{
						strings.Split(shippedBindingKey, "|")[0]: {Scheme: util.AuthSchemeNone},
					},
				})
			if err != nil {
				t.Fatalf("failed to build the step: %v", err)
			}
			defer closeStep()

			stepCtx := &model.StepContext{Context: t.Context(), Body: body}
			if err := step.Run(stepCtx); err == nil {
				t.Fatal("expected a payload this upstream cannot answer to be refused")
			} else if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q should explain the refusal in terms of %q", err, tc.expect)
			}
			// The point of refusing early: the upstream is never troubled with
			// a request that cannot produce an answer.
			if called {
				t.Error("the provider was called for a payload the mapping refuses")
			}
		})
	}
}

// Agmarknet routinely reports several rows for the same market, commodity and
// date differing only by Variety and Grade. The id was built from
// scope:commodity:date, so those rows collided: two distinct resources under
// one id, referenced twice by the offer, and a consumer resolving resourceIds
// could not tell which price it had.
func TestShippedMappingGivesCollidingRowsDistinctIDs(t *testing.T) {
	t.Parallel()

	// The same pair as this package's own fixture -- FAQ/Common and
	// Non-FAQ/D.B. -- but sharing an arrival date, which is what the fixture
	// was accidentally saved by not doing.
	const sameDay = `[
	  {
	    "Grade": "Non-FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "D.B.", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "1900", "Max Price": "2100", "Modal Price": "2000",
	    "Price Unit": "Rs./Qtl", "Arrival Date": "20-08-2025"
	  },
	  {
	    "Grade": "FAQ", "Group": "Cereals", "State": "Chattisgarh",
	    "Market": "Kasdol APMC", "Variety": "Common", "District": "Balodabazar",
	    "Commodity": "Paddy(Common)",
	    "Min Price": "1600", "Max Price": "1800", "Modal Price": "1700",
	    "Price Unit": "Rs./Qtl", "Arrival Date": "20-08-2025"
	  }
	]`

	_, answer := runShippedWith(t, selectRequest, sameDay)

	resources := resourcesOf(t, answer)
	if len(resources) != 2 {
		t.Fatalf("got %d resources, want both rows", len(resources))
	}
	seen := map[string]int{}
	for _, r := range resources {
		seen[r["id"].(string)]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id %q is shared by %d resources; the offer cannot reference one of them unambiguously", id, n)
		}
	}
	if len(seen) != 2 {
		t.Errorf("got %d distinct ids for 2 rows: %v", len(seen), seen)
	}
}

// supportedPriceFields was validated on the way in and ignored on the way out,
// so a caller asking for one field got all three.
func TestShippedMappingHonoursTheRequestedPriceFields(t *testing.T) {
	t.Parallel()

	modalOnly := strings.Replace(selectRequest,
		`"supportedPriceFields": ["Minimum", "Maximum", "Modal"]`,
		`"supportedPriceFields": ["Modal"]`, 1)
	if modalOnly == selectRequest {
		t.Fatal("the fixture no longer lists all three price fields; this test needs updating")
	}

	_, answer := runShippedWith(t, modalOnly, providerResponse)

	for _, r := range resourcesOf(t, answer) {
		prices := r["resourceAttributes"].(map[string]any)["prices"].(map[string]any)
		if _, ok := prices["modal"]; !ok {
			t.Error("prices.modal is missing, and it is the field that was asked for")
		}
		for _, unasked := range []string{"minimum", "maximum"} {
			if v, present := prices[unasked]; present {
				t.Errorf("prices.%s = %#v was returned though the request did not ask for it", unasked, v)
			}
		}
		// The pack requires these whatever was asked for.
		for _, required := range []string{"currency", "unit"} {
			if _, ok := prices[required]; !ok {
				t.Errorf("prices.%s is missing, which the pack requires", required)
			}
		}
	}
}
