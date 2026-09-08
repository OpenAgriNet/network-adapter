package KnowledgeAdvisory_test

// mappings_test.go runs the shipped knowledge mapping through the real mapper
// and the real provider step. It is the only test that proves the three pieces
// fit: a mapping is JSONata inside YAML fetched over HTTP, and nothing but
// running it establishes that what is published actually produces valid Beckn.
//
// An external test package on purpose -- it uses the plugins exactly as the
// adapter does, through their exported surface and nothing else.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/KnowledgeAdvisory"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// mappingsDir is where the shipped mappings live, relative to this package.
const mappingsDir = "../../../../config/mappings/knowledge"

// shippedMapping is the file this binding-action publishes: one file, both
// directions. The action segment must match the action the registry entry
// declares -- a mismatch would apply a correct mapping to the wrong call.
const shippedMapping = "knowledge-advisory.select.yaml"

const shippedCapability = "openagrinet:KnowledgeAdvisory"

const shippedBindingKey = "knowledge-provider|" + shippedCapability

// selectRequest is a KnowledgeAdvisory select in OnDemand mode: it carries the
// topics to search for and the languages it can read, and no advisory of its
// own -- the pack's OnDemand branch requires exactly those two.
const selectRequest = `{
  "context": { "version": "2.0.0", "action": "select",
    "networkId": "oan-dev",
    "transactionId": "9f2c1a8e-4b70-4d31-9c55-6f2e0b1d7a44",
    "messageId": "7d41b9e0-52a6-4c18-8b73-1e9f0a4c6d22",
    "timestamp": "2026-09-09T06:12:01.330Z" },
  "message": { "contract": { "commitments": [{
    "status": { "descriptor": { "code": "DRAFT", "name": "Draft" } },
    "resources": [{
      "id": "res:knowledge-advisory:schemes:ondemand",
      "resourceAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/schema/KnowledgeAdvisory/v0.1/context.jsonld",
        "@type": "openagrinet:KnowledgeAdvisory",
        "informationMode": "OnDemand",
        "topics": ["gruha jyoti scheme eligibility"],
        "languages": ["en"]
      }
    }],
    "offer": {
      "id": "offer:knowledge-advisory:schemes",
      "provider": { "id": "knowledge-provider" },
      "resourceIds": ["res:knowledge-advisory:schemes:ondemand"]
    }
  }] } }
}`

