package implementation

// publishpipelines_test.go proves the embed carries what the registry names,
// and runs each real pipeline that needs no upstream through the frame, so a
// folder added without Go is still held to its contract here.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

const (
	mandiRegistryPath   = "pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml"
	weatherRegistryPath = "pkg/plugin/implementation/WeatherObservation/cataloguepublish-mausangram/pipeline.yaml"
)

// Every embedded pipeline loads against the contract and drops no key. This is
// the check a folder-only capability gets instead of a package of its own.
func TestEveryEmbeddedPipelineLoads(t *testing.T) {
	paths := PublishPipelines()
	for _, want := range []string{mandiRegistryPath, weatherRegistryPath} {
		found := false
		for _, got := range paths {
			found = found || got == want
		}
		if !found {
			t.Errorf("PublishPipelines() = %v, missing %s", paths, want)
		}
	}

	for _, registryPath := range paths {
		t.Run(registryPath, func(t *testing.T) {
			files, err := PublishPipeline(registryPath)
			if err != nil {
				t.Fatalf("PublishPipeline: %v", err)
			}
			spec, err := pipeline.LoadSpec(files.FS, files.Path)
			if err != nil {
				t.Fatalf("LoadSpec: %v", err)
			}
			if spec.Metadata.Capability == "" {
				t.Error("metadata.capability is empty")
			}
			missing, err := pipeline.UnmappedKeys(files.FS, files.Path)
			if err != nil {
				t.Fatalf("UnmappedKeys: %v", err)
			}
			if len(missing) != 0 {
				t.Errorf("keys the file declares that nothing reads: %v", missing)
			}
		})
	}
}

// A path the binary does not embed is refused by name, never substituted.
func TestPublishPipelineRefusesWhatIsNotEmbedded(t *testing.T) {
	for _, bad := range []string{
		"pkg/plugin/implementation/Nope/cataloguepublish-x/pipeline.yaml",
		"somewhere/else/pipeline.yaml",
		"",
	} {
		if _, err := PublishPipeline(bad); err == nil {
			t.Errorf("PublishPipeline(%q) succeeded; want a refusal", bad)
		}
	}
}

// wantWeatherCatalogue is the agreed target payload. {{providerId}} and
// {{networkId}} are what the inputs fill; the context ids and timestamp are
// generated per run.
const wantWeatherCatalogue = `{
  "context": {
    "action": "catalog/publish",
    "version": "2.0.0",
    "transactionId": "b1f0c2d3-4e5a-4b6c-8d9e-0f1a2b3c4d5e",
    "messageId": "c2e1d3f4-5a6b-4c7d-9e0f-1a2b3c4d5e6f",
    "timestamp": "2026-09-01T06:00:00Z"
  },
  "message": {
    "catalogs": [
      {
        "id": "cat-mausamgram-point-forecast-v2",
        "isActive": true,
        "descriptor": {
          "code": "IMD-NWP-01",
          "name": "IMD Mausamgram point weather forecast",
          "shortDesc": "Five-day point weather forecast from IMD Mausamgram NWP",
          "longDesc": "Rainfall, temperature, humidity and wind forecast for a single point, five days ahead, from the India Meteorological Department's Mausamgram numerical weather prediction service."
        },
        "provider": {
          "id": "{{providerId}}",
          "descriptor": { "code": "IMD-NWP-01", "name": "IMD Mausamgram NWP" }
        },
        "validity": { "startDate": "2026-01-01T00:00:00Z", "endDate": "2027-12-31T23:59:59Z" },
        "resources": [
          {
            "id": "res:mausamgram:point-forecast",
            "descriptor": {
              "code": "WX-POINT-FORECAST",
              "name": "Point weather forecast",
              "shortDesc": "Five-day weather forecast for a single point",
              "longDesc": "Daily rainfall, minimum and maximum temperature, minimum and maximum humidity and wind speed for a requested latitude and longitude."
            },
            "resourceAttributes": {
              "@context": "https://raw.githubusercontent.com/OpenAgriNet/network-specs/schema-packs-v0.1/schema/WeatherObservation/v0.1/context.jsonld",
              "@type": "openagrinet:WeatherObservation",
              "informationMode": "OnDemand",
              "subjectCategories": ["Weather"],
              "supportedObservationTypes": ["Forecast"],
              "supportedParameters": ["Rainfall", "Temperature", "Humidity", "WindSpeed", "WindDirection", "Alert"],
              "forecastHorizon": "P5D",
              "updateFrequency": "PT24H",
              "geographicGranularities": ["Point"],
              "languages": ["en"],
              "coverageAreas": [
                { "codeScheme": "ISO-3166-1", "areaCode": "IN", "areaLevel": "Country", "areaName": "India" },
                { "type": "Polygon", "coordinates": [[[68.0, 6.0], [98.0, 6.0], [98.0, 38.0], [68.0, 38.0], [68.0, 6.0]]] }
              ]
            }
          }
        ]
      }
    ],
    "publishDirectives": [
      {
        "catalogId": "cat-mausamgram-point-forecast-v2",
        "catalogType": "REGULAR",
        "updateMode": "MERGE",
        "visibleTo": ["{{networkId}}"]
      }
    ]
  }
}`

