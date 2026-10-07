package Grievance_test

// pmkisan_mappings_test.go runs the shipped PM-KISAN mappings through the real mapper,
// the real provider step and the real envelope, against a stand-in pkPortal that
// decrypts what it is sent and encrypts what it answers -- as the live one
// does. It is the only test that proves the pieces fit: a mapping is JSONata
// inside YAML fetched over HTTP, the plaintext it produces is sealed before it
// leaves, and nothing but running it establishes what reaches the pkPortal and
// what comes back as Beckn.
//
// An external test package on purpose: it uses the plugins exactly as the
// adapter does, through their exported surface.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/Grievance"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

const pkMappingsDir = "../../../../config/mappings/pmkisan"

const (
	pkCapability = "openagrinet:PMKISANGrievance"
	pkBindingKey = "pmkisan|" + pkCapability

	// Test material only, never the pkPortal's.
	pkKeyHex   = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	pkNonceHex = "a0a1a2a3a4a5a6a7a8a9aaab"
	pkToken    = "test-service-token"

	pkRegistrationNo = "UP12345678A"
)

const pkSupportRequest = `{
  "context": {
    "version": "2.0.0", "action": "support", "networkId": "openagrinet",
    "transactionId": "7f3a0000-0000-4000-8000-000000000001",
    "messageId": "4a1e0000-0000-4000-8000-000000000002",
    "timestamp": "2026-09-28T11:04:00Z"
  },
  "message": {
    "support": {
      "orderId": "UP12345678A",
      "channels": [{
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "grievance": {
          "category": { "code": "G003", "name": "Installment not received" },
          "description": "  Third instalment for 2026 has not been credited.  "
        }
      }]
    }
  }
}`

const pkStatusRequest = `{
  "context": {
    "version": "2.0.0", "action": "status", "networkId": "openagrinet",
    "transactionId": "c04b0000-0000-4000-8000-000000000003",
    "messageId": "e91f0000-0000-4000-8000-000000000004",
    "timestamp": "2026-10-02T09:10:44Z"
  },
  "message": { "contract": {
    "id": "c9b31a45-0f78-4e2d-9a60-84b7d3e15c02",
    "commitments": [{
      "status": { "descriptor": { "code": "ACTIVE" } },
      "offer": {
        "id": "off:pmkisan:grievance",
        "provider": { "id": "pmkisan", "descriptor": { "name": "PM-KISAN Grievance Portal" } },
        "resourceIds": ["res:pmkisan:grievance"]
      },
      "resources": [{ "id": "res:pmkisan:grievance", "quantity": { "count": 1 } }],
      "commitmentAttributes": {
        "@context": "https://openagrinet.github.io/network-specs/api-schemas/PMKISANGrievance/v0.1/context.jsonld",
        "@type": "openagrinet:PMKISANGrievance",
        "informationMode": "OnDemand",
        "scheme": { "code": "PM-KISAN", "name": "Pradhan Mantri Kisan Samman Nidhi" },
        "enrolmentId": "UP12345678A",
        "case": { "filedOn": "2026-09-28" }
      }
    }]
  }}
}`

// pkStatusAnswer is what /GrievanceStatusCheck decrypts to: every grievance on
// the registration number, each carrying the farmer's personal data beside
// the grievance. Three records, two dates, two date formats -- the pkPortal's
// is undocumented, so both shapes the mapping reads are exercised.
const pkStatusAnswer = `{
  "Responce": "True",
  "details": [
    {
      "Reg_No": "UP12345678A", "Farmer_Name": "Ramesh Kumar", "Father_Name": "Suresh Kumar",
      "Gender": "Male", "MobileNo": "9876543210", "StateName": "Uttar Pradesh",
      "DistrictName": "Lucknow", "BlockName": "Malihabad", "RevenueVillageName": "Rahimabad",
      "GrievanceDate": "15/08/2026", "GrievanceStatus": "Closed",
      "GrievanceDescription": "An older grievance.", "OfficerReply": "Resolved.",
      "OfficeReplyDate": "20/08/2026"
    },
    {
      "Reg_No": "UP12345678A", "Farmer_Name": "Ramesh Kumar", "Father_Name": "Suresh Kumar",
      "Gender": "Male", "MobileNo": "9876543210", "StateName": "Uttar Pradesh",
      "DistrictName": "Lucknow", "BlockName": "Malihabad", "RevenueVillageName": "Rahimabad",
      "GrievanceDate": "28/09/2026 11:04:12",
      "GrievanceDescription": "Third instalment for 2026 has not been credited.",
      "OfficerReply": "Instalment released on 2026-10-01, credited to the linked account.",
      "OfficeReplyDate": "01/10/2026"
    }
  ]
}`