// providerResponse is captured from the live retrieval service, trimmed to the
// fields the mapping reads. Four hits over TWO documents: the engine returns
// several chunks per document, and this corpus indexes one chunk twice, once
// as type "document" and once as "scheme". Note doc_language and
// category_tags come back BLANK -- which is why the mapping takes languages
// and topics from the request instead.
const providerResponse = `{
 "candidate_count": 51,
 "final_count": 4,
 "hits": [
  {
   "_id": "26860172-2762-4b0b-cabf-3c947846c2e1",
   "_score": 0.77918214,
   "doc_id": "74b89b5f69dcd56f6566d4b1a9392ffd",
   "text": "![House icon](a2bc7bb7c74e387ff445bcc6cf9b2a5a_1_img.webp)\n# Gruha Jyoti Scheme:\n1. 200 units of free electricity per family per month\n1.1. Your monthly household electricity consumption:\n0-100 Units\n100-200 Units\nAbove 200 Units\n1.2. Household consumption electricity meter connection number _____\n![Hand icon](a2bc7bb7c74e387ff445bcc6cf9b2a5a_7_img.webp)\n# Chayata Scheme:\nTo receive Rs.4000 per month under Chayata Scheme and Rs.6000 Disability Pension, provide the following details.\n( Those currently receiving pension do not need to apply )\n1. Disability Certificate Number: _____\n2. Others:\nOld Age\nWidow\nWeavers\nCotton Workers\nDiabetics\nAIDS Affected\nBeedi Workers' Livelihood Pension\nFilariasis Affected\nSingle Woman Livelihood Pension\nBeedi Contractor Livelihood Pension\nPath of Progress.. Welfare of All.. Our People's Government\n![Government of Telangana logo](a2bc7bb7c74e387ff445bcc6cf9b2a5a_16_img.webp)\nGovernment of Telangana\n![Circular graphic with Nirmala Sitharaman's portrait and icons for various schemes: Mahalaxmi, Rashtra Janjane, Grahajyoti, Chayata, Yuvavikasan, and Indiramandir](a2bc7bb7c74e387ff445bcc6cf9b2a5a_18_img.webp)![Portrait of Nirmala Sitharaman and N. Chandrababu Naidu](a2bc7bb7c74e387ff445bcc6cf9b2a5a_19_img.webp)\nPeople's Governance\nFrom 28.12.2023 to 06.01.2024",
   "type": "document",
   "instance_name": "Bharat Vistaar",
   "is_reference": false,
   "doc_language": "",
   "category_tags": ""
  },
  {
   "_id": "0e753c2b-040a-4af9-3dc4-c9fca3b01e4d",
   "_score": 0.7783708,
   "doc_id": "74b89b5f69dcd56f6566d4b1a9392ffd",
   "text": "![House icon](71c6c13ee057c1204dd8980597d79d5b_1_img.webp)\n# Gruha Jyoti Scheme:\n1. 200 units of free electricity per family per month\n1.1. Your monthly household electricity consumption:\n0-100 Units\n100-200 Units\nAbove 200 Units\n1.2. Household consumption electricity meter connection number _____\n![Hand icon](71c6c13ee057c1204dd8980597d79d5b_7_img.webp)\n# Chayatha Scheme:\nTo receive Rs.4000 per month under Chayatha Scheme and Rs.6000 pension for disabled persons, provide the following details.\n(Those currently receiving pension do not need to apply)\n1. Disabled person certificate number: _____\n2. Others:\nOld Age\nWidow\nGita Workers\nAgricultural Laborers\nDiabetes Affected\nAIDS Affected\nBeedi Workers' Livelihood Pension\nFilaria Affected\nBantali Women Livelihood Pension\nBeedi Contractors' Livelihood Pension\nPath of Progress.. Welfare of All.. Our People's Government\n![Government of Telangana logo](71c6c13ee057c1204dd8980597d79d5b_16_img.webp)\nGovernment of Telangana\n![Circular graphic with Nirmala Sitharaman's portrait and icons for various schemes: Mahalaxmi, Rashtra Janjane, Grahajyoti, Chayath, Yuvavikasan, and Indiramandir](71c6c13ee057c1204dd8980597d79d5b_18_img.webp)![Portrait of Nirmala Sitharaman and N. Chandrababu Naidu](71c6c13ee057c1204dd8980597d79d5b_19_img.webp)\nPeople's Governance\nFrom 28.12.2023 to 06.01.2024",
   "type": "scheme",
   "scheme_code": "tgj",
   "scheme_name": "Telangana Gruha Jyoti",
   "instance_name": "Bharat Vistaar",
   "is_reference": false,
   "doc_language": "",
   "category_tags": ""
  },
  {
   "_id": "b9ec5c75-7331-0424-f741-4b3294c6b75e",
   "_score": 0.77453613,
   "doc_id": "2cc58f8a1c39a64fc132338be3547f52",
   "text": "- (d) Under the Anna Bhagya Scheme, from which state is the rice being brought; what is the rate of this rice; what is the transportation and other cost? (Details to be provided)\n- (e) Following what standard has the rice brought from outside the state been entrusted for transportation; and which company/agency is currently transporting the rice?\n- (f) At what rate is the levy quantity of rice being collected in the state for the Anna Bhagya Scheme being purchased; for how many months is this collected rice sufficient? (Detailed information to be provided)\n**Smart City Scheme**\n**Shri Kavatagi Matha Mahantesh Mallikarjuna (Local Institutions Department):-**\n1053 (1274) Will the Hon'ble Minister of Urban Development kindly inform the following matters :-\n- (a) Has the fact that the ambitious Smart City Scheme of the Central Government is not being implemented properly in the state come to the government's notice;\n- (b) If it has come to notice, what steps has the government taken to implement the Smart City Scheme in the state and complete it at a rapid pace; for the delay",
   "type": "document",
   "instance_name": "Bharat Vistaar",
   "is_reference": false,
   "doc_language": "",
   "category_tags": ""
  },
  {
   "_id": "4c6466a5-6975-a3be-4a62-637ae1a611d3",
   "_score": 0.7743588,
   "doc_id": "2cc58f8a1c39a64fc132338be3547f52",
   "text": "**Regarding the 'Jalashree' Scheme being implemented by KUIDFC in the State, Shri N. Appajigaoud (Local Institutions Department) :-**\n1031 (1244) Hon'ble Urban Development Minister, please inform on these matters :-\n- (a) What are the urban areas selected under the 24 x 7 drinking water supply scheme under the 'Jalashree' scheme being implemented by KUIDFC in the State, and what is the financial assistance allocated for their implementation; (Provide complete information)\n- (b) On what criteria were the urban areas included in this scheme selected; (Provide details)\n- (c) At what stage is the implementation of works in the urban areas selected under this scheme; (Provide complete information)\n- (d) Many urban areas selected under this scheme have been taken up under the drinking water development program under the Central Government's 'AMRUT' scheme, is it correct to include these urban areas again under the 'Jalashree' scheme; (Provide complete information)\n\n(d) Has the Government considered including the taluks in the districts where drought occurs frequently and the groundwater level is depleting in the state under the earlier 'Jalashree' scheme; if so, provide complete information?\n**Regarding Industrial Clusters in the state and industries established therein, Shri N. Appajigaoud (Local Bodies Department) :-**\n1032 (1248) Hon'ble Minister of Large and Medium Industries, please inform on these matters :-\n- (a) How many Industrial Clusters are there in the state; (Provide district-wise information including area)\n- (b) Out of these Industrial Clusters, in how many clusters has the Automobile industry been established; Provide complete information for each cluster including factories/enterprises;\n- (c) In several Small and Medium Industrial Estates located in the state for several decades such as Peethya Industrial Estate, Bidadi Industrial Estate, Mysore-Hebbal Industrial Estate, Belur-Hubballi Industrial Estate, etc., due to recent economic recession",
   "type": "document",
   "instance_name": "Bharat Vistaar",
   "is_reference": false,
   "doc_language": "",
   "category_tags": ""
  }
 ]
}`

