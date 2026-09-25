package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// echoMapper stands in for jsonmapper: it hands back whatever the builder
// passed it, so a test can assert on the INPUT the render mapping would see
// without a mapping file existing.
type echoMapper struct {
	calls int
	refs  []string
}

func (m *echoMapper) Verify(context.Context, string, any) error { return nil }

func (m *echoMapper) Transform(_ context.Context, ref string, d definition.Direction, in any) ([]byte, error) {
	m.calls++
	m.refs = append(m.refs, ref)
	return json.Marshal(map[string]any{"direction": string(d), "input": in})
}

// failingMapper reports what a broken mapping does, so a render failure is
// distinguishable from a build failure.
type failingMapper struct{}

func (failingMapper) Verify(context.Context, string, any) error { return nil }

func (failingMapper) Transform(context.Context, string, definition.Direction, any) ([]byte, error) {
	return nil, fmt.Errorf("mapping blew up")
}

func testBuildContext(t *testing.T, inputs map[string]string) (*runContext, *exprCache) {
	t.Helper()
	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	if inputs == nil {
		inputs = map[string]string{}
	}
	return newRunContext(inputs, "never-logged-token"), cache
}

// decodeEcho pulls the render input back out of what echoMapper returned.
func decodeEcho(t *testing.T, content []byte) (response []any, local map[string]any) {
	t.Helper()
	var echoed struct {
		Direction string `json:"direction"`
		Input     struct {
			Response []any          `json:"response"`
			Local    map[string]any `json:"_local"`
		} `json:"input"`
	}
	if err := json.Unmarshal(content, &echoed); err != nil {
		t.Fatalf("decoding rendered content: %v", err)
	}
	if echoed.Direction != string(definition.DirectionResponse) {
		t.Errorf("render ran in direction %q, want %q", echoed.Direction, definition.DirectionResponse)
	}
	return echoed.Input.Response, echoed.Input.Local
}

// zeroCommoditiesRule is "this record lists nothing", and it is NOT spelled
// `$count(commodities) = 0` the way mandi-price-agmarket.yaml spells it.
//
// Under this jsonata build that comparison is ALWAYS false: a field holding an
// empty array resolves to an empty sequence, and the 0 that $count returns for
// one does not compare equal to the literal 0 (`$count(x) < 1` over the same
// record fails outright with "the values 0 and 1 ... must be of the same data
// type"). $number() puts it back into the number the comparison expects.
//
// The builder is right either way -- it applies what the file says -- but the
// file's own rule silently excludes nothing, so no test here pretends
// otherwise.
const zeroCommoditiesRule = "$number($count(commodities)) = 0"

// simpleCatalog is the smallest catalog block that builds anything: group,
// name, render.
func simpleCatalog() Catalog {
	return Catalog{
		GroupBy:  "stateCode",
		Chunk:    Chunk{Slug: "${stateCode}"},
		Identity: Identity{CatalogID: "catalog:test:${slug}"},
		Render:   Render{Mapping: "mappings/catalog.yaml"},
	}
}

func TestBuildCataloguesGroupsByTheNamedField(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	mapper := &echoMapper{}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 2.0},
		{"stateCode": "KA", "marketId": 1.0},
		{"stateCode": "MH", "marketId": 3.0},
	}

	built, counters, err := buildCatalogues(context.Background(), simpleCatalog(), records, rc, cache, mapper, "http://mappings")
	if err != nil {
		t.Fatalf("buildCatalogues: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d catalogues, want 2", len(built))
	}
	// Groups are walked in sorted key order so two runs read the same way.
	if built[0].Slug != "KA" || built[1].Slug != "MH" {
		t.Errorf("slugs are %q/%q, want KA/MH", built[0].Slug, built[1].Slug)
	}
	if built[1].CatalogID != "catalog:test:MH" {
		t.Errorf("catalogId is %q", built[1].CatalogID)
	}
	if counters["groups"] != 2 || counters["catalogues"] != 2 {
		t.Errorf("counters = %v", counters)
	}

	response, _ := decodeEcho(t, built[1].Content)
	if len(response) != 2 {
		t.Fatalf("MH carries %d records, want 2", len(response))
	}
	if mapper.refs[0] != "http://mappings/catalog.yaml" {
		t.Errorf("mapping ref is %q, want the mappings/ prefix stripped", mapper.refs[0])
	}
}

