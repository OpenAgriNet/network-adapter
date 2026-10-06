package Grievance_test

// mappings_test.go runs the shipped PMFBY mapping files through the real mapper
// and the real grievance step, against a stub PMFBY that logs in, sends an OTP,
// files a ticket and reads it back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/Grievance"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

const (
	mappingsDir       = "../../../../config/mappings/pmfby"
	shippedBindingKey = "pmfby|openagrinet:PMFBYGrievance"
	stubToken         = "stub-login-token"
	validOTP          = "123456"
	knownTicket       = "100626000099001"
	phone             = "9876543210"
	sendOTPPath       = "/SendOTP"
	insertPath        = "/AddKRPHNCIPGrievenceSupportTicket"
	statusPath        = "/GetGrievenceTicketsStatus"
	loginPath         = "/NICUsersLogin"
)

// knownTicketRecord is PMFBY's record for knownTicket, personal data included.
const knownTicketRecord = `{"responseCode":1,"responseMessage":"Fetched","responseDynamic":{
	"GrievenceSupportTicketNo":"100626000099001","TicketStatus":"Under Review","TicketStatusID":109301,
	"ComplaintDate":"2026-10-04","ApplicationNo":"040108251010160770605",
	"GrievenceDescription":"Claim not received","TicketCategoryID":3,"TicketSubCategoryID":10,
	"TicketCategoryName":"Claim","TicketSubCategoryName":"Claim not received",
	"RequestYear":2025,"RequestSeason":1,"latestRemark":"Forwarded to insurer",
	"FarmerName":"Ramesh","RequestorMobileNo":"9876543210","InsurancePolicyNo":"P-1"}}`

// pmfbyStub stands in for PMFBY. A refusal is a non-2xx; PMFBY's real failure
// shape is not documented, so nothing here depends on one. Every field left
// zero gives the ordinary answer.
type pmfbyStub struct {
	down      bool              // 503 on every call but the login
	loginDown bool              // 401 on the login
	answers   map[string]string // overrides the answer for a path
	statusFor map[string]int    // answers a path with this status and no body
	calls     map[string]int    // per path, login included
	bodies    map[string]map[string]any
}

func (p *pmfbyStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.calls[r.URL.Path]++
	if r.URL.Path == loginPath {
		var login map[string]string
		_ = json.NewDecoder(r.Body).Decode(&login)
		if p.loginDown || login["appAccessUID"] != "user" || login["appAccessPWD"] != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"token":%q}`, stubToken)
		return
	}
	if p.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != stubToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status, set := p.statusFor[r.URL.Path]; set {
		w.WriteHeader(status)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	p.bodies[r.URL.Path] = body

	if answer, overridden := p.answers[r.URL.Path]; overridden {
		fmt.Fprint(w, answer)
		return
	}
	switch r.URL.Path {
	case sendOTPPath:
		fmt.Fprint(w, `{"responseCode":1,"responseMessage":"OTP sent"}`)
	case insertPath:
		if body["otp"] != validOTP {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"responseMessage":"Invalid OTP"}`)
			return
		}
		fmt.Fprintf(w, `{"responseCode":1,"responseMessage":"Ticket created","responseDynamic":{"GrievenceSupportTicketNo":%q,"GrievenceSupportTicketID":8842317}}`, knownTicket)
	case statusPath:
		if body["GrievenceSupportTicketNo"] != knownTicket {
			fmt.Fprint(w, `{"responseCode":1,"responseMessage":"No record found","responseDynamic":null}`)
			return
		}
		fmt.Fprint(w, knownTicketRecord)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type stubRegistry struct{ plan *model.ProviderRecord }

func (s *stubRegistry) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	return s.plan, nil
}

// harness is the real step over the shipped mappings, wired to a stub PMFBY.
// One harness serves several requests, so a held token carries across them.
type harness struct {
	pmfby *pmfbyStub
	step  definition.Step
}

