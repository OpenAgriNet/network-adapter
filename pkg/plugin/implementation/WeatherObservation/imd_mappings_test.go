package WeatherObservation_test

// imd_mappings_test.go is the IMD half of the shipped-mapping tests. Same
// package, so it reuses stubRegistry, firstCommitment and assertParameter from
// mappings_test.go; a separate file because IMD's mappings live in their own
// directory and its answer is a different shape.
//
// What only this file proves: that the station is resolved. IMD is addressed by
// a station id, that id is nowhere in the payload, and it reaches the query
// only because the step was configured with a resolver.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/WeatherObservation"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// imdMappingsDirEnv overrides where the shipped IMD mappings are read from.
// CI sets it; locally the default below is usually right.
const imdMappingsDirEnv = "IMD_MAPPINGS_DIR"

// imdMappingsDirDefault is the helmcharts checkout beside this one. Mappings
// are SERVED from that repository, not this one -- the registry points at its
// raw URL -- so there is one copy and these tests read the same file the
// deployment does.
const imdMappingsDirDefault = "../../../../../helmcharts/quick-start/config/mappings/imd"

// imdMappingsDir returns that directory, or skips: a mapping this checkout
// cannot see is not a failure, it is a test that does not apply.
func imdMappingsDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(imdMappingsDirEnv)
	if dir == "" {
		dir = imdMappingsDirDefault
	}
	if _, err := os.Stat(filepath.Join(dir, imdMapping)); err != nil {
		t.Skipf("no IMD mappings at %s; set %s to a helmcharts checkout", dir, imdMappingsDirEnv)
	}
	return dir
}

// imdMapping is the file this binding-action publishes: one file, both
// directions. The registry carries its full URL, and the action segment of the
// name must match what that entry declares.
const imdMapping = "weather-observation.select.yaml"

// imdBindingKey is the capability these tests exercise. Named here because the
// package has no default: it serves whatever a deployment configures.
const imdBindingKey = "imd|openagrinet:WeatherObservation"

// imdPrerequisite is the resolver a provider names to be served a station id.
// Configuration, not a binding in Go.
const imdPrerequisite = "findStation"

// imdStation is what the mocked prerequisite returns: NANCOWRY, in the
// Nicobars.
const imdStation = "43382"

// imdRequestedResourceID is what the caller asks for: the CAPABILITY, not a
// station and not a day. The station is resolved by the prerequisite and the
// days come back in the answer, so neither belongs in an id the caller writes.
const imdRequestedResourceID = "res:imd:city-forecast"

// imdSelectRequest is a /select for IMD, naming it as the provider and carrying
// a point near the station the mock resolves to.
//
// The @context is on raw.githubusercontent.com because that is the only domain
// the adapters' extendedSchema_allowedDomains permits; any other host is
// refused with SCH_INVALID_JSONLD_CONTEXT before a step ever runs.
const imdSelectRequest = `{
  "context": { "version": "2.0.0", "action": "select",
    "networkId": "da.gov.in/vistaar",
    "transactionId": "c1b7e2a4-9f3d-4c58-8a11-0e6d5b2f7c93",
    "messageId": "3e8a0d61-7b24-4f19-9c02-5a7f1e4b8d60",
    "timestamp": "2026-01-22T04:30:00.000Z" },
  "message": { "contract": { "commitments": [{
    "status": { "descriptor": { "code": "DRAFT", "name": "Draft" } },
    "resources": [{
      "id": "res:imd:city-forecast",
      "quantity": 1,
      "resourceAttributes": {
        "@context": "https://raw.githubusercontent.com/OpenAgriNet/network-specs/schema-packs-v0.1/schema/WeatherObservation/v0.1/context.jsonld",
        "@type": "openagrinet:WeatherObservation",
        "subjectCategories": ["Weather"],
        "location": { "type": "Point", "coordinates": [93.55, 7.98333] },
        "informationMode": "OnDemand",
        "supportedObservationTypes": ["Forecast"],
        "supportedParameters": ["Rainfall", "Temperature", "Humidity"],
        "geographicGranularities": ["Point"]
      }
    }],
    "offer": {
      "id": "offer:imd:open-data",
      "resourceIds": ["res:imd:city-forecast"],
      "provider": { "id": "imd",
                    "descriptor": { "code": "IMD-CITY-01", "name": "IMD City Weather" } }
    }
  }] } }
}`