func TestBuildCataloguesRefusesARecordMissingTheGroupField(t *testing.T) {
	rc, cache := testBuildContext(t, nil)

	records := []map[string]any{{"marketId": 1.0}}
	if _, _, err := buildCatalogues(context.Background(), simpleCatalog(), records, rc, cache, &echoMapper{}, "http://mappings"); err == nil {
		t.Fatal("a record with nothing to group by built a catalogue")
	}
}

func TestBuildCataloguesExclusions(t *testing.T) {
	commodityRule := ExcludeRule{
		// NOT `$count(commodities) = 0`: see the note on
		// zeroCommoditiesRule below.
		When:   zeroCommoditiesRule,
		Reason: "upstream reported no commodities trading in this window",
	}
	geometryRule := ExcludeRule{
		When:   "${inputs.withoutGeometry} = 'skip' and coordinateQuality != 'ok'",
		Reason: "coordinate ${coordinateQuality}",
	}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "commodities": []any{"wheat"}, "coordinateQuality": "ok"},
		{"stateCode": "MH", "marketId": 2.0, "commodities": []any{}, "coordinateQuality": "ok"},
		{"stateCode": "MH", "marketId": 3.0, "commodities": []any{"rice"}, "coordinateQuality": "missing"},
	}

	cases := []struct {
		name            string
		withoutGeometry string
		wantPublished   int
		wantCounters    map[string]int
	}{
		{
			// The geometry rule reads an input. Interpolated as bare text it
			// would read `publish = 'skip'` -- a comparison against a PATH
			// called publish, which is undefined, so the rule would never fire
			// either way and the skip option would silently do nothing.
			name:            "publish keeps the geometry-less market",
			withoutGeometry: "publish",
			wantPublished:   2,
			wantCounters: map[string]int{
				"excluded": 1,
				"excluded:upstream reported no commodities trading in this window": 1,
			},
		},
		{
			name:            "skip drops it, naming the verdict",
			withoutGeometry: "skip",
			wantPublished:   1,
			wantCounters: map[string]int{
				"excluded":                    2,
				"excluded:coordinate missing": 1,
				"excluded:upstream reported no commodities trading in this window": 1,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, map[string]string{"withoutGeometry": tc.withoutGeometry})
			catalog := simpleCatalog()
			catalog.Exclude = []ExcludeRule{commodityRule, geometryRule}

			built, counters, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
			if err != nil {
				t.Fatalf("buildCatalogues: %v", err)
			}
			if len(built) != 1 {
				t.Fatalf("built %d catalogues, want 1", len(built))
			}
			response, _ := decodeEcho(t, built[0].Content)
			if len(response) != tc.wantPublished {
				t.Fatalf("catalogue carries %d records, want %d", len(response), tc.wantPublished)
			}
			for key, want := range tc.wantCounters {
				if counters[key] != want {
					t.Errorf("counter %q = %d, want %d (all: %v)", key, counters[key], want, counters)
				}
			}
		})
	}
}

func TestBuildCataloguesAnnotatesWithoutExcluding(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Annotate = []AnnotateRule{{When: "coordinateQuality != 'ok'", As: "publishedWithoutLocation"}}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "coordinateQuality": "ok"},
		{"stateCode": "MH", "marketId": 2.0, "coordinateQuality": "suspect"},
	}

	built, counters, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
	if err != nil {
		t.Fatalf("buildCatalogues: %v", err)
	}
	response, _ := decodeEcho(t, built[0].Content)
	if len(response) != 2 {
		t.Fatalf("an annotated record was dropped: %d records", len(response))
	}
	first := response[0].(map[string]any)
	second := response[1].(map[string]any)
	if _, marked := first["publishedWithoutLocation"]; marked {
		t.Error("the market with an ok coordinate was annotated")
	}
	if second["publishedWithoutLocation"] != true {
		t.Errorf("the geometry-less market was not annotated: %v", second)
	}
	if counters["annotated:publishedWithoutLocation"] != 1 {
		t.Errorf("counters = %v", counters)
	}
	// The caller's records must not have grown a field behind its back.
	if _, leaked := records[1]["publishedWithoutLocation"]; leaked {
		t.Error("annotation wrote onto the caller's record")
	}
}