// newHarness builds the step. statusRetries is the registry's retryMax for
// status; init and support get 0, as the seed ships them. tweak edits the
// shipped config before the step is built.
func newHarness(t *testing.T, pmfby *pmfbyStub, statusRetries int, tweak ...func(*Grievance.Config)) *harness {
	t.Helper()
	t.Setenv("PMFBY_USER", "user")
	t.Setenv("PMFBY_PASSWORD", "secret")
	pmfby.calls = map[string]int{}
	pmfby.bodies = map[string]map[string]any{}

	mappings := httptest.NewServer(http.FileServer(http.Dir(mappingsDir)))
	t.Cleanup(mappings.Close)
	upstream := httptest.NewServer(pmfby)
	t.Cleanup(upstream.Close)

	mapper, closeMapper, err := jsonmapper.New(context.Background(), &jsonmapper.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeMapper() })

	action := func(path, file string, retries int) model.ActionPlan {
		return model.ActionPlan{Method: http.MethodPost, Path: path,
			Mappings: mappings.URL + "/" + file, RetryMax: retries}
	}
	registry := &stubRegistry{plan: &model.ProviderRecord{
		BindingKey: shippedBindingKey, ParticipantID: "pmfby", CapabilityCode: "openagrinet:PMFBYGrievance",
		BaseURL: upstream.URL,
		Actions: map[string]model.ActionPlan{
			"init":    action(sendOTPPath, "grievance.init.yaml", 0),
			"support": action(insertPath, "grievance.support.yaml", 0),
			"status":  action(statusPath, "grievance.status.yaml", statusRetries),
		},
	}}

	cfg := &Grievance.Config{
		BindingKeys:      []string{shippedBindingKey},
		ProviderIDAt:     "message.contract.commitments[].offer.provider.id",
		CapabilityCodeAt: "message.contract.commitments[].commitmentAttributes.@type",
		// A support request composes no contract; its channel names both.
		FallbackProviderIDAt:     "message.support.channels[].provider.id",
		FallbackCapabilityCodeAt: "message.support.channels[].@type",
		AuthByProvider: map[string]*common.AuthProfile{"pmfby": {
			Scheme: util.AuthSchemeTokenHeader, TokenURL: upstream.URL + loginPath,
			TokenUserField: "appAccessUID", TokenUserEnv: "PMFBY_USER",
			TokenSecretName: "appAccessPWD", TokenSecretEnv: "PMFBY_PASSWORD",
			TokenResponseField: "token", TokenTTLRaw: "10m", HeaderName: "Authorization",
		}},
	}
	for _, apply := range tweak {
		apply(cfg)
	}
	step, closeStep, err := Grievance.New(context.Background(), registry, mapper, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeStep() })
	return &harness{pmfby: pmfby, step: step}
}

// answer is the part of a Beckn reply these tests read.
type answer struct {
	Context map[string]any `json:"context"`
	Message struct {
		Contract struct {
			ID          string         `json:"id"`
			Descriptor  map[string]any `json:"descriptor"`
			Commitments []struct {
				Status struct {
					Descriptor struct {
						Code string `json:"code"`
					} `json:"descriptor"`
				} `json:"status"`
				Offer                map[string]any `json:"offer"`
				CommitmentAttributes map[string]any `json:"commitmentAttributes"`
			} `json:"commitments"`
		} `json:"contract"`
		Support struct {
			OrderID    string           `json:"orderId"`
			Descriptor map[string]any   `json:"descriptor"`
			Channels   []map[string]any `json:"channels"`
		} `json:"support"`
	} `json:"message"`
}

// channel is the case record on an on_support.
func (a *answer) channel() map[string]any {
	return a.Message.Support.Channels[0]
}

func (a *answer) attributes() map[string]any {
	return a.Message.Contract.Commitments[0].CommitmentAttributes
}

func (a *answer) status() string {
	return a.Message.Contract.Commitments[0].Status.Descriptor.Code
}

// send runs body through the step. A nil answer means the step passed it on.
func (h *harness) send(t *testing.T, body []byte) (*answer, error) {
	t.Helper()
	stepCtx := &model.StepContext{Context: t.Context(), Body: body}
	if err := h.step.Run(stepCtx); err != nil {
		return nil, err
	}
	if len(stepCtx.ResponseBody) == 0 {
		return nil, nil
	}
	var got answer
	if err := json.Unmarshal(stepCtx.ResponseBody, &got); err != nil {
		t.Fatalf("answer is not JSON: %v\n%s", err, stepCtx.ResponseBody)
	}
	return &got, nil
}

// mustSend is send for a request that has to succeed.
func (h *harness) mustSend(t *testing.T, body []byte) *answer {
	t.Helper()
	got, err := h.send(t, body)
	if err != nil {
		t.Fatalf("Run() = %v, want success", err)
	}
	if got == nil {
		t.Fatal("the step passed the request on instead of answering it")
	}
	return got
}