// imdResponse is IMD's own shape, with the field names the old per-provider
// service read: ONE FLAT OBJECT for every day, the day number inside the field
// name, and the casing inconsistent between Max_Temp and Min_temp. Three days,
// so the mapping is exercised against a partial answer.
const imdResponse = `[{
  "Date": "2026-01-22",
  "Station_Code": "43382",
  "Station_Name": "NANCOWRY",
  "Past_24_hrs_Rainfall": 7.00,
  "Relative_Humidity_at_0830": 88,
  "Relative_Humidity_at_1730": 89,
  "Todays_Forecast_Max_Temp": 29.8,
  "Todays_Forecast_Min_temp": 23.4,
  "Todays_Forecast": "Generally cloudy sky with Light rain",
  "Day_2_Max_Temp": 30.1,
  "Day_2_Min_temp": 23.8,
  "Day_2_Forecast": "Partly cloudy sky",
  "Day_3_Max_Temp": 30.5,
  "Day_3_Min_temp": 24.0,
  "Latitude": 7.98333,
  "Longitude": 93.55
}]`

// serveIMDMappings publishes the shipped IMD mapping over HTTP, the way the
// registry's reference points at it.
func serveIMDMappings(t *testing.T) *httptest.Server {
	t.Helper()
	dir := imdMappingsDir(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(filepath.Join(dir, filepath.Base(r.URL.Path)))
		if err != nil {
			t.Errorf("could not read the mapping %q: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, string(body))
	}))
}

