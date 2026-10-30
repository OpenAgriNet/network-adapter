package publish

// golden_test.go pins the EXACT bytes this pipeline produces.
//
// Every other test here asserts a property -- two catalogs, this many
// resources, that field present. A golden file asserts the whole document, so
// a change nobody described shows up as a diff rather than as a test that
// still passes because it never looked at the field that moved.
//
// It exists to make the risky changes safe to check: renaming the package and
// the file, deleting fields the engine does not read, rewriting the mappings.
// Any of those can alter the output silently. After this, none can.
//
// Regenerate deliberately, never reflexively:
//
//	go test ./pkg/plugin/implementation/MandiPrice/publish/ -run Golden -update
//
// and READ the diff. A golden file updated without reading it is worse than
// no golden file, because it reads as review.

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

var update = flag.Bool("update", false, "rewrite the golden file from this run's output")

// The goldens are built from the demo sample's own markets (sampleFixture below),
// in the shape the YAML mappings produce. Edge cases -- a market with no
// coordinates, a second state, repeated price rows, an unreported price -- are
// asserted directly in run_execute_test.go.
const (
	goldenSampleOnDemandPath = "testdata/golden/catalog-ondemand.json"
	goldenSampleDirectPath   = "testdata/golden/catalog-direct.json"
)

// renderGolden turns a report into the stable document the golden file holds.
//
// Order is imposed rather than assumed: grouping walks a map, so the slugs
// come back in whatever order Go felt like. A golden file that re-sorted
// itself every run would fail for a reason that is not a regression.
func renderGolden(t *testing.T, report pipeline.RunReport) []byte {
	t.Helper()
	return renderGoldenWith(t, report, volatile)
}

// renderGoldenWith is renderGolden with its own set of volatile fields.
func renderGoldenWith(t *testing.T, report pipeline.RunReport, keys map[string]string) []byte {
	t.Helper()

	type entry struct {
		Slug      string          `json:"slug"`
		CatalogID string          `json:"catalogId"`
		Content   json.RawMessage `json:"content"`
	}

	entries := make([]entry, 0, len(report.Catalogs))
	for _, catalog := range report.Catalogs {
		// Re-encoded through a generic decode so the golden file is
		// canonically formatted and key order is stable, rather than carrying
		// whatever spacing the renderer emitted.
		var document any
		if err := json.Unmarshal(catalog.Content, &document); err != nil {
			t.Fatalf("catalog %s is not JSON: %v", catalog.Slug, err)
		}
		stabiliseKeys(document, keys)
		canonical, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("re-encoding catalog %s: %v", catalog.Slug, err)
		}
		entries = append(entries, entry{
			Slug:      catalog.Slug,
			CatalogID: catalog.CatalogID,
			Content:   canonical,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })

	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("encoding the golden document: %v", err)
	}
	return append(out, '\n')
}

// volatile names the fields that differ on every run for reasons that are not
// regressions: two freshly-minted UUIDs and the wall clock.
//
// They are REPLACED, not deleted, so the golden file still fails if one stops
// being emitted -- which would be a real change to the envelope, and exactly
// the kind this file exists to catch.
var volatile = map[string]string{
	"transactionId": "<uuid>",
	"messageId":     "<uuid>",
	"timestamp":     "<timestamp>",
}

// stabilise walks a decoded document and substitutes the volatile fields in
// place, at any depth: the envelope has them at the top and a resource could
// grow its own.
func stabilise(node any) { stabiliseKeys(node, volatile) }

// stabiliseKeys is stabilise over a given set of volatile fields.
func stabiliseKeys(node any, keys map[string]string) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if placeholder, isVolatile := keys[key]; isVolatile {
				if _, isString := value.(string); isString {
					typed[key] = placeholder
					continue
				}
			}
			stabiliseKeys(value, keys)
		}
	case []any:
		for _, item := range typed {
			stabiliseKeys(item, keys)
		}
	}
}

// directVolatile adds the Direct resources' wall-clock fields: a price is
// published with the moment it was generated and is valid for a day from it.
var directVolatile = map[string]string{
	"transactionId": "<uuid>",
	"messageId":     "<uuid>",
	"timestamp":     "<timestamp>",
	"generatedAt":   "<timestamp>",
	"startsAt":      "<timestamp>",
	"endsAt":        "<timestamp + 2 days>",
	"startDate":     "<timestamp>",
	"endDate":       "<timestamp + 2 days>",
}

// compareGolden writes the golden under -update, and otherwise compares byte for
// byte.
func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating the golden directory: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		t.Logf("golden file rewritten: %s -- read the diff before committing it", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (run with -update to create it): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("the output no longer matches %s.\n\n--- want ---\n%s\n\n--- got ---\n%s", path, want, got)
	}
}