// serveMappings publishes the shipped mapping over HTTP, which is how the
// mapper fetches it in production.
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

type stubRegistry struct{ plan *model.ProviderRecord }

func (s *stubRegistry) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	return s.plan, nil
}

// runShipped drives the real step over the real mapping and returns the body
// the provider was sent and the answer produced.
func runShipped(t *testing.T, request string) (map[string]any, map[string]any) {
	return runShippedWith(t, request, providerResponse)
}

func runShippedWith(t *testing.T, request, providerBody string) (map[string]any, map[string]any) {
	t.Helper()

	mappings := serveMappings(t)
	defer mappings.Close()

	var sent map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
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
		ParticipantID:  "knowledge-provider",
		CapabilityCode: shippedCapability,
		BaseURL:        upstream.URL,
		Actions: map[string]model.ActionPlan{
			// POST with a body -- the first capability to send one. The others
			// are GETs whose mapping output becomes a query string.
			"select": {Method: http.MethodPost, Path: "/docs-pipeline-api/search",
				Mappings: mappings.URL + "/" + shippedMapping, TimeoutMs: 30000, RetryMax: 1},
		},
	}}

	step, closeStep, err := KnowledgeAdvisory.New(context.Background(), registry, mapper,
		&KnowledgeAdvisory.Config{BindingKeys: []string{shippedBindingKey}})
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
	return sent, answer
}

// attributesOf returns the single advisory the answer carries.
func attributesOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	res := resourcesOf(t, answer)
	if len(res) != 1 {
		t.Fatalf("expected one advisory resource, got %d", len(res))
	}
	attrs, ok := res[0].(map[string]any)["resourceAttributes"].(map[string]any)
	if !ok {
		t.Fatal("the resource carries no resourceAttributes")
	}
	return attrs
}

func resourcesOf(t *testing.T, answer map[string]any) []any {
	t.Helper()
	msg, ok := answer["message"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carries no message: %v", answer)
	}
	contract := msg["contract"].(map[string]any)
	commitments := contract["commitments"].([]any)
	if len(commitments) != 1 {
		t.Fatalf("expected one commitment, got %d", len(commitments))
	}
	res, _ := commitments[0].(map[string]any)["resources"].([]any)
	return res
}

// The request half's whole job: the caller's topics become the query. Only
// `query` is required upstream, so nothing else but the corpus is sent.
func TestShippedMappingTurnsTopicsIntoTheQuery(t *testing.T) {
	sent, _ := runShipped(t, selectRequest)

	if got := sent["query"]; got != "gruha jyoti scheme eligibility" {
		t.Errorf("query = %v, want the caller's topics joined", got)
	}
	if _, ok := sent["index_name"]; !ok {
		t.Error("the request names no index_name")
	}
}

// The answer is a Direct advisory: the caller asked a question and this
// payload carries the response, so it must not claim OnDemand -- that would
// tell the caller to invoke a provider it just invoked.
func TestShippedMappingAnswersWithADirectAdvisory(t *testing.T) {
	_, answer := runShipped(t, selectRequest)
	attrs := attributesOf(t, answer)

	if got := attrs["informationMode"]; got != "Direct" {
		t.Errorf("informationMode = %v, want Direct", got)
	}
	if got := attrs["@type"]; got != shippedCapability {
		t.Errorf("@type = %v, want %s", got, shippedCapability)
	}
	// Every field the pack's Direct branch requires.
	for _, required := range []string{"topics", "issuedAt", "recommendations", "source"} {
		if _, ok := attrs[required]; !ok {
			t.Errorf("the advisory omits %q, which Direct requires", required)
		}
	}
}