func TestBuildCataloguesOrders(t *testing.T) {
	records := []map[string]any{
		{"stateCode": "MH", "marketId": 10.0},
		{"stateCode": "MH", "marketId": 9.0},
		{"stateCode": "MH", "marketId": 100.0},
	}

	cases := []struct {
		name      string
		direction string
		want      []float64
	}{
		// 9 before 10 before 100: numeric keys must not order as text.
		{name: "asc", direction: "asc", want: []float64{9, 10, 100}},
		{name: "desc", direction: "desc", want: []float64{100, 10, 9}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, nil)
			catalog := simpleCatalog()
			catalog.Order = Order{By: "marketId", Direction: tc.direction}

			built, _, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
			if err != nil {
				t.Fatalf("buildCatalogues: %v", err)
			}
			response, _ := decodeEcho(t, built[0].Content)
			for i, want := range tc.want {
				if got := response[i].(map[string]any)["marketId"]; got != want {
					t.Errorf("position %d is %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestBuildCataloguesRefusesAnUnknownOrderDirection(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Order = Order{By: "marketId", Direction: "sideways"}

	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}
	if _, _, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings"); err == nil {
		t.Fatal("an unknown order direction was accepted")
	}
}

// TestChunkAtTheBudgetBoundary is the rule the geometry cap turns on: a chunk
// is cut only when adding the NEXT record would exceed the budget, so exactly
// budget-many costing records still make one catalogue.
func TestChunkAtTheBudgetBoundary(t *testing.T) {
	chunk := Chunk{
		Budget: 4,
		Cost:   "coordinateQuality = 'ok' ? 1 : 0",
		Slug:   "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}",
	}

	costing := func(n int) []map[string]any {
		records := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			records = append(records, map[string]any{
				"stateCode": "MH", "marketId": float64(i), "coordinateQuality": "ok",
			})
		}
		return records
	}

	cases := []struct {
		name      string
		records   []map[string]any
		wantSlugs []string
		wantSizes []int
	}{
		{name: "exactly the budget", records: costing(4), wantSlugs: []string{"MH"}, wantSizes: []int{4}},
		{name: "one over the budget", records: costing(5), wantSlugs: []string{"MH", "MH-2"}, wantSizes: []int{4, 1}},
		{name: "two full chunks", records: costing(8), wantSlugs: []string{"MH", "MH-2"}, wantSizes: []int{4, 4}},
		{
			// Free records never force a split, however many there are.
			name: "costless records all fit",
			records: []map[string]any{
				{"stateCode": "MH", "marketId": 1.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 2.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 3.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 4.0, "coordinateQuality": "missing"},
				{"stateCode": "MH", "marketId": 5.0, "coordinateQuality": "missing"},
			},
			wantSlugs: []string{"MH"},
			wantSizes: []int{5},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, nil)
			catalog := simpleCatalog()
			catalog.Chunk = chunk
			catalog.Order = Order{By: "marketId", Direction: "asc"}

			built, _, err := buildCatalogues(context.Background(), catalog, tc.records, rc, cache, &echoMapper{}, "http://mappings")
			if err != nil {
				t.Fatalf("buildCatalogues: %v", err)
			}
			if len(built) != len(tc.wantSlugs) {
				t.Fatalf("built %d catalogues, want %d", len(built), len(tc.wantSlugs))
			}
			for i, want := range tc.wantSlugs {
				if built[i].Slug != want {
					t.Errorf("chunk %d slug is %q, want %q", i, built[i].Slug, want)
				}
				if built[i].CatalogID != "catalog:test:"+want {
					t.Errorf("chunk %d catalogId is %q", i, built[i].CatalogID)
				}
				response, _ := decodeEcho(t, built[i].Content)
				if len(response) != tc.wantSizes[i] {
					t.Errorf("chunk %d carries %d records, want %d", i, len(response), tc.wantSizes[i])
				}
			}
		})
	}
}

func TestChunkRefusesASlugThatCannotDistinguishChunks(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Chunk = Chunk{Budget: 1, Cost: "1", Slug: "${stateCode}"}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0},
		{"stateCode": "MH", "marketId": 2.0},
	}
	_, _, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
	if err == nil {
		t.Fatal("two chunks published under one slug")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Errorf("error does not name the slug: %v", err)
	}
}