// The sample goldens: the demo sample's markets, end to end through the YAML
// mappings, byte for byte. Regenerate with -update, and read the diff.
func TestGoldenSampleOnDemandCatalog(t *testing.T) {
	upstream := fakeAgmarknetWith(t, sampleStates(), sampleFixture)
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: Pipeline(), Record: publishingRecord(),
		Lookup: modeEnv(upstream.URL, "onDemand"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Catalogs) != 1 {
		t.Fatalf("the sample run produced %d catalogs, want the one OnDemand MH catalog", len(report.Catalogs))
	}
	compareGolden(t, goldenSampleOnDemandPath, renderGolden(t, report))
}

func TestGoldenSampleDirectCatalog(t *testing.T) {
	upstream := fakeAgmarknetWith(t, sampleStates(), sampleFixture)
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: Pipeline(), Record: publishingRecord(),
		Lookup: modeEnv(upstream.URL, "direct"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Catalogs) != 1 {
		t.Fatalf("the sample run produced %d catalogs, want the one Direct MH catalog", len(report.Catalogs))
	}
	compareGolden(t, goldenSampleDirectPath, renderGoldenWith(t, report, directVolatile))
}

// sample_fixture_test.go is the fake upstream's data for the sample goldens
// (testdata/golden/catalog-ondemand.json and catalog-direct.json). It is the
// demo sample's own data -- the four Ahmednagar markets of
// capability-examples/MandiPrice/publish/ondemand-mh.json and direct-mh.json:
// their names, districts, coordinates, commodities and onion prices -- in the
// upstream's raw shapes, so the goldens read like the samples and differ from
// them only where the YAML mappings decide.
//
// Market ids are the upstream's market_id (1284 for Pathardi). The demo
// samples' marketCode (1432) is the upstream's agm_market_center_code, the
// other number master-markets.yaml warns against; the pipeline publishes
// market_id.
//
// The edge cases are asserted directly in run_execute_test.go.

var sampleFixture = upstreamFixture{
	master:    sampleMasterMarketsBody,
	prices:    samplePricesByPair,
	districts: map[string]string{"1284": "338", "1634": "338", "1649": "338", "1894": "338"},
}

func sampleStates() []upstreamState {
	return []upstreamState{{code: "MH", name: "Maharashtra", rows: sampleRowsMH}}
}

const sampleRowsMH = `[
  {
    "market_id": 1284,
    "mkt_name": "Pathardi APMC",
    "state_code": "MH",
    "state_name": "Maharashtra",
    "district_id": 338,
    "district_name": "Ahmednagar",
    "cmdt_details": [
      {
        "cmdt_id": 5,
        "cmdt_name": "Jowar(Sorghum)"
      },
      {
        "cmdt_id": 23,
        "cmdt_name": "Onion"
      },
      {
        "cmdt_id": 28,
        "cmdt_name": "Bajra(Pearl Millet/Cumbu)"
      }
    ]
  },
  {
    "market_id": 1634,
    "mkt_name": "Rahuri APMC",
    "state_code": "MH",
    "state_name": "Maharashtra",
    "district_id": 338,
    "district_name": "Ahmednagar",
    "cmdt_details": [
      {
        "cmdt_id": 4,
        "cmdt_name": "Maize"
      },
      {
        "cmdt_id": 13,
        "cmdt_name": "Soyabean"
      },
      {
        "cmdt_id": 18,
        "cmdt_name": "Orange"
      },
      {
        "cmdt_id": 20,
        "cmdt_name": "Mango"
      },
      {
        "cmdt_id": 23,
        "cmdt_name": "Onion"
      },
      {
        "cmdt_id": 24,
        "cmdt_name": "Potato"
      },
      {
        "cmdt_id": 25,
        "cmdt_name": "Garlic"
      },
      {
        "cmdt_id": 28,
        "cmdt_name": "Bajra(Pearl Millet/Cumbu)"
      },
      {
        "cmdt_id": 31,
        "cmdt_name": "Cauliflower"
      },
      {
        "cmdt_id": 32,
        "cmdt_name": "Brinjal"
      },
      {
        "cmdt_id": 45,
        "cmdt_name": "Arhar(Tur/Red Gram)(Whole)"
      },
      {
        "cmdt_id": 46,
        "cmdt_name": "Green Peas"
      },
      {
        "cmdt_id": 59,
        "cmdt_name": "Papaya"
      },
      {
        "cmdt_id": 62,
        "cmdt_name": "Guar"
      },
      {
        "cmdt_id": 64,
        "cmdt_name": "Mousambi(Sweet Lime)"
      },
      {
        "cmdt_id": 65,
        "cmdt_name": "Tomato"
      },
      {
        "cmdt_id": 67,
        "cmdt_name": "Bitter gourd"
      },
      {
        "cmdt_id": 68,
        "cmdt_name": "Bottle gourd"
      },
      {
        "cmdt_id": 71,
        "cmdt_name": "Bhindi(Ladies Finger)"
      },
      {
        "cmdt_id": 73,
        "cmdt_name": "Green Chilli"
      },
      {
        "cmdt_id": 74,
        "cmdt_name": "Chilly Capsicum"
      },
      {
        "cmdt_id": 87,
        "cmdt_name": "Ginger(Green)"
      },
      {
        "cmdt_id": 124,
        "cmdt_name": "Sweet Potato"
      },
      {
        "cmdt_id": 126,
        "cmdt_name": "Cabbage"
      },
      {
        "cmdt_id": 131,
        "cmdt_name": "Cucumbar(Kheera)"
      },
      {
        "cmdt_id": 132,
        "cmdt_name": "Ridgeguard(Tori)"
      },
      {
        "cmdt_id": 140,
        "cmdt_name": "Drumstick"
      },
      {
        "cmdt_id": 145,
        "cmdt_name": "Sweet Pumpkin"
      },
      {
        "cmdt_id": 151,
        "cmdt_name": "Lime"
      },
      {
        "cmdt_id": 156,
        "cmdt_name": "Guava"
      },
      {
        "cmdt_id": 160,
        "cmdt_name": "Pomegranate"
      },
      {
        "cmdt_id": 162,
        "cmdt_name": "Seetapal"
      }
    ]
  },
  {
    "market_id": 1649,
    "mkt_name": "Newasa APMC",
    "state_code": "MH",
    "state_name": "Maharashtra",
    "district_id": 338,
    "district_name": "Ahmednagar",
    "cmdt_details": [
      {
        "cmdt_id": 23,
        "cmdt_name": "Onion"
      }
    ]
  },
  {
    "market_id": 1894,
    "mkt_name": "Parner APMC",
    "state_code": "MH",
    "state_name": "Maharashtra",
    "district_id": 338,
    "district_name": "Ahmednagar",
    "cmdt_details": [
      {
        "cmdt_id": 23,
        "cmdt_name": "Onion"
      }
    ]
  }
]`