// ONE recommendation per DOCUMENT, not per hit. Four hits arrive over two
// documents, because the engine returns several chunks per document and this
// corpus indexes one chunk twice. Per-hit would say the same thing twice.
func TestShippedMappingCollapsesHitsToDocuments(t *testing.T) {
	_, answer := runShipped(t, selectRequest)
	attrs := attributesOf(t, answer)

	recs, _ := attrs["recommendations"].([]any)
	if len(recs) != 2 {
		t.Errorf("recommendations = %d, want 2 -- one per document, from 4 hits", len(recs))
	}
	// supportingResourceIds declares uniqueItems, so a per-hit mapping would
	// emit the same id twice and be refused by the pack.
	sup, _ := attrs["supportingResourceIds"].([]any)
	if len(sup) != 2 {
		t.Errorf("supportingResourceIds = %d, want 2 distinct documents", len(sup))
	}
	seen := map[any]bool{}
	for _, id := range sup {
		if seen[id] {
			t.Errorf("supportingResourceIds repeats %v, which uniqueItems forbids", id)
		}
		seen[id] = true
	}
}

// topics and the recommendation language come from the REQUEST. This corpus
// returns category_tags and doc_language BLANK on every hit, so reading them
// from upstream would emit an advisory with an empty language -- which the
// pack requires to be at least two characters.
func TestShippedMappingTakesTopicsAndLanguageFromTheRequest(t *testing.T) {
	_, answer := runShipped(t, selectRequest)
	attrs := attributesOf(t, answer)

	topics, _ := attrs["topics"].([]any)
	if len(topics) != 1 || topics[0] != "gruha jyoti scheme eligibility" {
		t.Errorf("topics = %v, want the caller's own", topics)
	}
	recs, _ := attrs["recommendations"].([]any)
	for i, r := range recs {
		lang := r.(map[string]any)["language"]
		if lang != "en" {
			t.Errorf("recommendation %d language = %v, want the request's en", i, lang)
		}
		if msg, _ := r.(map[string]any)["message"].(string); strings.TrimSpace(msg) == "" {
			t.Errorf("recommendation %d carries no message", i)
		}
	}
}

// source names the PARTICIPANT, never the provider's own `source` field --
// that one reads "docs-pipeline", the ingestion pipeline, not the authority
// behind the knowledge.
func TestShippedMappingNamesTheParticipantAsTheSource(t *testing.T) {
	_, answer := runShipped(t, selectRequest)
	attrs := attributesOf(t, answer)

	source, ok := attrs["source"].(map[string]any)
	if !ok {
		t.Fatal("the advisory carries no source")
	}
	id, _ := source["sourceId"].(string)
	if id == "" {
		t.Error("source carries no sourceId")
	}
	if strings.Contains(id, "docs-pipeline") {
		t.Errorf("sourceId = %q, which is the ingestion pipeline, not the source", id)
	}
}

// NO RESULTS IS AN ANSWER, not a failure. Commitment.resources is required but
// carries no minItems, so an empty array says "nothing matched" without
// inventing an advisory -- and without the dangling resource id that building
// one anyway produced.
func TestShippedMappingAnswersAnEmptyResultWithNoResources(t *testing.T) {
	_, answer := runShippedWith(t, selectRequest,
		`{"candidate_count":0,"final_count":0,"hits":[]}`)

	if res := resourcesOf(t, answer); len(res) != 0 {
		t.Errorf("resources = %d, want 0 for a query nothing matched", len(res))
	}
	commitments := answer["message"].(map[string]any)["contract"].(map[string]any)["commitments"].([]any)
	offer := commitments[0].(map[string]any)["offer"].(map[string]any)
	ids, _ := offer["resourceIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("offer.resourceIds = %v, want only the selected id", ids)
	}
	if !strings.Contains(fmt.Sprint(ids[0]), "ondemand") {
		t.Errorf("offer.resourceIds = %v, want the id the caller selected", ids)
	}
}

// The offer keeps BOTH ids: the OnDemand one the caller selected and the
// Direct one minted here. OnDemand and Direct require different fields, so the
// answer cannot restate the selected resource -- and without the old id the
// caller has nothing tying the callback to what it asked for.
func TestShippedMappingKeepsBothResourceIds(t *testing.T) {
	_, answer := runShipped(t, selectRequest)

	commitments := answer["message"].(map[string]any)["contract"].(map[string]any)["commitments"].([]any)
	offer := commitments[0].(map[string]any)["offer"].(map[string]any)
	ids, _ := offer["resourceIds"].([]any)
	if len(ids) != 2 {
		t.Fatalf("offer.resourceIds = %v, want the selected id and the minted one", ids)
	}
	if !strings.Contains(fmt.Sprint(ids[0]), "ondemand") {
		t.Errorf("the selected id is missing from %v", ids)
	}
	minted := attributesOf(t, answer)
	_ = minted
	res := resourcesOf(t, answer)
	if got := res[0].(map[string]any)["id"]; fmt.Sprint(ids[1]) != fmt.Sprint(got) {
		t.Errorf("offer.resourceIds[1] = %v but the resource id is %v", ids[1], got)
	}
}