const (
	weatherProviderID = "mausamgram-test"
	weatherNetworkID  = "oan-test"
)

func weatherRecord() *model.ProviderRecord {
	return &model.ProviderRecord{
		BindingKey: weatherProviderID + "|openagrinet:WeatherObservation",
		Actions:    map[string]model.ActionPlan{"publish": {Mappings: weatherRegistryPath}},
	}
}

func weatherEnv(publishURL string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch name {
		case "WEATHER_PARTICIPANT_ID":
			return weatherProviderID, true
		case "APP_NETWORK_ID":
			return weatherNetworkID, true
		case "CATALOG_PUBLISH_URL":
			return publishURL, publishURL != ""
		}
		return "", false
	}
}

// weatherDue is just after the file's cron (01:00 IST), so the run is due.
func weatherDue(t *testing.T) time.Time {
	t.Helper()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return time.Date(2026, 9, 24, 1, 5, 0, 0, ist)
}

// The weather pipeline declares no upstream: it must run with NO credentials
// in the environment, and what it renders is the target payload field for
// field once the per-run values are set aside.
func TestWeatherPipelineRendersTheMausamgramCatalogue(t *testing.T) {
	files, err := PublishPipeline(weatherRegistryPath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: files,
		Record:   weatherRecord(),
		Lookup:   weatherEnv(""),
		Now:      weatherDue(t),
		OutDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Catalogues) != 1 {
		t.Fatalf("got %d catalogues, want 1", len(report.Catalogues))
	}
	got := report.Catalogues[0]
	if got.CatalogID != "cat-mausamgram-point-forecast-v2" {
		t.Errorf("CatalogID = %q, want cat-mausamgram-point-forecast-v2", got.CatalogID)
	}

	var gotDoc, wantDoc map[string]any
	if err := json.Unmarshal(got.Content, &gotDoc); err != nil {
		t.Fatalf("decode rendered: %v\n%s", err, got.Content)
	}
	if err := json.Unmarshal([]byte(wantWeatherCatalogue), &wantDoc); err != nil {
		t.Fatalf("decode want: %v", err)
	}

	gotCtx := gotDoc["context"].(map[string]any)
	for _, field := range []string{"transactionId", "messageId"} {
		if id, _ := gotCtx[field].(string); len(id) != 36 {
			t.Errorf("context.%s = %v, want a generated UUID", field, gotCtx[field])
		}
	}
	if ts, _ := gotCtx["timestamp"].(string); ts == "" {
		t.Error("context.timestamp is empty")
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("context.timestamp %q is not RFC3339: %v", ts, err)
	}
	wantCtx := wantDoc["context"].(map[string]any)
	for _, field := range []string{"transactionId", "messageId", "timestamp"} {
		wantCtx[field] = gotCtx[field]
	}
	wantMsg := wantDoc["message"].(map[string]any)
	wantMsg["catalogs"].([]any)[0].(map[string]any)["provider"].(map[string]any)["id"] = weatherProviderID
	wantMsg["publishDirectives"].([]any)[0].(map[string]any)["visibleTo"] = []any{weatherNetworkID}

	if !reflect.DeepEqual(gotDoc, wantDoc) {
		gotJSON, _ := json.MarshalIndent(gotDoc, "", "  ")
		t.Errorf("rendered catalogue differs from the target payload:\n%s", gotJSON)
	}
}

// Publishing sends the one catalogue to the adapter's /publish and reads
// ACCEPTED as success.
func TestWeatherPipelinePublishes(t *testing.T) {
	var calls atomic.Int32
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/publish" {
			t.Errorf("posted to %q, want /publish", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"context": map[string]any{"action": "catalog/on_publish"},
			"message": map[string]any{"results": []any{
				map[string]any{"catalogId": "cat-mausamgram-point-forecast-v2", "status": "ACCEPTED"},
			}},
		})
	}))
	defer adapter.Close()

	files, err := PublishPipeline(weatherRegistryPath)
	if err != nil {
		t.Fatalf("PublishPipeline: %v", err)
	}
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: files,
		Record:   weatherRecord(),
		Lookup:   weatherEnv(adapter.URL),
		Now:      weatherDue(t),
		OutDir:   t.TempDir(),
		Publish:  true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("adapter called %d times, want 1", calls.Load())
	}
	if report.Published == nil || report.Published.HasFailures() {
		t.Errorf("Published = %+v, want one ACCEPTED outcome", report.Published)
	}
	if !strings.HasSuffix(report.PipelinePath, "cataloguepublish-mausangram/pipeline.yaml") {
		t.Errorf("PipelinePath = %q", report.PipelinePath)
	}
}