// --- the stand-in pkPortal ----------------------------------------------------

type pkPortal struct {
	t      *testing.T
	gcm    cipher.AEAD
	nonce  []byte
	answer string // plaintext to seal; ignored when raw is set
	raw    string // the exact body to answer with, unsealed
	calls  int
	path   string
	sent   map[string]any // the decrypted request
}

func newPKPortal(t *testing.T, answer string) *pkPortal {
	t.Helper()
	key, _ := hex.DecodeString(pkKeyHex)
	nonce, _ := hex.DecodeString(pkNonceHex)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		t.Fatal(err)
	}
	return &pkPortal{t: t, gcm: gcm, nonce: nonce, answer: answer}
}

func (p *pkPortal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.calls++
	p.path = r.URL.Path

	body, _ := io.ReadAll(r.Body)
	var envelope map[string]string
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope) != 1 {
		p.t.Errorf("portal received %s, want exactly {\"EncryptedRequest\": ...}", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sealed, _ := base64.StdEncoding.DecodeString(envelope["EncryptedRequest"])
	plain, err := p.gcm.Open(nil, p.nonce, sealed, nil)
	if err != nil {
		p.t.Errorf("portal could not decrypt the request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	p.sent = map[string]any{}
	_ = json.Unmarshal(plain, &p.sent)

	if p.raw != "" {
		fmt.Fprint(w, p.raw)
		return
	}
	out := base64.StdEncoding.EncodeToString(p.gcm.Seal(nil, p.nonce, []byte(p.answer), nil))
	fmt.Fprintf(w, `{"d":{"__type":"Grievance.Output","output":%q}}`, out)
}

// --- harness ----------------------------------------------------------------

func pkServeMappings(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(filepath.Join(pkMappingsDir, filepath.Base(r.URL.Path)))
		if err != nil {
			t.Errorf("could not read the mapping %q: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, string(body))
	}))
	t.Cleanup(server.Close)
	return server
}

type pkRegistry struct{ plan *model.ProviderRecord }

func (s *pkRegistry) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	return s.plan, nil
}

// runPMKISAN drives the real step over the shipped mappings against p, configured as
// config/provider-adapter.yaml configures it.
func runPMKISAN(t *testing.T, p *pkPortal, request string) ([]byte, error) {
	t.Helper()
	t.Setenv("TEST_PMKISAN_KEY", pkKeyHex)
	t.Setenv("TEST_PMKISAN_NONCE", pkNonceHex)
	t.Setenv("TEST_PMKISAN_TOKEN", pkToken)

	mappings := pkServeMappings(t)
	upstream := httptest.NewServer(p)
	t.Cleanup(upstream.Close)

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatalf("failed to build the mapper: %v", err)
	}
	t.Cleanup(func() { _ = closeMapper() })

	registry := &pkRegistry{plan: &model.ProviderRecord{
		BindingKey:     pkBindingKey,
		ParticipantID:  "pmkisan",
		CapabilityCode: pkCapability,
		BaseURL:        upstream.URL,
		Actions: map[string]model.ActionPlan{
			"support": {Method: http.MethodPost, Path: "/LodgeGrievance",
				Mappings: mappings.URL + "/grievance.support.yaml", TimeoutMs: 30000},
			"status": {Method: http.MethodPost, Path: "/GrievanceStatusCheck",
				Mappings: mappings.URL + "/grievance.status.yaml", TimeoutMs: 30000, RetryMax: 2},
		},
	}}

	// The envelope is built from the provider block exactly as the plugin's
	// cmd package builds it from config/provider-adapter.yaml.
	envelopes, _, err := Grievance.ParseEnvelopes(map[string]string{
		"envelope-pmkisan":              "aesGcm",
		"envelopeKeyEnv-pmkisan":        "TEST_PMKISAN_KEY",
		"envelopeNonceEnv-pmkisan":      "TEST_PMKISAN_NONCE",
		"envelopeTokenEnv-pmkisan":      "TEST_PMKISAN_TOKEN",
		"envelopeTokenField-pmkisan":    "TokenNo",
		"envelopeRequestField-pmkisan":  "EncryptedRequest",
		"envelopeResponsePaths-pmkisan": "d.output,output",
	})
	if err != nil {
		t.Fatalf("ParseEnvelopes() = %v", err)
	}

	step, closeStep, err := Grievance.New(context.Background(), registry, mapper, &Grievance.Config{
		BindingKeys:      []string{pkBindingKey},
		ProviderIDAt:     "message.contract.commitments[].offer.provider.id",
		CapabilityCodeAt: "message.contract.commitments[].commitmentAttributes.@type",
		// support composes no contract; its binding is on the channel.
		FallbackProviderIDAt:     "message.support.channels[].provider.id",
		FallbackCapabilityCodeAt: "message.support.channels[].@type",
		AuthByProvider: map[string]*common.AuthProfile{
			"pmkisan": {Scheme: util.AuthSchemeNone},
		},
		EnvelopeByProvider: envelopes,
	})
	if err != nil {
		t.Fatalf("failed to build the step: %v", err)
	}
	t.Cleanup(func() { _ = closeStep() })

	stepCtx := &model.StepContext{Context: t.Context(), Body: []byte(request)}
	if err := step.Run(stepCtx); err != nil {
		return nil, err
	}
	if len(stepCtx.ResponseBody) == 0 {
		t.Fatal("the step produced no answer and no error")
	}
	return stepCtx.ResponseBody, nil
}

func pkDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, body)
	}
	return answer
}

// pkAt walks a decoded document by keys and indices.
func pkAt(t *testing.T, node any, path ...any) any {
	t.Helper()
	for _, step := range path {
		switch key := step.(type) {
		case string:
			fields, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("at %v: %T is not an object", path, node)
			}
			node = fields[key]
		case int:
			items, ok := node.([]any)
			if !ok || key >= len(items) {
				t.Fatalf("at %v: no element %d", path, key)
			}
			node = items[key]
		}
	}
	return node
}

func pkBecknErr(t *testing.T, err error) (*model.Error, int) {
	t.Helper()
	var coded *model.CodedErr
	if errors.As(err, &coded) {
		return coded.BecknError(), coded.HTTPStatus()
	}
	var ack *model.AckNoCallbackErr
	if errors.As(err, &ack) {
		return ack.BecknError(), http.StatusAccepted
	}
	var other model.BecknErrorer
	if errors.As(err, &other) {
		return other.BecknError(), http.StatusBadRequest
	}
	t.Fatalf("error %v (%T) is not a Beckn error", err, err)
	return nil, 0
}

// --- support ----------------------------------------------------------------

func TestPMKISANSupportLodgesTheGrievanceInPlaintextTheMappingNeverSees(t *testing.T) {
	p := newPKPortal(t, `{"Responce":"True","GrievanceID":"PMK2026091234","message":"Grievance submitted successfully"}`)
	body, err := runPMKISAN(t, p, pkSupportRequest)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	// What the pkPortal decrypted: the mapping's fields plus the pkToken the
	// envelope added. Description trimmed, as the pkPortal measures it.
	if p.path != "/LodgeGrievance" {
		t.Errorf("portal was called at %s, want /LodgeGrievance", p.path)
	}
	for field, want := range map[string]string{
		"Type":                 "Reg_No_Details",
		"IdentityNo":           pkRegistrationNo,
		"GrievanceType":        "G003",
		"GrievanceDescription": "Third instalment for 2026 has not been credited.",
		"TokenNo":              pkToken,
	} {
		if got := p.sent[field]; got != want {
			t.Errorf("portal received %s = %v, want %q", field, got, want)
		}
	}

	answer := pkDecode(t, body)
	if got := pkAt(t, answer, "context", "action"); got != "on_support" {
		t.Errorf("context.action = %v, want on_support", got)
	}
	support := pkAt(t, answer, "message", "support")
	if got := pkAt(t, support, "orderId"); got != pkRegistrationNo {
		t.Errorf("orderId = %v, want it echoed unchanged", got)
	}
	if _, present := support.(map[string]any)["descriptor"]; present {
		t.Error("on_support carries a descriptor; the grievance now travels on the channel")
	}
	channel := pkAt(t, support, "channels", 0)
	for field, want := range map[string]any{
		"@type":           pkCapability,
		"informationMode": "Direct",
	} {
		if got := pkAt(t, channel, field); got != want {
			t.Errorf("channel %s = %v, want %v", field, got, want)
		}
	}
	// The caller's own, echoed: provider and the grievance band.
	if got := pkAt(t, channel, "provider", "id"); got != "pmkisan" {
		t.Errorf("provider.id = %v, want pmkisan", got)
	}
	if got := pkAt(t, channel, "grievance", "category", "code"); got != "G003" {
		t.Errorf("grievance.category.code = %v, want it echoed", got)
	}
	// The case band: the portal's handle, and the adapter's assertions -- the
	// network's code with no name, and today in IST, the date a later status
	// matches on.
	if got := pkAt(t, channel, "case", "ticketNo"); got != "PMK2026091234" {
		t.Errorf("case.ticketNo = %v, want the portal's GrievanceID", got)
	}
	if got := pkAt(t, channel, "case", "status"); fmt.Sprint(got) != fmt.Sprint(map[string]any{"code": "Registered"}) {
		t.Errorf("case.status = %v, want {code: Registered} with no name", got)
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	if got, want := pkAt(t, channel, "case", "filedOn"), time.Now().In(ist).Format("2006-01-02"); got != want {
		t.Errorf("case.filedOn = %v, want today in IST, %s", got, want)
	}

	if strings.Contains(string(body), pkToken) {
		t.Errorf("the answer carries the service token:\n%s", body)
	}
	if strings.Contains(string(body), "Grievance submitted successfully") {
		t.Errorf("the answer passes the portal's message through:\n%s", body)
	}
}

func TestPMKISANSupportRefusesBeforeCallingThePortal(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, code string
	}{
		// Missing: refused by the request half under _error.
		{"no registration number", `"orderId": "UP12345678A",`, ``, "SCH_REQUIRED_FIELD_MISSING"},
		{"no description", `,
          "description": "  Third instalment for 2026 has not been credited.  "`, ``, "SCH_REQUIRED_FIELD_MISSING"},
		{"no category", `"category": { "code": "G003", "name": "Installment not received" },`, ``, "SCH_REQUIRED_FIELD_MISSING"},
		// Malformed: refused by required.
		{"a registration number in native script", `"orderId": "UP12345678A"`, `"orderId": "UP१२३४५"`, "SCH_INVALID_FORMAT"},
		{"a category outside the portal's list", `"code": "G003"`, `"code": "G011"`, "SCH_INVALID_FORMAT"},
		{"a description that says nothing", `"  Third instalment for 2026 has not been credited.  "`, `"   short   "`, "SCH_INVALID_FORMAT"},
		{"a sub-category, which PM-KISAN does not have",
			`"category": { "code": "G003", "name": "Installment not received" },`,
			`"category": { "code": "G003", "name": "Installment not received" }, "subCategory": { "code": "1" },`,
			"SCH_INVALID_FORMAT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := strings.Replace(pkSupportRequest, tc.from, tc.to, 1)
			if request == pkSupportRequest {
				t.Fatalf("the edit %q did not apply to the request", tc.from)
			}
			p := newPKPortal(t, `{"Responce":"True"}`)
			_, err := runPMKISAN(t, p, request)
			if err == nil {
				t.Fatal("Run() succeeded, want a refusal")
			}
			beckn, status := pkBecknErr(t, err)
			if status != http.StatusBadRequest || beckn.Code != tc.code {
				t.Errorf("got %s/%d, want %s/400", beckn.Code, status, tc.code)
			}
			if p.calls != 0 {
				t.Errorf("the portal was called %d times; a refused payload must never reach it", p.calls)
			}
		})
	}
}