const packContext = "https://openagrinet.github.io/network-specs/api-schemas/PMFBYGrievance/v0.1/context.jsonld"

// scheme is what every PMFBY payload names, in both directions.
var scheme = map[string]any{"code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana"}

// provider is the portal a support request names on its channel, since it
// composes no contract to name it on.
var provider = map[string]any{"id": "pmfby", "descriptor": map[string]any{"name": "PMFBY Grievance Portal"}}

func becknContext(action string) map[string]any {
	return map[string]any{"version": "2.0.0", "action": action, "transactionId": "txn-1", "messageId": "msg-1"}
}

// request builds a Beckn grievance payload for init or status: a contract whose
// one commitment carries these attributes.
func request(action string, attributes map[string]any) []byte {
	attributes["@context"] = packContext
	attributes["@type"] = "openagrinet:PMFBYGrievance"
	attributes["informationMode"] = "OnDemand"
	attributes["scheme"] = scheme
	body, _ := json.Marshal(map[string]any{
		"context": becknContext(action),
		"message": map[string]any{"contract": map[string]any{
			"id": "contract-1",
			"commitments": []any{map[string]any{
				"status":               map[string]any{"descriptor": map[string]any{"code": "DRAFT"}},
				"resources":            []any{map[string]any{"id": "res:pmfby:grievance"}},
				"offer":                map[string]any{"id": "offer:pmfby:grievance", "provider": map[string]any{"id": "pmfby"}},
				"commitmentAttributes": attributes,
			}},
		}},
	})
	return body
}

// support builds a Beckn support request, which composes no contract. fields is
// flat: orderId goes on support, code, name and longDesc on its descriptor, and
// everything else on channels[0]. A field left out is absent from the payload.
func support(fields map[string]any) []byte {
	channel := map[string]any{"@context": packContext, "@type": "openagrinet:PMFBYGrievance",
		"informationMode": "OnDemand", "scheme": scheme, "provider": provider}
	body := map[string]any{"channels": []any{channel}}
	descriptor := map[string]any{}
	for field, value := range fields {
		switch field {
		case "orderId":
			body["orderId"] = value
		case "code", "name", "longDesc":
			descriptor[field] = value
		default:
			channel[field] = value
		}
	}
	if len(descriptor) > 0 {
		body["descriptor"] = descriptor
	}
	raw, _ := json.Marshal(map[string]any{"context": becknContext("support"), "message": map[string]any{"support": body}})
	return raw
}

func initAttributes() map[string]any { return map[string]any{"applicantPhone": phone} }

// challenge is the OTP as a support request carries it.
func challenge(otp string) map[string]any {
	return map[string]any{"method": "SMS_OTP", "value": otp}
}

func supportFields() map[string]any {
	return map[string]any{
		"orderId": "040108251010160770605", "code": "3.10", "name": "Claim / Claim not received",
		"longDesc": "  Claim not received  ", "applicantPhone": phone, "challenge": challenge(validOTP),
		"cropYear": "2025", "season": "Kharif",
	}
}

func statusAttributes() map[string]any {
	return map[string]any{"applicantPhone": phone, "ticketNo": knownTicket}
}

// assertCoded fails unless err is a CodedErr with this status and code.
func assertCoded(t *testing.T, err error, status int, code string) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) {
		t.Fatalf("error = %v, want a coded %d %s", err, status, code)
	}
	if coded.HTTPStatus() != status || coded.BecknError().Code != code {
		t.Errorf("error = %d %s (%v), want %d %s", coded.HTTPStatus(), coded.BecknError().Code, err, status, code)
	}
}

// todayIST is the date PMFBY expects complaintDate in.
func todayIST() string {
	return time.Now().In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")
}

// --- init ------------------------------------------------------------------