// TestIMDMappingServesASelect runs the published IMD mapping end to end.
func TestIMDMappingServesASelect(t *testing.T) {
	mappings := serveIMDMappings(t)
	defer mappings.Close()

	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		fmt.Fprint(w, imdResponse)
	}))
	defer upstream.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey:     imdBindingKey,
		ParticipantID:  "imd",
		CapabilityCode: "openagrinet:WeatherObservation",
		BaseURL:        upstream.URL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodGet, Path: "/api/weather",
				Mappings: mappings.URL + "/" + imdMapping, TimeoutMs: 30000, RetryMax: 1},
		},
	}}

	// No prerequisite is configured: prerequisites.go binds findStation to this
	// binding key, so serving the key is what brings the station with it.
	step, closeStep, err := WeatherObservation.New(context.Background(), registry, mapper,
		&WeatherObservation.Config{
			BindingKeys: []string{imdBindingKey},
			// IMD's endpoint is open, and that is declared rather than
			// defaulted: auth is per provider and a served provider with no
			// profile is refused at startup.
			AuthByProvider: map[string]*common.AuthProfile{
				"imd": {Scheme: util.AuthSchemeNone},
			},
			// The resolver is NAMED, the way a deployment names it beside the
			// credentials. Nothing in Go binds it to this participant id.
			PrerequisiteByProvider: map[string]string{"imd": imdPrerequisite},
		})
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	defer closeStep()

	stepCtx := &model.StepContext{Context: t.Context(), Body: []byte(imdSelectRequest)}
	if err := step.Run(stepCtx); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}

	// --- the request reached the provider correctly -------------------------
	// The path is the registry's, and the station is the PREREQUISITE'S: it
	// appears nowhere in the payload, so this is the assertion that fails if
	// prerequisites.go stops resolving it.
	if gotPath != "/api/weather" {
		t.Errorf("upstream path = %q, want /api/weather from the registry row", gotPath)
	}
	if want := "id=" + imdStation; !strings.Contains(gotQuery, want) {
		t.Errorf("upstream query %q is missing %q -- the station prerequisite did not resolve",
			gotQuery, want)
	}

	// --- the response mapping produced Beckn --------------------------------
	if len(stepCtx.ResponseBody) == 0 {
		t.Fatal("the step produced no answer")
	}
	var answer map[string]any
	if err := json.Unmarshal(stepCtx.ResponseBody, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}

	beckncontext, _ := answer["context"].(map[string]any)
	if beckncontext["action"] != "on_select" {
		t.Errorf("action = %v, want on_select", beckncontext["action"])
	}
	// The transaction has to survive the round trip, or the caller cannot match
	// the answer to what it asked.
	if beckncontext["transactionId"] != "c1b7e2a4-9f3d-4c58-8a11-0e6d5b2f7c93" {
		t.Errorf("transactionId = %v, want the one from the request", beckncontext["transactionId"])
	}
	// A mapping transforms a payload; it does not assert who anyone is.
	for _, field := range []string{"bapId", "bapUri", "bppId", "bppUri"} {
		if _, present := beckncontext[field]; present {
			t.Errorf("response context carries %q; a mapping must not assert identity", field)
		}
	}

	commitment := firstCommitment(t, answer)
	status, _ := commitment["status"].(map[string]any)
	descriptor, _ := status["descriptor"].(map[string]any)
	if descriptor["code"] != "DRAFT" {
		t.Errorf("status = %v, want DRAFT -- QUOTED is not in the spec's enum", descriptor["code"])
	}

	// One resource per day IMD reported, and THE DATES ARE DERIVED: IMD gives
	// one date and names the rest by number. A day numbered wrong here is a
	// forecast delivered for the wrong day, which nothing downstream could
	// detect.
	resources, _ := commitment["resources"].([]any)
	if len(resources) != 3 {
		t.Fatalf("got %d resources, want 3 -- one per day IMD reported", len(resources))
	}

	wantDates := []string{"2026-01-22", "2026-01-23", "2026-01-24"}
	returned := make([]string, 0, len(resources))
	for i, entry := range resources {
		resource, _ := entry.(map[string]any)
		id, _ := resource["id"].(string)
		// The day distinguishes one answered resource from another; the station
		// does not appear. It is how this provider is addressed internally, and
		// a consumer reading it out of an id would depend on that.
		if want := "res:imd:forecast:" + wantDates[i]; id != want {
			t.Errorf("resource %d id = %q, want %q", i, id, want)
		}
		if strings.Contains(id, imdStation) {
			t.Errorf("resource %d id = %q leaks the station id", i, id)
		}
		// Required by Commitment.resources in the spec even though the spec
		// defines no quantity property -- a consumer that validates refuses an
		// answer without it.
		if _, present := resource["quantity"]; !present {
			t.Errorf("resource %s carries no quantity; the spec requires one", id)
		}
		attributes, _ := resource["resourceAttributes"].(map[string]any)
		validity, _ := attributes["validity"].(map[string]any)
		if validity["startsAt"] != wantDates[i] || validity["endsAt"] != wantDates[i] {
			t.Errorf("resource %d validity = %v, want the single day %q", i, validity, wantDates[i])
		}
		returned = append(returned, id)
	}

	// The offer must reference the days actually returned, not the station that
	// was asked for. This is the assertion that fails the moment the offer is
	// echoed unchanged.
	offer, _ := commitment["offer"].(map[string]any)
	referenced, _ := offer["resourceIds"].([]any)
	if len(referenced) != len(returned) {
		t.Fatalf("offer.resourceIds has %d entries, want %d -- one per resource returned",
			len(referenced), len(returned))
	}
	for i, entry := range referenced {
		if entry == imdRequestedResourceID {
			t.Errorf("offer.resourceIds[%d] still names the capability the request asked "+
				"for; it must name the days that came back", i)
		}
	}
	// And the descriptor the request offered is still there: only the
	// references are rewritten, not the offer.
	if offer["id"] != "offer:imd:open-data" {
		t.Errorf("offer id = %v, want the one the request offered", offer["id"])
	}

	// --- the WeatherObservation schema pack, Direct mode ---------------------
	first, _ := resources[0].(map[string]any)
	attributes, _ := first["resourceAttributes"].(map[string]any)
	for _, f := range []struct{ key, want string }{
		{"@type", "openagrinet:WeatherObservation"},
		{"informationMode", "Direct"},
		{"observationType", "Forecast"},
	} {
		if attributes[f.key] != f.want {
			t.Errorf("%s = %v, want %v", f.key, attributes[f.key], f.want)
		}
	}
	// modelRunAt is what a Direct Forecast requires on top of the other five.
	for _, required := range []string{"source", "location", "generatedAt", "modelRunAt", "validity", "parameters"} {
		if attributes[required] == nil {
			t.Errorf("resourceAttributes carries no %q", required)
		}
	}

	// GeoJSON order, and the station's own coordinates as IMD echoed them.
	// Reading them the other way round gives a point in the wrong hemisphere
	// that is still a valid answer.
	location, _ := attributes["location"].(map[string]any)
	coordinates, _ := location["coordinates"].([]any)
	if len(coordinates) != 2 || coordinates[0] != 93.55 || coordinates[1] != 7.98333 {
		t.Errorf("coordinates = %v, want [93.55, 7.98333] in GeoJSON order", coordinates)
	}

	// Day 1 takes the FORECAST temperatures, not the observed ones: IMD sends
	// both, they are not the same thing, and this resource is labelled Forecast.
	parameters, _ := attributes["parameters"].([]any)
	if len(parameters) != 6 {
		t.Errorf("got %d parameters on day 1, want 6 for a fully-reported day", len(parameters))
	}
	assertParameter(t, parameters, "Temperature", "Maximum", "Cel", 29.8)
	assertParameter(t, parameters, "Temperature", "Minimum", "Cel", 23.4)
	assertParameter(t, parameters, "Rainfall", "Total", "mm", 7.00)
	assertParameter(t, parameters, "Humidity", "Minimum", "%", 88.0)

	// A description is a parameter, not a field of its own: the pack has no
	// advisory property but does have an Alert parameter.
	assertAlert(t, parameters, "Generally cloudy sky with Light rain")

	// --- readings IMD reports once, for the station -------------------------
	// Rainfall and humidity are a past-24-hour total and two fixed-hour
	// readings, so they belong to day 1 alone. A later day repeating them would
	// be inventing data.
	second, _ := resources[1].(map[string]any)
	secondAttributes, _ := second["resourceAttributes"].(map[string]any)
	secondParameters, _ := secondAttributes["parameters"].([]any)
	if len(secondParameters) != 3 {
		t.Errorf("got %d parameters on day 2, want 3 -- two temperatures and the description",
			len(secondParameters))
	}
	for _, entry := range secondParameters {
		p, _ := entry.(map[string]any)
		if p["parameter"] == "Rainfall" || p["parameter"] == "Humidity" {
			t.Errorf("day 2 carries %v; IMD reports it once for the station, not per day",
				p["parameter"])
		}
	}

	// --- a day IMD reported only partially -----------------------------------
	// Readings it did not take are absent, not present and empty: a consumer
	// must be able to tell "no description given" from an empty one.
	third, _ := resources[2].(map[string]any)
	thirdAttributes, _ := third["resourceAttributes"].(map[string]any)
	thirdParameters, _ := thirdAttributes["parameters"].([]any)
	if len(thirdParameters) != 2 {
		t.Errorf("got %d parameters on day 3, want only the 2 temperatures reported",
			len(thirdParameters))
	}
	for _, entry := range thirdParameters {
		if p, _ := entry.(map[string]any); p["parameter"] == "Alert" {
			t.Error("a day IMD gave no description for must carry no Alert parameter")
		}
	}
}