func TestChunkRefusesANonNumericCost(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Chunk = Chunk{Budget: 2, Cost: "'expensive'", Slug: "${stateCode}"}

	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}
	if _, _, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings"); err == nil {
		t.Fatal("a cost that is not a number was accepted")
	}
}

// TestBuildCataloguesEmptyGroupProducesNoCatalogue is the one rule that cannot
// be got wrong: an empty catalogue retires the group's resources from the
// network on the next MERGE.
func TestBuildCataloguesEmptyGroupProducesNoCatalogue(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()
	catalog.Exclude = []ExcludeRule{{When: zeroCommoditiesRule, Reason: "nothing traded"}}

	records := []map[string]any{
		{"stateCode": "MH", "marketId": 1.0, "commodities": []any{}},
		{"stateCode": "KA", "marketId": 2.0, "commodities": []any{"rice"}},
	}

	mapper := &echoMapper{}
	built, counters, err := buildCatalogues(context.Background(), catalog, records, rc, cache, mapper, "http://mappings")
	if err != nil {
		t.Fatalf("buildCatalogues: %v", err)
	}
	if len(built) != 1 || built[0].Slug != "KA" {
		t.Fatalf("built %+v, want only KA", built)
	}
	if mapper.calls != 1 {
		t.Errorf("the render mapping ran %d times, want 1", mapper.calls)
	}
	if counters["emptyGroups"] != 1 {
		t.Errorf("an emptied group was not counted: %v", counters)
	}
}

func TestBuildCataloguesBrokenExpressionsAreErrorsNotFalse(t *testing.T) {
	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0, "commodities": []any{"rice"}}}

	cases := []struct {
		name    string
		catalog func(Catalog) Catalog
	}{
		{
			name: "exclude",
			catalog: func(c Catalog) Catalog {
				c.Exclude = []ExcludeRule{{When: "$count(((", Reason: "broken"}}
				return c
			},
		},
		{
			name: "annotate",
			catalog: func(c Catalog) Catalog {
				c.Annotate = []AnnotateRule{{When: "$count(((", As: "broken"}}
				return c
			},
		},
		{
			name: "chunk cost",
			catalog: func(c Catalog) Catalog {
				c.Chunk = Chunk{Budget: 2, Cost: "$count(((", Slug: "${stateCode}"}
				return c
			},
		},
		{
			name: "an exclusion naming an input that was never declared",
			catalog: func(c Catalog) Catalog {
				c.Exclude = []ExcludeRule{{When: "${inputs.notDeclared} = 'skip'", Reason: "broken"}}
				return c
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, cache := testBuildContext(t, nil)
			_, _, err := buildCatalogues(context.Background(), tc.catalog(simpleCatalog()), records, rc, cache, &echoMapper{}, "http://mappings")
			if err == nil {
				t.Fatal("a broken expression was read as false instead of failing the build")
			}
		})
	}
}

func TestBuildCataloguesRendersLocalsAndIdentities(t *testing.T) {
	rc, cache := testBuildContext(t, map[string]string{
		"participantId": "agmarknet",
		"networkId":     "oan-dev",
	})
	catalog := simpleCatalog()
	catalog.Identity.ResourceID = "resource:test:market:${marketId}"
	catalog.Render.Local = map[string]string{
		"participantId": "${inputs.participantId}",
		"networkId":     "${inputs.networkId}",
		"catalogSlug":   "${slug}",
		"stateName":     "${group.stateName}",
	}

	records := []map[string]any{
		// The first row's stateName is blank, so a group value taken blindly
		// from the first record would name the catalogue after nothing.
		{"stateCode": "MH", "marketId": 1.0, "stateName": ""},
		{"stateCode": "MH", "marketId": 2.0, "stateName": "Maharashtra"},
	}

	built, _, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
	if err != nil {
		t.Fatalf("buildCatalogues: %v", err)
	}
	response, local := decodeEcho(t, built[0].Content)
	want := map[string]any{
		"participantId": "agmarknet",
		"networkId":     "oan-dev",
		"catalogSlug":   "MH",
		"stateName":     "Maharashtra",
	}
	for key, value := range want {
		if local[key] != value {
			t.Errorf("_local[%q] = %v, want %v", key, local[key], value)
		}
	}
	if got := response[0].(map[string]any)["resourceId"]; got != "resource:test:market:1" {
		t.Errorf("resourceId is %v", got)
	}
}