func TestInit_ValidPhone_ReturnsMaskedOTPChallenge(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	before := time.Now()
	got := h.mustSend(t, request("init", initAttributes()))

	if sent := h.pmfby.bodies[sendOTPPath]; len(sent) != 1 || sent["requestorMobileNo"] != phone {
		t.Errorf("PMFBY was sent %v, want requestorMobileNo alone", sent)
	}
	issued, _ := got.attributes()["challengeIssued"].(map[string]any)
	if issued["method"] != "SMS_OTP" || issued["sentTo"] != "98XXXXXX10" {
		t.Errorf("challengeIssued = %v, want SMS_OTP sent to 98XXXXXX10", issued)
	}
	// Ten minutes from the call, the validity the adapter derives.
	expiresAt, err := time.Parse(time.RFC3339, fmt.Sprint(issued["expiresAt"]))
	if err != nil || expiresAt.Before(before.Add(10*time.Minute-time.Second)) ||
		expiresAt.After(time.Now().Add(10*time.Minute+time.Second)) {
		t.Errorf("expiresAt = %v, want ten minutes from now", issued["expiresAt"])
	}
	if got.Context["action"] != "on_init" || got.status() != "DRAFT" {
		t.Errorf("action %v, status %v; want on_init, DRAFT", got.Context["action"], got.status())
	}
	if got.attributes()["informationMode"] != "OnDemand" || got.attributes()["scheme"] == nil {
		t.Errorf("commitmentAttributes = %v, want OnDemand and the scheme echoed", got.attributes())
	}
	if _, echoed := got.attributes()["applicantPhone"]; echoed {
		t.Error("applicantPhone must not be echoed")
	}
}

func TestInit_Answer_EchoesContextContractAndType(t *testing.T) {
	got := newHarness(t, &pmfbyStub{}, 0).mustSend(t, request("init", initAttributes()))

	if got.Context["transactionId"] != "txn-1" || got.Context["messageId"] != "msg-1" || got.Context["timestamp"] == nil {
		t.Errorf("context = %v, want the request's ids and a fresh timestamp", got.Context)
	}
	if got.Message.Contract.ID != "contract-1" {
		t.Errorf("contract id = %q, want contract-1 carried across the flow", got.Message.Contract.ID)
	}
	if got.attributes()["@type"] != "openagrinet:PMFBYGrievance" || got.attributes()["@context"] == nil {
		t.Errorf("commitmentAttributes = %v, want @type and @context echoed", got.attributes())
	}
	if got.Message.Contract.Commitments[0].Offer["id"] != "offer:pmfby:grievance" {
		t.Errorf("offer = %v, want the request's offer", got.Message.Contract.Commitments[0].Offer)
	}
}

func TestInit_MissingPhone_Returns400WithoutCallingPMFBY(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	_, err := h.send(t, request("init", map[string]any{}))
	assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
	if h.pmfby.calls[sendOTPPath] != 0 {
		t.Error("PMFBY was called for a request missing a required field")
	}
}

func TestInit_NumericPhone_Returns400WithoutSendingOTP(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	_, err := h.send(t, request("init", map[string]any{"applicantPhone": 9876543210}))
	assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
	if h.pmfby.calls[sendOTPPath] != 0 {
		t.Error("SendOTP was called for a phone the reply cannot mask")
	}
}