// WHICH provider resolves WHAT is configuration, and these are the two things
// that has to get right. Construction only -- no mapping, no upstream.
//
// A resolver bound to a participant id in Go would need none of this, and would
// also silently resolve nothing the moment a deployment renamed its provider --
// which this stack does, running mausamgram-mock and agmarknet-mock.
func TestPrerequisiteIsNamedByConfig(t *testing.T) {
	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{BindingKey: imdBindingKey}}
	auth := map[string]*common.AuthProfile{"imd": {Scheme: util.AuthSchemeNone}}

	// An unknown name fails AT CONSTRUCTION. The handler builds every provider
	// step at startup, so a typo stops the process instead of leaving a
	// capability that resolves nothing until a request arrives.
	t.Run("an unknown name is refused at startup", func(t *testing.T) {
		_, _, err := WeatherObservation.New(context.Background(), registry, mapper,
			&WeatherObservation.Config{
				BindingKeys:            []string{imdBindingKey},
				AuthByProvider:         auth,
				PrerequisiteByProvider: map[string]string{"imd": "findStaton"},
			})
		if err == nil {
			t.Fatal("an unknown prerequisite must not build")
		}
		// The message has to name the alternatives, or a reader has to go and
		// find the catalogue in the source.
		if !strings.Contains(err.Error(), imdPrerequisite) {
			t.Errorf("error %q should list %q as available", err, imdPrerequisite)
		}
	})

	// none and absent mean the same thing, and both have to build: every other
	// provider in the pipeline is one of them.
	for _, prerequisite := range []string{"none", ""} {
		t.Run("prerequisite "+prerequisite+" builds", func(t *testing.T) {
			_, closeStep, err := WeatherObservation.New(context.Background(), registry, mapper,
				&WeatherObservation.Config{
					BindingKeys:            []string{imdBindingKey},
					AuthByProvider:         auth,
					PrerequisiteByProvider: map[string]string{"imd": prerequisite},
				})
			if err != nil {
				t.Fatalf("a provider needing nothing must build: %v", err)
			}
			defer closeStep()
		})
	}
}