const sampleMasterMarketsBody = `[
  {
    "market_id": 1284,
    "market_name": "Pathardi APMC",
    "state_name": "Maharashtra",
    "district_name": "Ahmednagar",
    "market_latitude": "19.17442009",
    "market_longitude": "75.17606191"
  },
  {
    "market_id": 1634,
    "market_name": "Rahuri APMC",
    "state_name": "Maharashtra",
    "district_name": "Ahmednagar",
    "market_latitude": "19.644527591487904",
    "market_longitude": "74.66192086448265"
  },
  {
    "market_id": 1649,
    "market_name": "Newasa APMC",
    "state_name": "Maharashtra",
    "district_name": "Ahmednagar",
    "market_latitude": "19.54478514",
    "market_longitude": "74.93534498"
  },
  {
    "market_id": 1894,
    "market_name": "Parner APMC",
    "state_name": "Maharashtra",
    "district_name": "Ahmednagar",
    "market_latitude": "18.99974287",
    "market_longitude": "74.44058799"
  }
]`

var samplePricesByPair = map[string]string{
	"1284|23": `[
  {
    "Grade": "Local",
    "Group": "Vegetables",
    "State": "Maharashtra",
    "Market": "Pathardi APMC",
    "Variety": "Red",
    "District": "Ahmednagar",
    "Commodity": "Onion",
    "Max Price": "5200",
    "Min Price": "1000",
    "Price Unit": "Rs./Qtl",
    "Modal Price": "3100",
    "Arrival Date": "06-09-2026"
  }
]`,
	"1634|23": `[
  {
    "Grade": "Local",
    "Group": "Vegetables",
    "State": "Maharashtra",
    "Market": "Rahuri APMC",
    "Variety": "Other",
    "District": "Ahmednagar",
    "Commodity": "Onion",
    "Max Price": "6100",
    "Min Price": "1000",
    "Price Unit": "Rs./Qtl",
    "Modal Price": "3100",
    "Arrival Date": "06-09-2026"
  }
]`,
	"1649|23": `[
  {
    "Grade": "Local",
    "Group": "Vegetables",
    "State": "Maharashtra",
    "Market": "Newasa APMC",
    "Variety": "Unhali",
    "District": "Ahmednagar",
    "Commodity": "Onion",
    "Max Price": "5100",
    "Min Price": "1500",
    "Price Unit": "Rs./Qtl",
    "Modal Price": "4200",
    "Arrival Date": "06-09-2026"
  }
]`,
	"1894|23": `[
  {
    "Grade": "Local",
    "Group": "Vegetables",
    "State": "Maharashtra",
    "Market": "Parner APMC",
    "Variety": "Unhali",
    "District": "Ahmednagar",
    "Commodity": "Onion",
    "Max Price": "5600",
    "Min Price": "1000",
    "Price Unit": "Rs./Qtl",
    "Modal Price": "4000",
    "Arrival Date": "06-09-2026"
  }
]`,
}