func TestInit_PortalDown_Returns502AfterOneAttempt(t *testing.T) {
	h := newHarness(t, &pmfbyStub{down: true}, 0)
	_, err := h.send(t, request("init", initAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
	if got := h.pmfby.calls[sendOTPPath]; got != 1 {
		t.Errorf("SendOTP called %d times, want 1: a retry sends a second OTP", got)
	}
}

func TestInit_LoginRejected_Returns502WithoutSendingOTP(t *testing.T) {
	h := newHarness(t, &pmfbyStub{loginDown: true}, 0)
	_, err := h.send(t, request("init", initAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
	if h.pmfby.calls[sendOTPPath] != 0 {
		t.Error("SendOTP was called without a token")
	}
}

// --- support ---------------------------------------------------------------

func TestSupport_ValidOTP_SendsPMFBYFieldsAndReturnsTicket(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	got := h.mustSend(t, support(supportFields()))

	want := map[string]any{
		"requestorMobileNo": phone, "otp": validOTP, "applicationNo": "040108251010160770605",
		"requestYear": "2025", "requestSeason": 1.0, "ticketCategoryID": 3.0, "ticketSubCategoryID": 10.0,
		"grievenceDescription": "Claim not received", "complaintDate": todayIST(), "receiptSourceID": 134306.0,
	}
	sent := h.pmfby.bodies[insertPath]
	for field, value := range want {
		if sent[field] != value {
			t.Errorf("PMFBY %s = %v, want %v", field, sent[field], value)
		}
	}
	if len(sent) != len(want) {
		t.Errorf("PMFBY was sent %d fields (%v), want exactly %d", len(sent), sent, len(want))
	}

	if got.Context["action"] != "on_support" {
		t.Errorf("action = %v, want on_support", got.Context["action"])
	}
	// orderId and descriptor are the caller's own, echoed unchanged.
	if got.Message.Support.OrderID != "040108251010160770605" || got.Message.Support.Descriptor["code"] != "3.10" {
		t.Errorf("support = %+v, want orderId and descriptor echoed", got.Message.Support)
	}
	channel := got.channel()
	caseStatus, _ := channel["caseStatus"].(map[string]any)
	routedTo, _ := channel["provider"].(map[string]any)
	if channel["ticketNo"] != knownTicket || len(caseStatus) != 1 || caseStatus["code"] != "Registered" ||
		channel["filedOn"] != todayIST() || routedTo["id"] != "pmfby" ||
		channel["informationMode"] != "Direct" || channel["scheme"] == nil {
		t.Errorf("channel = %v", channel)
	}
	for _, private := range []string{"challenge", "applicantPhone", "ticketId"} {
		if _, echoed := channel[private]; echoed {
			t.Errorf("%s must not be echoed", private)
		}
	}
}

// The pack holds ticketNo as a string of digits, whichever PMFBY sends.
func TestSupport_NumericTicketNo_ReturnedAsString(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{
		insertPath: fmt.Sprintf(`{"responseCode":1,"responseDynamic":{"GrievenceSupportTicketNo":%s}}`, knownTicket)}}, 0)
	if got := h.mustSend(t, support(supportFields())).channel()["ticketNo"]; got != knownTicket {
		t.Errorf("ticketNo = %#v, want %q", got, knownTicket)
	}
}

func TestSupport_NumericCropYear_SentAsString(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	fields := supportFields()
	fields["cropYear"] = 2025
	h.mustSend(t, support(fields))
	if got := h.pmfby.bodies[insertPath]["requestYear"]; got != "2025" {
		t.Errorf("requestYear = %#v, want the string \"2025\" PMFBY takes", got)
	}
}

func TestSupport_EachSeason_SendsItsCode(t *testing.T) {
	for season, code := range map[string]float64{"Kharif": 1, "Rabi": 2, "Zaid": 3} {
		t.Run(season, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["season"] = season
			h.mustSend(t, support(fields))
			if got := h.pmfby.bodies[insertPath]["requestSeason"]; got != code {
				t.Errorf("requestSeason = %v, want %v", got, code)
			}
		})
	}
}

func TestSupport_CategoryCode_SplitsOnTheDot(t *testing.T) {
	for category, want := range map[string][2]float64{"3.10": {3, 10}, "12.5": {12, 5}, "1.1": {1, 1}} {
		t.Run(category, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["code"] = category
			h.mustSend(t, support(fields))
			sent := h.pmfby.bodies[insertPath]
			if sent["ticketCategoryID"] != want[0] || sent["ticketSubCategoryID"] != want[1] {
				t.Errorf("sent %v / %v, want %v", sent["ticketCategoryID"], sent["ticketSubCategoryID"], want)
			}
		})
	}
}

func TestSupport_MissingField_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, field := range []string{"orderId", "code", "longDesc", "applicantPhone", "challenge", "cropYear", "season"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			delete(fields, field)
			_, err := h.send(t, support(fields))
			assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called for a request missing a required field")
			}
		})
	}
}

func TestSupport_NoDescriptor_Returns400(t *testing.T) {
	fields := supportFields()
	for _, field := range []string{"code", "name", "longDesc"} {
		delete(fields, field)
	}
	_, err := newHarness(t, &pmfbyStub{}, 0).send(t, support(fields))
	assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
}

func TestSupport_MalformedCategory_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, category := range []string{"3", "3.", ".10", "3.10.1", "a.b", "3-10", ""} {
		t.Run(category, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["code"] = category
			_, err := h.send(t, support(fields))
			assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called with a malformed category")
			}
		})
	}
}

func TestSupport_UnknownSeason_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, season := range []string{"Summer", "kharif", "KHARIF", ""} {
		t.Run(season, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["season"] = season
			_, err := h.send(t, support(fields))
			assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called with an unknown season")
			}
		})
	}
}