// The station id is the one thing the request half cannot get from the payload.
// This is where "the prerequisite is wired" is checked directly, without the
// upstream call in the way.
func TestIMDMappingSendsTheResolvedStation(t *testing.T) {
	mappings := serveIMDMappings(t)
	defer mappings.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	var beckn any
	if err := json.Unmarshal([]byte(imdSelectRequest), &beckn); err != nil {
		t.Fatalf("failed to decode the request: %v", err)
	}

	got, err := mapper.Transform(context.Background(), mappings.URL+"/"+imdMapping,
		definition.DirectionRequest, map[string]any{
			"beckn":  beckn,
			"_local": map[string]any{"stationId": imdStation},
		})
	if err != nil {
		t.Fatalf("the request half returned an unexpected error: %v", err)
	}

	var query map[string]any
	if err := json.Unmarshal(got, &query); err != nil {
		t.Fatalf("the request half produced something that is not an object: %v", err)
	}

	// Named id because that is what IMD reads.
	if query["id"] != imdStation {
		t.Errorf("id = %v, want the resolved station %q", query["id"], imdStation)
	}
}

// A binding key with no prerequisite entry has to fail loudly. Without the
// mapping's assertion it would send an empty id and read IMD's 404 as an
// outage, which says nothing about the cause.
func TestIMDMappingRefusesAnUnresolvedStation(t *testing.T) {
	mappings := serveIMDMappings(t)
	defer mappings.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	var beckn any
	if err := json.Unmarshal([]byte(imdSelectRequest), &beckn); err != nil {
		t.Fatalf("failed to decode the request: %v", err)
	}

	// No _local at all, which is what a capability with no prerequisite entry
	// looks like to the mapping.
	_, err = mapper.Transform(context.Background(), mappings.URL+"/"+imdMapping,
		definition.DirectionRequest, map[string]any{"beckn": beckn})
	if err == nil {
		t.Fatal("the request half accepted a payload with no resolved station")
	}
	if !strings.Contains(err.Error(), "station") {
		t.Errorf("error %q should say the station was not resolved", err)
	}
}

