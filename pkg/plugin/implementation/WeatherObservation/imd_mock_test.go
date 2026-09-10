package WeatherObservation_test

// imd_mock_test.go points the real step at a real mockimd process, so the mock
// and the shipped mapping are proved to fit each other rather than each fitting
// a fixture in this repository.
//
// OPT-IN: it skips unless MOCKIMD_ADDR is set, because it needs a process this
// package cannot start. Everything else here runs against a stub and needs
// nothing.
//
//	cd quick-start/mock-server/mockimd && go build -o /tmp/mockimd .
//	/tmp/mockimd -addr=:9102 &
//	MOCKIMD_ADDR=http://127.0.0.1:9102 go test -run TestIMDMappingFitsTheMock ./...
//
// Worth running with the mock's knobs moved -- -days at 1, 5 and 7, -imd-wrap
// at array, data and object. The resource count should follow the day count and
// the envelope should make no difference at all.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/WeatherObservation"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// mockAddrEnv names the running mock. Absent means skip: a mock that is not
// running is not a failure, it is a test that does not apply.
const mockAddrEnv = "MOCKIMD_ADDR"

func TestIMDMappingFitsTheMock(t *testing.T) {
	base := os.Getenv(mockAddrEnv)
	if base == "" {
		t.Skipf("%s is not set; start mockimd and set it to test against a real process", mockAddrEnv)
	}

	mappings := serveIMDMappings(t)
	defer mappings.Close()

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	defer closeMapper()

	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey:     imdBindingKey,
		ParticipantID:  "imd",
		CapabilityCode: "openagrinet:WeatherObservation",
		BaseURL:        base,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodGet, Path: "/api/weather",
				Mappings: mappings.URL + "/" + imdMapping, TimeoutMs: 30000},
		},
	}}

	step, closeStep, err := WeatherObservation.New(context.Background(), registry, mapper,
		&WeatherObservation.Config{
			BindingKeys: []string{imdBindingKey},
			AuthByProvider: map[string]*common.AuthProfile{
				"imd": {Scheme: util.AuthSchemeNone},
			},
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

	var answer map[string]any
	if err := json.Unmarshal(stepCtx.ResponseBody, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}

	commitment := firstCommitment(t, answer)

	// The failure this exists to catch: the mapping accepts an answer it cannot
	// read and returns a valid, signable on_select with nothing in it. That is
	// what pointing at the wrong IMD endpoint looks like.
	resources, _ := commitment["resources"].([]any)
	if len(resources) == 0 {
		t.Fatal("the mock and the mapping do not fit: the answer carries no resources")
	}

	// Dates are DERIVED -- the mock gives one Date and names the rest by number,
	// as IMD does. Consecutive days are the only proof the derivation is right;
	// a wrong one delivers a forecast for the wrong day, which nothing
	// downstream could detect.
	previous := ""
	for i, entry := range resources {
		resource, _ := entry.(map[string]any)
		attributes, _ := resource["resourceAttributes"].(map[string]any)
		validity, _ := attributes["validity"].(map[string]any)
		startsAt, _ := validity["startsAt"].(string)
		if startsAt == "" {
			t.Fatalf("resource %d carries no validity", i)
		}
		if previous != "" && startsAt <= previous {
			t.Errorf("resource %d is dated %q, which does not follow %q", i, startsAt, previous)
		}
		previous = startsAt

		// Rainfall and humidity are reported once for the station, so they
		// belong to day 1 alone. A later day carrying them means the mapping
		// repeated them, which is inventing data.
		if i == 0 {
			continue
		}
		parameters, _ := attributes["parameters"].([]any)
		for _, p := range parameters {
			parameter, _ := p.(map[string]any)
			switch parameter["parameter"] {
			case "Rainfall", "Humidity":
				t.Errorf("resource %d carries %v; the mock reports it once for the station",
					i, parameter["parameter"])
			}
		}
	}

	t.Logf("%d resources, %s to %s", len(resources),
		firstDate(t, resources), previous)
}

// firstDate reads the first resource's start date, for the log line above.
func firstDate(t *testing.T, resources []any) string {
	t.Helper()
	first, _ := resources[0].(map[string]any)
	attributes, _ := first["resourceAttributes"].(map[string]any)
	validity, _ := attributes["validity"].(map[string]any)
	startsAt, _ := validity["startsAt"].(string)
	return startsAt
}