func TestPMKISANSupportReportsAPortalRefusalInOurWords(t *testing.T) {
	for name, answer := range map[string]string{
		"Responce": `{"Responce":"False","message":"Registration number not found"}`,
		"Rsponce":  `{"Rsponce":"false","message":"Registration number not found"}`,
		"status":   `{"status":"False","Message":"Registration number not found"}`,
		"Status":   `{"Status":"False","Remark":"Registration number not found"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runPMKISAN(t, newPKPortal(t, answer), pkSupportRequest)
			if err == nil {
				t.Fatal("Run() succeeded on a portal refusal")
			}
			beckn, status := pkBecknErr(t, err)
			if status != http.StatusBadGateway || beckn.Code != "NET_DOWNSTREAM_UNAVAILABLE" {
				t.Errorf("got %s/%d, want NET_DOWNSTREAM_UNAVAILABLE/502", beckn.Code, status)
			}
			if strings.Contains(beckn.Message, "not found") {
				t.Errorf("message %q passes the portal's text through", beckn.Message)
			}
		})
	}
}

func TestPMKISANSupportTreatsPlainTextFromThePortalAsARefusal(t *testing.T) {
	p := newPKPortal(t, "")
	p.raw = `{"d":{"__type":"Grievance.Output","output":"Invalid Token No."}}`
	_, err := runPMKISAN(t, p, pkSupportRequest)
	beckn, status := pkBecknErr(t, err)
	if status != http.StatusBadGateway || beckn.Code != "NET_DOWNSTREAM_UNAVAILABLE" {
		t.Errorf("got %s/%d, want NET_DOWNSTREAM_UNAVAILABLE/502", beckn.Code, status)
	}
}

// --- status -----------------------------------------------------------------

func TestPMKISANStatusAnswersTheGrievanceFiledOnThatDate(t *testing.T) {
	p := newPKPortal(t, pkStatusAnswer)
	body, err := runPMKISAN(t, p, pkStatusRequest)
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if p.path != "/GrievanceStatusCheck" {
		t.Errorf("portal was called at %s, want /GrievanceStatusCheck", p.path)
	}
	if p.sent["Type"] != "Reg_No_Status" || p.sent["IdentityNo"] != pkRegistrationNo || p.sent["TokenNo"] != pkToken {
		t.Errorf("portal received %v, want Reg_No_Status for the registration number, with TokenNo", p.sent)
	}

	answer := pkDecode(t, body)
	if got := pkAt(t, answer, "context", "action"); got != "on_status" {
		t.Errorf("context.action = %v, want on_status", got)
	}
	contract := pkAt(t, answer, "message", "contract")
	if got := pkAt(t, contract, "id"); got != "c9b31a45-0f78-4e2d-9a60-84b7d3e15c02" {
		t.Errorf("contract.id = %v, want it echoed", got)
	}
	if _, present := contract.(map[string]any)["descriptor"]; present {
		t.Error("on_status carries contract.descriptor; the complaint now travels in the grievance band")
	}
	attributes := pkAt(t, contract, "commitments", 0, "commitmentAttributes")
	if got := pkAt(t, attributes, "informationMode"); got != "Direct" {
		t.Errorf("informationMode = %v, want Direct", got)
	}
	if got := pkAt(t, attributes, "grievance"); fmt.Sprint(got) !=
		fmt.Sprint(map[string]any{"description": "Third instalment for 2026 has not been credited."}) {
		t.Errorf("grievance = %v, want the matching record's description alone", got)
	}
	for field, want := range map[string]any{
		"filedOn":    "2026-09-28",
		"remarkedOn": "2026-10-01",
		"remark":     "Instalment released on 2026-10-01, credited to the linked account.",
	} {
		if got := pkAt(t, attributes, "case", field); got != want {
			t.Errorf("case.%s = %v, want %v", field, got, want)
		}
	}
	// No GrievanceStatus on this record, and a remark exists: inferred, so
	// the network's code and no name.
	if got := pkAt(t, attributes, "case", "status"); fmt.Sprint(got) != fmt.Sprint(map[string]any{"code": "Replied"}) {
		t.Errorf("case.status = %v, want {code: Replied} with no name", got)
	}
	if _, present := attributes.(map[string]any)["enrolmentId"]; present {
		t.Error("on_status echoes enrolmentId; the registration number is consumed, not surfaced")
	}
	commitmentStatus := pkAt(t, pkDecode(t, body), "message", "contract", "commitments", 0, "status", "descriptor")
	if fmt.Sprint(commitmentStatus) != fmt.Sprint(map[string]any{"code": "ACTIVE"}) {
		t.Errorf("commitment status = %v, want {code: ACTIVE}", commitmentStatus)
	}

	// The allow-list: nothing about the farmer, and not the other grievance.
	for _, leaked := range []string{"Ramesh", "Suresh", "9876543210", "Rahimabad", "Malihabad",
		pkRegistrationNo, "An older grievance", "Male"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("the answer carries %q, which the allow-list must drop:\n%s", leaked, body)
		}
	}
}

func TestPMKISANStatusMapsThePortalsPhraseToTheNetworksCode(t *testing.T) {
	for phrase, code := range map[string]string{
		// A phrase naming a network state maps to it, case and spacing aside.
		"Under Review": "UnderReview",
		"REPLIED":      "Replied",
		"Closed":       "Closed",
		// The portal's word for a grievance it has answered.
		"Disposed": "Replied",
		// One it does not recognise is non-terminal: never inferred closed.
		"Pending at District": "UnderReview",
	} {
		t.Run(phrase, func(t *testing.T) {
			answer := strings.Replace(pkStatusAnswer, `"GrievanceDate": "28/09/2026 11:04:12",`,
				`"GrievanceDate": "2026-09-28", "GrievanceStatus": "`+phrase+`",`, 1)
			body, err := runPMKISAN(t, newPKPortal(t, answer), pkStatusRequest)
			if err != nil {
				t.Fatalf("Run() = %v", err)
			}
			status := pkAt(t, pkDecode(t, body), "message", "contract", "commitments", 0, "commitmentAttributes", "case", "status")
			if fmt.Sprint(status) != fmt.Sprint(map[string]any{"code": code, "name": phrase}) {
				t.Errorf("case.status = %v, want code %s with the phrase kept in name", status, code)
			}
		})
	}
}

func TestPMKISANStatusReportsNothingOnFileAsAnAcceptedAnswer(t *testing.T) {
	request := strings.Replace(pkStatusRequest, `"case": { "filedOn": "2026-09-28" }`, `"case": { "filedOn": "2026-09-29" }`, 1)
	_, err := runPMKISAN(t, newPKPortal(t, pkStatusAnswer), request)
	if err == nil {
		t.Fatal("Run() answered a date with no grievance on it")
	}
	var ack *model.AckNoCallbackErr
	if !errors.As(err, &ack) {
		t.Fatalf("error %v (%T), want an AckNoCallbackErr", err, err)
	}
	if ack.Status != model.StatusACK || ack.Err.Code != "BIZ_NO_RESULTS_FOUND" {
		t.Errorf("got %s/%s, want ACK/BIZ_NO_RESULTS_FOUND", ack.Status, ack.Err.Code)
	}
}

func TestPMKISANStatusRefusesWithoutAFilingDate(t *testing.T) {
	request := strings.Replace(pkStatusRequest, `,
        "case": { "filedOn": "2026-09-28" }`, ``, 1)
	if request == pkStatusRequest {
		t.Fatal("the edit did not apply to the request")
	}
	p := newPKPortal(t, pkStatusAnswer)
	_, err := runPMKISAN(t, p, request)
	beckn, status := pkBecknErr(t, err)
	if status != http.StatusBadRequest || beckn.Code != "SCH_REQUIRED_FIELD_MISSING" {
		t.Errorf("got %s/%d, want SCH_REQUIRED_FIELD_MISSING/400", beckn.Code, status)
	}
	if p.calls != 0 {
		t.Errorf("the portal was called %d times without a filing date", p.calls)
	}
}

func TestPMKISANStatusReportsAnAnswerThatWillNotDecryptAsOurs(t *testing.T) {
	// The pkPortal answers with an envelope sealed under a nonce this adapter
	// does not hold -- what key or nonce drift looks like from our side.
	p := newPKPortal(t, "")
	other, _ := hex.DecodeString("ffffffffffffffffffffffff")
	sealed := base64.StdEncoding.EncodeToString(p.gcm.Seal(nil, other, []byte(pkStatusAnswer), nil))
	p.raw = `{"d":{"output":"` + sealed + `"}}`

	_, err := runPMKISAN(t, p, pkStatusRequest)
	if err == nil {
		t.Fatal("Run() succeeded on an answer it cannot decrypt")
	}
	beckn, status := pkBecknErr(t, err)
	if status != http.StatusInternalServerError || beckn.Code != "NET_INTERNAL_ERROR" {
		t.Errorf("got %s/%d, want NET_INTERNAL_ERROR/500: a portal that answered correctly is not to blame",
			beckn.Code, status)
	}
}

func TestPMKISANStatusReportsAnAnswerWithNoRecordsAsNothingOnFile(t *testing.T) {
	for name, answer := range map[string]string{
		"no details":    `{"Responce":"True"}`,
		"empty details": `{"Responce":"True","details":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runPMKISAN(t, newPKPortal(t, answer), pkStatusRequest)
			var ack *model.AckNoCallbackErr
			if !errors.As(err, &ack) || ack.Err.Code != "BIZ_NO_RESULTS_FOUND" {
				t.Fatalf("Run() = %v, want an ACK carrying BIZ_NO_RESULTS_FOUND", err)
			}
		})
	}
}

func TestPMKISANSupportReadsTheTicketFromEitherName(t *testing.T) {
	for name, tc := range map[string]struct{ answer, want string }{
		"GrievanceID":                   {`{"Responce":"True","GrievanceID":"PMK1"}`, "PMK1"},
		"GrievanceNo when ID is absent": {`{"Responce":"True","GrievanceNo":"PMK2"}`, "PMK2"},
		"a numeric GrievanceNo":         {`{"Responce":"True","GrievanceNo":20260912}`, "20260912"},
		"neither":                       {`{"Responce":"True"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := runPMKISAN(t, newPKPortal(t, tc.answer), pkSupportRequest)
			if err != nil {
				t.Fatalf("Run() = %v", err)
			}
			c := pkAt(t, pkDecode(t, body), "message", "support", "channels", 0, "case").(map[string]any)
			got, present := c["ticketNo"]
			if tc.want == "" {
				if present {
					t.Errorf("case.ticketNo = %v, want it absent when the portal sends no handle", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("case.ticketNo = %v, want %s", got, tc.want)
			}
		})
	}
}