// PMFBY refusing the lodge call with a 400 -- the stub's wrong OTP -- is a wrong
// OTP to the caller, and the lodge call is not repeated.
func TestSupport_PortalAnswers400_Returns400BizGenericError(t *testing.T) {
	fields := supportFields()
	fields["challenge"] = challenge("000000")
	h := newHarness(t, &pmfbyStub{}, 0)
	_, err := h.send(t, support(fields))
	assertCoded(t, err, http.StatusBadRequest, "BIZ_GENERIC_ERROR")
	if strings.Contains(err.Error(), "Invalid OTP") {
		t.Errorf("error %q leaks PMFBY's body", err)
	}
	if got := h.pmfby.calls[insertPath]; got != 1 {
		t.Errorf("the lodge call was made %d times, want 1", got)
	}
}

// Any other 4xx is PMFBY's refusal, not the farmer's OTP.
func TestSupport_PortalAnswersOther4xx_Returns502(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{statusFor: map[string]int{insertPath: status}}, 0)
			_, err := h.send(t, support(supportFields()))
			assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
			if got := h.pmfby.calls[insertPath]; got != 1 {
				t.Errorf("the lodge call was made %d times, want 1", got)
			}
		})
	}
}

// A 400 on another action is not an OTP error: only support says so.
func TestStatus_PortalAnswers400_Returns502(t *testing.T) {
	h := newHarness(t, &pmfbyStub{statusFor: map[string]int{statusPath: http.StatusBadRequest}}, 0)
	_, err := h.send(t, request("status", statusAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
}

func TestSupport_PortalDown_Returns502AfterOneAttempt(t *testing.T) {
	h := newHarness(t, &pmfbyStub{down: true}, 0)
	_, err := h.send(t, support(supportFields()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
	if got := h.pmfby.calls[insertPath]; got != 1 {
		t.Errorf("the lodge call was made %d times, want 1: a retry files a duplicate", got)
	}
}

func TestSupport_SecondChannel_Returns400WithoutCallingPMFBY(t *testing.T) {
	var payload map[string]any
	_ = json.Unmarshal(support(supportFields()), &payload)
	body := payload["message"].(map[string]any)["support"].(map[string]any)
	body["channels"] = append(body["channels"].([]any), body["channels"].([]any)[0])
	raw, _ := json.Marshal(payload)

	h := newHarness(t, &pmfbyStub{}, 0)
	_, err := h.send(t, raw)
	assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
	if !strings.Contains(err.Error(), "2 channels") {
		t.Errorf("error %q should say \"2 channels\" so the caller can act on it", err)
	}
	if h.pmfby.calls[insertPath] != 0 {
		t.Error("PMFBY was called for a request naming two channels")
	}
}

// The fallback keys are what make a support request reachable at all: without
// them it composes no contract the plugin can read a binding from.
func TestSupport_FallbackNotConfigured_PassesThroughUntouched(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0, func(c *Grievance.Config) {
		c.FallbackProviderIDAt, c.FallbackCapabilityCodeAt = "", ""
	})
	got, err := h.send(t, support(supportFields()))
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want the support request passed on untouched", got, err)
	}
	if len(h.pmfby.calls) != 0 {
		t.Errorf("PMFBY was called %v without a way to recognise the request", h.pmfby.calls)
	}
}

func TestSupport_NoProvider_PassesThrough(t *testing.T) {
	var payload map[string]any
	_ = json.Unmarshal(support(supportFields()), &payload)
	channels := payload["message"].(map[string]any)["support"].(map[string]any)["channels"].([]any)
	delete(channels[0].(map[string]any), "provider")
	body, _ := json.Marshal(payload)
	h := newHarness(t, &pmfbyStub{}, 0)
	got, err := h.send(t, body)
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want a support request naming no provider passed on", got, err)
	}
}

// --- status ----------------------------------------------------------------

func TestStatus_KnownTicket_ReturnsCaseRecordAndComplaint(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	got := h.mustSend(t, request("status", statusAttributes()))

	sent := h.pmfby.bodies[statusPath]
	if len(sent) != 2 || sent["GrievenceSupportTicketNo"] != knownTicket || sent["requestorMobileNo"] != phone {
		t.Errorf("PMFBY was sent %v, want the ticket and the phone alone", sent)
	}
	descriptor := got.Message.Contract.Descriptor
	if descriptor["code"] != "3.10" || descriptor["name"] != "Claim / Claim not received" ||
		descriptor["longDesc"] != "Claim not received" {
		t.Errorf("contract.descriptor = %v, want the complaint as PMFBY holds it", descriptor)
	}
	attributes := got.attributes()
	for field, want := range map[string]any{
		"ticketNo": knownTicket, "applicationNo": "040108251010160770605", "cropYear": "2025",
		"season": "Kharif", "filedOn": "2026-10-04", "caseRemark": "Forwarded to insurer",
		"informationMode": "Direct",
	} {
		if attributes[field] != want {
			t.Errorf("%s = %v, want %v", field, attributes[field], want)
		}
	}
	// An unrecognised portal phrase is UnderReview, the phrase kept as the name.
	caseStatus, _ := attributes["caseStatus"].(map[string]any)
	if caseStatus["code"] != "UnderReview" || caseStatus["name"] != "Under Review" {
		t.Errorf("caseStatus = %v, want UnderReview / Under Review", caseStatus)
	}
	if got.Context["action"] != "on_status" || got.status() != "ACTIVE" {
		t.Errorf("action %v, status %v; want on_status, ACTIVE", got.Context["action"], got.status())
	}
}

func TestStatus_KnownTicket_DropsPersonalAndInternalFields(t *testing.T) {
	got := newHarness(t, &pmfbyStub{}, 0).mustSend(t, request("status", statusAttributes()))
	allowed := map[string]bool{"@context": true, "@type": true, "informationMode": true, "scheme": true,
		"ticketNo": true, "applicationNo": true, "cropYear": true, "season": true, "caseStatus": true,
		"filedOn": true, "caseRemark": true}
	for field := range got.attributes() {
		if !allowed[field] {
			t.Errorf("%s is mapped; the response is an allow-list", field)
		}
	}
}

func TestStatus_NoRemarkYet_OmitsCaseRemark(t *testing.T) {
	record := strings.Replace(knownTicketRecord, `"latestRemark":"Forwarded to insurer"`, `"latestRemark":""`, 1)
	got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes()))
	if _, present := got.attributes()["caseRemark"]; present {
		t.Errorf("caseRemark = %v, want it omitted while PMFBY has none", got.attributes()["caseRemark"])
	}
}