// The point is required even though it is not what is sent upstream: it is
// what the station is resolved from. The mapping's own precondition, checked
// against the published file.
func TestIMDMappingPreconditions(t *testing.T) {
	mappings := serveIMDMappings(t)
	defer mappings.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()
	ref := mappings.URL + "/" + imdMapping

	payload := func(t *testing.T, geometry string) map[string]any {
		t.Helper()
		location := ""
		if geometry != "" {
			location = `"location": ` + geometry + `,`
		}
		body := `{"context":{"action":"select"},"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{` +
			location + `"@type":"openagrinet:WeatherObservation"}}]}]}}}`
		var beckn any
		if err := json.Unmarshal([]byte(body), &beckn); err != nil {
			t.Fatalf("failed to build the payload: %v", err)
		}
		return map[string]any{"beckn": beckn}
	}

	t.Run("a Point is served", func(t *testing.T) {
		if err := mapper.Verify(context.Background(), ref,
			payload(t, `{"type":"Point","coordinates":[93.55,7.98333]}`)); err != nil {
			t.Errorf("a Point must be served: %v", err)
		}
	})

	for _, tc := range []struct{ name, geometry string }{
		{"a polygon", `{"type":"Polygon","coordinates":[[[93.0,7.0],[94.0,7.0],[94.0,8.0],[93.0,7.0]]]}`},
		{"no location at all", ``},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			err := mapper.Verify(context.Background(), ref, payload(t, tc.geometry))
			if err == nil {
				t.Fatalf("expected %s to be refused", tc.name)
			}
			// The message is the mapping's, and has to name what is needed.
			if !strings.Contains(err.Error(), "Point") {
				t.Errorf("error %q should say a Point is what this capability needs", err)
			}
		})
	}
}

// How many days IMD answers with is IMD's business, not the mapping's. It
// reports fewer whenever it has fewer, and the mapping builds days by NAME, so
// a day dropped in the middle must not shift the ones after it.
func TestIMDMappingTakesHoweverManyDaysIMDSent(t *testing.T) {
	mappings := serveIMDMappings(t)
	defer mappings.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	var beckn any
	if err := json.Unmarshal([]byte(imdSelectRequest), &beckn); err != nil {
		t.Fatalf("failed to decode the request: %v", err)
	}

	for _, days := range []int{1, 4, 7} {
		t.Run(fmt.Sprintf("%d days", days), func(t *testing.T) {
			provider := map[string]any{
				"Date":                     "2026-01-22",
				"Station_Code":             imdStation,
				"Todays_Forecast_Max_Temp": 29.8,
			}
			for i := 2; i <= days; i++ {
				provider[fmt.Sprintf("Day_%d_Max_Temp", i)] = float64(30 + i)
			}

			got, err := mapper.Transform(context.Background(), mappings.URL+"/"+imdMapping,
				definition.DirectionResponse, map[string]any{
					"beckn":    beckn,
					"_local":   map[string]any{"stationId": imdStation},
					"response": provider,
				})
			if err != nil {
				t.Fatalf("the response half returned an unexpected error: %v", err)
			}

			var answer map[string]any
			if err := json.Unmarshal(got, &answer); err != nil {
				t.Fatalf("the answer is not JSON: %v", err)
			}

			commitment := firstCommitment(t, answer)
			resources, _ := commitment["resources"].([]any)
			if len(resources) != days {
				t.Fatalf("got %d resources, want %d -- one per day IMD sent", len(resources), days)
			}

			// Day N is the reported date plus N-1 days, in order.
			for i, entry := range resources {
				resource, _ := entry.(map[string]any)
				attributes, _ := resource["resourceAttributes"].(map[string]any)
				validity, _ := attributes["validity"].(map[string]any)
				want := fmt.Sprintf("2026-01-%02d", 22+i)
				if validity["startsAt"] != want {
					t.Errorf("resource %d is dated %v, want %q", i, validity["startsAt"], want)
				}
			}
		})
	}
}