func TestBuildCataloguesNeverCarriesTheToken(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	catalog := simpleCatalog()

	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}
	built, _, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
	if err != nil {
		t.Fatalf("buildCatalogues: %v", err)
	}
	if strings.Contains(string(built[0].Content), "never-logged-token") {
		t.Error("the rendered catalogue carries the upstream token")
	}
}

func TestBuildCataloguesReportsARenderFailure(t *testing.T) {
	rc, cache := testBuildContext(t, nil)
	records := []map[string]any{{"stateCode": "MH", "marketId": 1.0}}

	if _, _, err := buildCatalogues(context.Background(), simpleCatalog(), records, rc, cache, failingMapper{}, "http://mappings"); err == nil {
		t.Fatal("a failing render mapping built a catalogue")
	}
}

// TestBuildCataloguesFromADeclaredCatalogBlock runs the mandi file's OWN
// catalog block, parsed as YAML, over synthetic records: the point of this
// builder is that a capability adds one by writing YAML and no Go.
func TestBuildCataloguesFromADeclaredCatalogBlock(t *testing.T) {
	const block = `
groupBy: stateCode

exclude:
  - when: "$number($count(commodities)) = 0"
    reason: "upstream reported no commodities trading in this window"
  - when: "${inputs.withoutGeometry} = 'skip' and coordinateQuality != 'ok'"
    reason: "coordinate ${coordinateQuality}"

annotate:
  - when: "coordinateQuality != 'ok'"
    as: publishedWithoutLocation

order: { by: marketId, direction: asc }

chunk:
  budget: 2
  of: geometries
  cost: "coordinateQuality = 'ok' and $exists(latitude) and $exists(longitude) ? 1 : 0"
  slug: "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}"

identity:
  catalogId: catalog:mandi-price:${slug}
  resourceId: resource:mandi-price:market:${marketId}

render:
  mapping: mappings/catalog.yaml
  local:
    participantId: ${inputs.participantId}
    networkId: ${inputs.networkId}
    catalogSlug: ${slug}
    stateName: ${group.stateName}
    windowFrom: ${inputs.fromDate}
    windowTo: ${inputs.toDate}
    generatedAt: ${now.rfc3339}
    transactionId: ${uuid}
    messageId: ${uuid}
    publishWithoutGeometry: ${inputs.withoutGeometry}

output:
  dir: ${inputs.catalogOut}
  file: mandi-${slug}.json
  filenamePrefix: mandi
`

	var catalog Catalog
	if err := yaml.Unmarshal([]byte(block), &catalog); err != nil {
		t.Fatalf("parsing the catalog block: %v", err)
	}

	rc, cache := testBuildContext(t, map[string]string{
		"participantId":   "agmarknet",
		"networkId":       "oan-dev",
		"fromDate":        "23-09-2026",
		"toDate":          "23-09-2026",
		"catalogOut":      "catalog",
		"withoutGeometry": "publish",
	})

	market := func(state string, id float64, quality string, commodities int, located bool) map[string]any {
		record := map[string]any{
			"stateCode": state, "stateName": state + " state", "marketId": id,
			"coordinateQuality": quality,
		}
		list := make([]any, 0, commodities)
		for i := 0; i < commodities; i++ {
			list = append(list, map[string]any{"code": float64(i), "name": "crop"})
		}
		record["commodities"] = list
		if located {
			record["latitude"] = 19.1
			record["longitude"] = 72.8
		}
		return record
	}

	records := []map[string]any{
		market("MH", 3, "ok", 1, true),
		market("MH", 1, "ok", 1, true),
		market("MH", 2, "ok", 1, true),
		market("MH", 4, "missing", 1, false),
		market("MH", 5, "ok", 0, true),
		market("KA", 9, "suspect", 2, false),
	}

	built, counters, err := buildCatalogues(context.Background(), catalog, records, rc, cache, &echoMapper{}, "http://mappings")
	if err != nil {
		t.Fatalf("buildCatalogues: %v", err)
	}

	// KA first, then MH split at the two-geometry budget: markets 1 and 2
	// carry a geometry each, 3 opens the second chunk, and 4 (no geometry)
	// rides along for free.
	wantSlugs := []string{"KA", "MH", "MH-2"}
	if len(built) != len(wantSlugs) {
		t.Fatalf("built %d catalogues, want %d", len(built), len(wantSlugs))
	}
	for i, want := range wantSlugs {
		if built[i].Slug != want {
			t.Fatalf("catalogue %d is %q, want %q", i, built[i].Slug, want)
		}
		if built[i].CatalogID != "catalog:mandi-price:"+want {
			t.Errorf("catalogue %d id is %q", i, built[i].CatalogID)
		}
	}

	first, local := decodeEcho(t, built[1].Content)
	if len(first) != 2 {
		t.Errorf("MH's first chunk carries %d records, want 2", len(first))
	}
	if local["stateName"] != "MH state" || local["publishWithoutGeometry"] != "publish" {
		t.Errorf("_local = %v", local)
	}
	if local["transactionId"] == "" || local["generatedAt"] == "" {
		t.Errorf("built-ins did not resolve: %v", local)
	}

	second, _ := decodeEcho(t, built[2].Content)
	if len(second) != 2 {
		t.Errorf("MH's second chunk carries %d records, want 2", len(second))
	}
	if second[1].(map[string]any)["publishedWithoutLocation"] != true {
		t.Errorf("the geometry-less market was not annotated: %v", second[1])
	}

	if counters["excluded:upstream reported no commodities trading in this window"] != 1 {
		t.Errorf("counters = %v", counters)
	}
	if counters["annotated:publishedWithoutLocation"] != 2 {
		t.Errorf("counters = %v", counters)
	}
}