// The pack requires caseStatus on a case read, so a missing phrase still says
// UnderReview; only the name, which would quote the portal, is dropped.
func TestStatus_NoStatusPhrase_UnderReviewWithoutName(t *testing.T) {
	for name, phrase := range map[string]string{"null": `null`, "empty": `""`} {
		t.Run(name, func(t *testing.T) {
			record := strings.Replace(knownTicketRecord, `"TicketStatus":"Under Review"`, `"TicketStatus":`+phrase, 1)
			got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
				mustSend(t, request("status", statusAttributes()))
			caseStatus, _ := got.attributes()["caseStatus"].(map[string]any)
			if len(caseStatus) != 1 || caseStatus["code"] != "UnderReview" {
				t.Errorf("caseStatus = %v, want code UnderReview alone", caseStatus)
			}
		})
	}
}

func TestStatus_NullFields_DroppedNotMappedOrFailed(t *testing.T) {
	inAttributes := func(field string) func(*answer) map[string]any {
		return func(a *answer) map[string]any { return map[string]any{field: a.attributes()[field]} }
	}
	inDescriptor := func(field string) func(*answer) map[string]any {
		return func(a *answer) map[string]any { return map[string]any{field: a.Message.Contract.Descriptor[field]} }
	}
	for name, tc := range map[string]struct {
		from, to string
		field    string
		where    func(string) func(*answer) map[string]any
	}{
		"null sub-category id": {`"TicketSubCategoryID":10`, `"TicketSubCategoryID":null`, "code", inDescriptor},
		"null description":     {`"GrievenceDescription":"Claim not received"`, `"GrievenceDescription":null`, "longDesc", inDescriptor},
		"null complaint date":  {`"ComplaintDate":"2026-10-04"`, `"ComplaintDate":null`, "filedOn", inAttributes},
		"null remark":          {`"latestRemark":"Forwarded to insurer"`, `"latestRemark":null`, "caseRemark", inAttributes},
		"null application no":  {`"ApplicationNo":"040108251010160770605"`, `"ApplicationNo":null`, "applicationNo", inAttributes},
		"null request season":  {`"RequestSeason":1`, `"RequestSeason":null`, "season", inAttributes},
	} {
		t.Run(name, func(t *testing.T) {
			record := strings.Replace(knownTicketRecord, tc.from, tc.to, 1)
			got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
				mustSend(t, request("status", statusAttributes()))
			if value := tc.where(tc.field)(got)[tc.field]; value != nil {
				t.Errorf("%s = %v, want it dropped", tc.field, value)
			}
			if got.attributes()["ticketNo"] != knownTicket {
				t.Errorf("ticketNo = %v, want the rest of the answer intact", got.attributes()["ticketNo"])
			}
		})
	}
}