func TestWriteCataloguesWritesOneFilePerSlug(t *testing.T) {
	built := []Catalogue{
		{Slug: "MH", CatalogID: "catalog:example:MH", Content: []byte(`{"context":{"a":1}}`)},
		{Slug: "MH-2", CatalogID: "catalog:example:MH-2", Content: []byte(`{"context":{"a":2}}`)},
		{Slug: "KA", CatalogID: "catalog:example:KA", Content: []byte(`{"context":{"a":3}}`)},
	}

	// A directory that does not exist yet: WriteCatalogues is what a fresh run
	// relies on to create its output directory.
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := WriteCatalogues(built, dir, "example"); err != nil {
		t.Fatalf("WriteCatalogues: %v", err)
	}

	for _, name := range []string{"example-KA.json", "example-MH.json", "example-MH-2.json"} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(content, &doc); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
		// Pretty-printed for the human who reviews these files before a
		// publish, which is the only reason they hit disk at all.
		if !bytes.Contains(content, []byte("\n  \"context\"")) {
			t.Errorf("%s is not indented: %s", name, content)
		}
	}

	// And nothing else: a stale file from an earlier slug would be published
	// as though it were current.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("wrote %d files, want 3", len(entries))
	}
}

func TestWriteCataloguesRemovesStaleCataloguesFromAnEarlierRun(t *testing.T) {
	dir := t.TempDir()

	// Monday: a split state left two catalogues behind.
	stale := filepath.Join(dir, "example-MH-2.json")
	if err := os.WriteFile(stale, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("seed stale catalogue: %v", err)
	}
	// Something that is not ours, in the same directory. It must survive:
	// this directory is an operator's to point wherever they like.
	bystander := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(bystander, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed bystander: %v", err)
	}
	// Another pipeline's catalogue, under its own prefix. Also must survive --
	// this is the sweep half of the two-pipelines-one-directory corruption.
	neighbour := filepath.Join(dir, "weather-KA.json")
	if err := os.WriteFile(neighbour, []byte(`{"someone else":true}`), 0o644); err != nil {
		t.Fatalf("seed neighbour: %v", err)
	}

	// Tuesday: the state fits in one catalogue.
	built := []Catalogue{{
		Slug:      "MH",
		CatalogID: "catalog:example:MH",
		Content:   []byte(`{"current":true}`),
	}}
	if err := WriteCatalogues(built, dir, "example"); err != nil {
		t.Fatalf("WriteCatalogues: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("yesterday's catalogue survived; the publish step would post it again as today's")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("a file that was not ours was deleted: %v", err)
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Errorf("another pipeline's catalogue was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "example-MH.json")); err != nil {
		t.Errorf("today's catalogue was not written: %v", err)
	}
}

// A catalogue name is UPSTREAM DATA that becomes a filename. Each of these is
// a real failure, not a hypothetical: traversal walks out of the operator's
// directory, a slash makes a file the publisher's glob never finds (built,
// reported, silently never published), and case-only differences collide on
// macOS.
func TestCatalogueNamesThatCannotBecomeFilesAreRefused(t *testing.T) {
	for name, slug := range map[string]string{
		"traversal":        "../../etc/passwd",
		"a slash":          "J/K",
		"a backslash":      `J\K`,
		"a leading dash":   "-weird", // allowed by shape; kept to document the boundary
		"a null-ish empty": "",
		"a space":          "North West",
		"a colon":          "ns:thing",
	} {
		t.Run(name, func(t *testing.T) {
			err := safeSlug(slug, "test")
			switch slug {
			case "-weird":
				if err != nil {
					t.Errorf("a leading dash is allowed by the shape but was refused: %v", err)
				}
			default:
				if err == nil {
					t.Errorf("%q was accepted as a catalogue name", slug)
				}
			}
		})
	}

	// And the ordinary names must still pass, or the check is just breakage.
	for _, ok := range []string{"MH", "MH-2", "openagrinet.MandiPrice", "region_1"} {
		if err := safeSlug(ok, "test"); err != nil {
			t.Errorf("a legitimate name %q was refused: %v", ok, err)
		}
	}
}

// Two groups whose names differ only in case are ONE file on macOS and
// Windows. Left uncaught, the second overwrites the first on disk and the
// network gets one catalogue published under two ids.
func TestGroupsDifferingOnlyInCaseAreRefused(t *testing.T) {
	records := []map[string]any{
		{"region": "mh", "id": 1},
		{"region": "MH", "id": 2},
	}
	catalog := Catalog{
		GroupBy:  "region",
		Order:    Order{By: "id"},
		Chunk:    Chunk{Budget: 10, Cost: "1", Slug: "${region}"},
		Identity: Identity{CatalogID: "catalog:example:${slug}"},
		Render:   Render{Mapping: "mappings/catalog.yaml"},
	}

	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	_, _, err = buildCatalogues(context.Background(), catalog, records,
		newRunContext(map[string]string{}, "tok"), cache, &echoMapper{}, "http://mappings.test")
	if err == nil {
		t.Fatal("two groups differing only in case were both built; one overwrites the other on disk")
	}
	if !strings.Contains(err.Error(), "case") {
		t.Errorf("error %q does not explain that the collision is a case one", err)
	}
}

// An EXACT slug collision is a different mistake from a case one, and the
// error has to say which.
//
// A chunk template that ignores the chunk index renders the same slug for
// every chunk of a group that splits. Telling the author that the two "differ
// only in case" sends them looking at their group names, which are fine.
func TestAnExactSlugCollisionIsNotReportedAsACaseCollision(t *testing.T) {
	records := []map[string]any{
		{"region": "MH", "id": 1},
		{"region": "MH", "id": 2},
		{"region": "MH", "id": 3},
	}
	catalog := Catalog{
		GroupBy: "region",
		Order:   Order{By: "id"},
		// Budget 2 splits this group in two, and the template names both
		// chunks the same thing.
		Chunk:    Chunk{Budget: 2, Cost: "1", Slug: "${region}"},
		Identity: Identity{CatalogID: "catalog:example:${slug}"},
		Render:   Render{Mapping: "mappings/catalog.yaml"},
	}

	cache, err := newExprCache()
	if err != nil {
		t.Fatalf("newExprCache: %v", err)
	}
	_, _, err = buildCatalogues(context.Background(), catalog, records,
		newRunContext(map[string]string{}, "tok"), cache, &echoMapper{}, "http://mappings.test")
	if err == nil {
		t.Fatal("two chunks rendering one slug were both built; the second overwrites the first")
	}
	if strings.Contains(err.Error(), "case") {
		t.Errorf("an exact collision was reported as a case collision: %v", err)
	}
	if !strings.Contains(err.Error(), "chunk") {
		t.Errorf("error %q does not point at the chunk template that caused it", err)
	}
}