func TestStatus_NullCategoryName_KeepsCodeDropsName(t *testing.T) {
	record := strings.Replace(knownTicketRecord, `"TicketCategoryName":"Claim"`, `"TicketCategoryName":null`, 1)
	got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes()))
	descriptor := got.Message.Contract.Descriptor
	if descriptor["code"] != "3.10" {
		t.Errorf("code = %v, want 3.10", descriptor["code"])
	}
	if name, present := descriptor["name"]; present {
		t.Errorf("name = %v, want it dropped rather than \"null / ...\"", name)
	}
}

func TestStatus_NoRecordInReply_Returns202NoResultsFound(t *testing.T) {
	attributes := statusAttributes()
	attributes["ticketNo"] = "999"
	_, err := newHarness(t, &pmfbyStub{}, 0).send(t, request("status", attributes))

	var ack *model.AckNoCallbackErr
	if !errors.As(err, &ack) {
		t.Fatalf("error = %v, want a 202 ACK", err)
	}
	if ack.Status != model.StatusACK || ack.Err.Code != "BIZ_NO_RESULTS_FOUND" {
		t.Errorf("answer = %s %s, want ACK BIZ_NO_RESULTS_FOUND", ack.Status, ack.Err.Code)
	}
}

func TestStatus_MissingField_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, field := range []string{"applicantPhone", "ticketNo"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			attributes := statusAttributes()
			delete(attributes, field)
			_, err := h.send(t, request("status", attributes))
			assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
			if h.pmfby.calls[statusPath] != 0 {
				t.Error("PMFBY was called for a request missing a required field")
			}
		})
	}
}

func TestStatus_PortalDown_RetriedAsTheRegistrySays(t *testing.T) {
	h := newHarness(t, &pmfbyStub{down: true}, 2)
	_, err := h.send(t, request("status", statusAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
	if got := h.pmfby.calls[statusPath]; got != 3 {
		t.Errorf("status called %d times, want 3: a read is safe to retry", got)
	}
}

// --- across the flow -------------------------------------------------------

func TestFlow_InitSupportStatus_LogsInOnce(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	h.mustSend(t, request("init", initAttributes()))
	ticket := h.mustSend(t, support(supportFields())).channel()["ticketNo"]
	got := h.mustSend(t, request("status", map[string]any{"applicantPhone": phone, "ticketNo": ticket}))

	if caseStatus, _ := got.attributes()["caseStatus"].(map[string]any); caseStatus["code"] != "UnderReview" {
		t.Errorf("status of the filed ticket = %v", got.attributes())
	}
	if logins := h.pmfby.calls[loginPath]; logins != 1 {
		t.Errorf("logged in %d times over three calls, want 1", logins)
	}
}

func TestFlow_AnswerNotJSON_ReturnsErrorWithoutMapping(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{sendOTPPath: `<html>maintenance</html>`}}, 0)
	got, err := h.send(t, request("init", initAttributes()))
	if err == nil || got != nil {
		t.Errorf("Run() = %v, %v; want an error and no answer", got, err)
	}
}

func TestFlow_UnservedAction_Returns400WithoutCallingPMFBY(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	_, err := h.send(t, request("select", initAttributes()))
	assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
	if len(h.pmfby.calls) != 0 {
		t.Errorf("PMFBY was called %v for an action it does not serve", h.pmfby.calls)
	}
}

func TestFlow_OtherCapability_PassesThroughUntouched(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	body := strings.Replace(string(request("init", initAttributes())),
		"openagrinet:PMFBYGrievance", "openagrinet:PMKISANGrievance", 1)
	got, err := h.send(t, []byte(body))
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want another capability passed on untouched", got, err)
	}
	if len(h.pmfby.calls) != 0 {
		t.Errorf("PMFBY was called %v for another capability", h.pmfby.calls)
	}
}
