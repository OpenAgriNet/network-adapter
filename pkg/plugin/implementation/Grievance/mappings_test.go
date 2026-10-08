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
	knownTicket       = "100626000099001"
	phone             = "9876543210"
	insertPath        = "/krphapi/FGMS/AddKRPHNCIPGrievenceSupportTicket"
	statusPath        = "/krphapi/FGMS/GetGrievenceTicketsStatus"
	loginPath         = "/krphapi/FGMS/NICUsersLogin"
)

// knownTicketRecord is PMFBY's record for knownTicket, personal data included.
// The category comes back by name alone: a case read carries no category id.
// The ticket number is not returned either, only PMFBY's internal ticket id.
const knownTicketRecord = `{"responseCode":"1","responseMessage":"Fetched","recordCount":1,"responseDynamic":{
	"GrievenceSupportTicketID":109301,"TicketStatus":"Under Review",
	"ComplaintDate":"2026-10-04","ApplicationNo":"040108251010160770605",
	"GrievenceDescription":"Claim not received",
	"TicketCategoryName":"Claim","TicketSubCategoryName":"Claim not received","CropName":"Paddy",
	"FarmerName":"Ramesh","StateMasterName":"Karnataka","DistrictMasterName":"Mysuru",
	"InsuranceCompany":"Insurer"}}`

// pmfbyStub stands in for PMFBY's FGMS: a login that nests its token in the
// reply envelope, and replies whose responseCode "1" is success. Every field
// left zero gives the ordinary answer.
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
		fmt.Fprintf(w, `{"responseCode":"1","responseDynamic":{"token":{"Token":%q}}}`, stubToken)
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
	case insertPath:
		fmt.Fprintf(w, `{"responseCode":"1","responseMessage":"Ticket created","responseDynamic":{"GrievenceSupportTicketNo":%q,"GrievenceSupportTicketID":8842317}}`, knownTicket)
	case statusPath:
		if body["GrievenceSupportTicketNo"] != knownTicket {
			fmt.Fprint(w, `{"responseCode":"1","responseMessage":"No record found","responseDynamic":null}`)
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
// status; support gets 0, as the seed ships it. tweak edits the
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
			TokenResponseField: "responseDynamic.token.Token", TokenTTLRaw: "10m", HeaderName: "Authorization",
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
			ID          string `json:"id"`
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
			OrderID  string           `json:"orderId"`
			Channels []map[string]any `json:"channels"`
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

// band reads one of the nested objects a payload groups its fields in: the
// grievance, the case, or a status inside a case.
func band(of map[string]any, name string) map[string]any {
	inner, _ := of[name].(map[string]any)
	return inner
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

// request builds a Beckn grievance payload for status: a contract whose
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
				"status":               map[string]any{"descriptor": map[string]any{"code": "ACTIVE"}},
				"resources":            []any{map[string]any{"id": "res:pmfby:grievance"}},
				"offer":                map[string]any{"id": "off:pmfby:grievance", "provider": map[string]any{"id": "pmfby"}},
				"commitmentAttributes": attributes,
			}},
		}},
	})
	return body
}

// support builds a Beckn support request, which composes no contract. fields is
// flat: orderId goes on support; categoryCode, subCategoryCode and description
// make channels[0].grievance; everything else goes on channels[0]. A field left
// out is absent from the payload.
func support(fields map[string]any) []byte {
	channel := map[string]any{"@context": packContext, "@type": "openagrinet:PMFBYGrievance",
		"informationMode": "OnDemand", "scheme": scheme, "provider": provider}
	body := map[string]any{"channels": []any{channel}}
	grievance := map[string]any{}
	for field, value := range fields {
		switch field {
		case "orderId":
			body["orderId"] = value
		case "categoryCode":
			grievance["category"] = map[string]any{"code": value, "name": "Claim"}
		case "subCategoryCode":
			grievance["subCategory"] = map[string]any{"code": value, "name": "Claim not received"}
		case "description":
			grievance["description"] = value
		default:
			channel[field] = value
		}
	}
	if len(grievance) > 0 {
		channel["grievance"] = grievance
	}
	raw, _ := json.Marshal(map[string]any{"context": becknContext("support"), "message": map[string]any{"support": body}})
	return raw
}

func supportFields() map[string]any {
	return map[string]any{
		"orderId": "040108251010160770605", "categoryCode": "3", "subCategoryCode": "10",
		"description": "  Claim not received  ", "applicantPhone": phone,
		"cropYear": "2025", "season": "Kharif",
	}
}

func statusAttributes() map[string]any {
	return map[string]any{"applicantPhone": phone, "case": map[string]any{"ticketNo": knownTicket}}
}

// refusal is the HTTP status and Beckn error a failed request is answered
// with, whichever of the two step error types carries it.
func refusal(t *testing.T, err error) (int, *model.Error) {
	t.Helper()
	var invalid *model.SchemaValidationErr
	var coded *model.CodedErr
	switch {
	case errors.As(err, &invalid):
		return http.StatusBadRequest, invalid.BecknError()
	case errors.As(err, &coded):
		return coded.HTTPStatus(), coded.BecknError()
	}
	t.Fatalf("error = %v (%T), want a refusal", err, err)
	return 0, nil
}

// assertCoded fails unless err is answered with this status and code.
func assertCoded(t *testing.T, err error, status int, code string) {
	t.Helper()
	if got, refused := refusal(t, err); got != status || refused.Code != code {
		t.Errorf("error = %d %s (%v), want %d %s", got, refused.Code, err, status, code)
	}
}

// todayIST is the date PMFBY expects complaintDate in.
func todayIST() string {
	return time.Now().In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02")
}

// --- support ---------------------------------------------------------------

func TestSupport_Lodged_SendsPMFBYFieldsAndReturnsTicket(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	got := h.mustSend(t, support(supportFields()))

	// No OTP: PMFBY's grievance service asks for none. Every value goes as a
	// string, the season as its code.
	want := map[string]any{
		"requestorMobileNo": phone, "applicationNo": "040108251010160770605",
		"requestYear": "2025", "requestSeason": "1", "ticketCategoryID": "3", "ticketSubCategoryID": "10",
		"grievenceDescription": "Claim not received", "complaintDate": todayIST(), "receiptSourceID": "134306",
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

	if got.Context["action"] != "on_support" || got.Context["transactionId"] != "txn-1" {
		t.Errorf("context = %v, want on_support on the request's transaction", got.Context)
	}
	// orderId and the grievance are the caller's own, echoed unchanged.
	channel := got.channel()
	grievance := band(channel, "grievance")
	if got.Message.Support.OrderID != "040108251010160770605" || band(grievance, "category")["code"] != "3" ||
		band(grievance, "subCategory")["code"] != "10" || grievance["description"] != "  Claim not received  " {
		t.Errorf("support = %+v, want orderId and grievance echoed", got.Message.Support)
	}
	filed := band(channel, "case")
	status := band(filed, "status")
	if filed["ticketNo"] != knownTicket || len(status) != 1 || status["code"] != "Registered" ||
		filed["filedOn"] != todayIST() || band(channel, "provider")["id"] != "pmfby" ||
		channel["informationMode"] != "Direct" || channel["scheme"] == nil {
		t.Errorf("channel = %v", channel)
	}
	for _, private := range []string{"applicantPhone", "ticketId"} {
		if _, echoed := channel[private]; echoed {
			t.Errorf("%s must not be echoed", private)
		}
	}
}

// The pack holds ticketNo as a string of digits, whichever PMFBY sends.
func TestSupport_NumericTicketNo_ReturnedAsString(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{
		insertPath: fmt.Sprintf(`{"responseCode":1,"responseDynamic":{"GrievenceSupportTicketNo":%s}}`, knownTicket)}}, 0)
	if got := band(h.mustSend(t, support(supportFields())).channel(), "case")["ticketNo"]; got != knownTicket {
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
	for season, code := range map[string]string{"Kharif": "1", "Rabi": "2", "Zaid": "3"} {
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

func TestSupport_CategoryCodes_SentAsGiven(t *testing.T) {
	for codes, want := range map[[2]string][2]string{{"3", "10"}: {"3", "10"}, {"12", "5"}: {"12", "5"}, {"1", "1"}: {"1", "1"}} {
		t.Run(codes[0]+"/"+codes[1], func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["categoryCode"], fields["subCategoryCode"] = codes[0], codes[1]
			h.mustSend(t, support(fields))
			sent := h.pmfby.bodies[insertPath]
			if sent["ticketCategoryID"] != want[0] || sent["ticketSubCategoryID"] != want[1] {
				t.Errorf("sent %v / %v, want %v", sent["ticketCategoryID"], sent["ticketSubCategoryID"], want)
			}
		})
	}
}

func TestSupport_MissingField_Returns400NamingItsPath(t *testing.T) {
	channel := "$.message.support.channels[0]."
	for field, path := range map[string]string{
		"orderId": "$.message.support.orderId", "applicantPhone": channel + "applicantPhone",
		"cropYear": channel + "cropYear", "season": channel + "season",
		"categoryCode": channel + "grievance.category.code", "subCategoryCode": channel + "grievance.subCategory.code",
		"description": channel + "grievance.description",
	} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			delete(fields, field)
			_, err := h.send(t, support(fields))
			assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
			if _, refused := refusal(t, err); refused.Details == nil || refused.Details.Path != path {
				t.Errorf("details = %+v, want path %s", refused.Details, path)
			}
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called for a request missing a required field")
			}
		})
	}
}

func TestSupport_NoGrievance_Returns400(t *testing.T) {
	fields := supportFields()
	for _, field := range []string{"categoryCode", "subCategoryCode", "description"} {
		delete(fields, field)
	}
	_, err := newHarness(t, &pmfbyStub{}, 0).send(t, support(fields))
	assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
}

func TestSupport_MalformedCategory_Returns400WithoutCallingPMFBY(t *testing.T) {
	for name, codes := range map[string][2]string{
		"joined": {"3.10", "10"}, "letters": {"a", "10"}, "empty": {"", "10"},
		"sub-category joined": {"3", "3.10"}, "sub-category letters": {"3", "b"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["categoryCode"], fields["subCategoryCode"] = codes[0], codes[1]
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

// complaintDate and receiptSourceId are optional: sent when the caller has
// them, and filedOn restates the date that went.
func TestSupport_CallerDateAndSource_SentInsteadOfDefaults(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	fields := supportFields()
	fields["complaintDate"], fields["receiptSourceId"] = "2026-09-20", "200001"
	got := h.mustSend(t, support(fields))
	sent := h.pmfby.bodies[insertPath]
	if sent["complaintDate"] != "2026-09-20" || sent["receiptSourceID"] != "200001" {
		t.Errorf("PMFBY was sent %v / %v, want the caller's date and source", sent["complaintDate"], sent["receiptSourceID"])
	}
	if filed := band(got.channel(), "case")["filedOn"]; filed != "2026-09-20" {
		t.Errorf("filedOn = %v, want the date that was sent", filed)
	}
}

func TestSupport_MalformedDateOrSource_Returns400WithoutCallingPMFBY(t *testing.T) {
	for name, field := range map[string][2]string{
		"date": {"complaintDate", "20-09-2026"}, "source": {"receiptSourceId", "web"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields[field[0]] = field[1]
			_, err := h.send(t, support(fields))
			assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
			if h.pmfby.calls[insertPath] != 0 {
				t.Errorf("PMFBY was called with %s %q", field[0], field[1])
			}
		})
	}
}

// PMFBY answers its own refusal with HTTP 200 and a responseCode other than
// "1". That filed nothing, so it is never reported as Registered.
func TestSupport_PortalDidNotRegister_Returns502(t *testing.T) {
	for name, reply := range map[string]string{
		"refused":            `{"responseCode":"0","responseMessage":"Invalid application number","responseDynamic":null}`,
		"success, no ticket": `{"responseCode":"1","responseDynamic":{}}`,
		"refused, ticket":    `{"responseCode":"0","responseDynamic":{"GrievenceSupportTicketNo":"100626000099001"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{answers: map[string]string{insertPath: reply}}, 0)
			_, err := h.send(t, support(supportFields()))
			assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
			if strings.Contains(err.Error(), "Invalid application") {
				t.Errorf("error %q leaks PMFBY's message", err)
			}
		})
	}
}

// A 4xx is PMFBY's refusal: no OTP is involved, so nothing is the farmer's to fix.
func TestSupport_PortalAnswers4xx_Returns502(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity} {
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
	attributes := got.attributes()
	// The complaint as PMFBY holds it: the category by name, with no code.
	grievance := band(attributes, "grievance")
	category, subCategory := band(grievance, "category"), band(grievance, "subCategory")
	if len(category) != 1 || category["name"] != "Claim" || len(subCategory) != 1 ||
		subCategory["name"] != "Claim not received" || grievance["description"] != "Claim not received" {
		t.Errorf("grievance = %v, want the complaint as PMFBY holds it", grievance)
	}
	filed := band(attributes, "case")
	if attributes["enrolmentId"] != "040108251010160770605" || attributes["informationMode"] != "Direct" ||
		filed["ticketNo"] != knownTicket || filed["filedOn"] != "2026-10-04" || filed["cropName"] != "Paddy" {
		t.Errorf("commitmentAttributes = %v", attributes)
	}
	// An unrecognised portal phrase is UnderReview, the phrase kept as the name.
	if status := band(filed, "status"); status["code"] != "UnderReview" || status["name"] != "Under Review" {
		t.Errorf("case.status = %v, want UnderReview / Under Review", status)
	}
	if got.Context["action"] != "on_status" || got.status() != "ACTIVE" {
		t.Errorf("action %v, status %v; want on_status, ACTIVE", got.Context["action"], got.status())
	}
}

func TestStatus_KnownTicket_DropsPersonalAndInternalFields(t *testing.T) {
	got := newHarness(t, &pmfbyStub{}, 0).mustSend(t, request("status", statusAttributes()))
	allowed := map[string]map[string]bool{
		"": {"@context": true, "@type": true, "informationMode": true, "scheme": true,
			"enrolmentId": true, "grievance": true, "case": true},
		"grievance": {"category": true, "subCategory": true, "description": true},
		"case":      {"ticketNo": true, "status": true, "filedOn": true, "cropName": true},
	}
	attributes := got.attributes()
	for name, fields := range allowed {
		in := attributes
		if name != "" {
			in = band(attributes, name)
		}
		for field := range in {
			if !fields[field] {
				t.Errorf("%s %s is mapped; the response is an allow-list", name, field)
			}
		}
	}
}

// A case read always carries a status, so a missing phrase still says
// UnderReview; only the name, which would quote the portal, is dropped.
func TestStatus_NoStatusPhrase_UnderReviewWithoutName(t *testing.T) {
	for name, phrase := range map[string]string{"null": `null`, "empty": `""`} {
		t.Run(name, func(t *testing.T) {
			record := strings.Replace(knownTicketRecord, `"TicketStatus":"Under Review"`, `"TicketStatus":`+phrase, 1)
			got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
				mustSend(t, request("status", statusAttributes()))
			status := band(band(got.attributes(), "case"), "status")
			if len(status) != 1 || status["code"] != "UnderReview" {
				t.Errorf("case.status = %v, want code UnderReview alone", status)
			}
		})
	}
}

func TestStatus_NullFields_DroppedNotMappedOrFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		from, to    string
		band, field string
	}{
		"null category name":  {`"TicketCategoryName":"Claim"`, `"TicketCategoryName":null`, "grievance", "category"},
		"empty sub-category":  {`"TicketSubCategoryName":"Claim not received"`, `"TicketSubCategoryName":""`, "grievance", "subCategory"},
		"null description":    {`"GrievenceDescription":"Claim not received"`, `"GrievenceDescription":null`, "grievance", "description"},
		"null complaint date": {`"ComplaintDate":"2026-10-04"`, `"ComplaintDate":null`, "case", "filedOn"},
		"null application no": {`"ApplicationNo":"040108251010160770605"`, `"ApplicationNo":null`, "", "enrolmentId"},
		"empty crop":          {`"CropName":"Paddy"`, `"CropName":""`, "case", "cropName"},
	} {
		t.Run(name, func(t *testing.T) {
			record := strings.Replace(knownTicketRecord, tc.from, tc.to, 1)
			got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
				mustSend(t, request("status", statusAttributes()))
			in := got.attributes()
			if tc.band != "" {
				in = band(in, tc.band)
			}
			if value, present := in[tc.field]; present {
				t.Errorf("%s = %v, want it dropped", tc.field, value)
			}
			if band(got.attributes(), "case")["ticketNo"] != knownTicket {
				t.Errorf("case = %v, want the rest of the answer intact", band(got.attributes(), "case"))
			}
		})
	}
}

func TestStatus_NoRecordInReply_Returns202NoResultsFound(t *testing.T) {
	attributes := statusAttributes()
	attributes["case"] = map[string]any{"ticketNo": "999"}
	_, err := newHarness(t, &pmfbyStub{}, 0).send(t, request("status", attributes))

	var ack *model.AckNoCallbackErr
	if !errors.As(err, &ack) {
		t.Fatalf("error = %v, want a 202 ACK", err)
	}
	if ack.Status != model.StatusACK || ack.Err.Code != "BIZ_NO_RESULTS_FOUND" {
		t.Errorf("answer = %s %s, want ACK BIZ_NO_RESULTS_FOUND", ack.Status, ack.Err.Code)
	}
}

func TestStatus_PortalRefused_Returns502(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{
		statusPath: `{"responseCode":"0","responseMessage":"Session expired"}`}}, 0)
	_, err := h.send(t, request("status", statusAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
}

func TestStatus_MissingField_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, field := range []string{"applicantPhone", "case"} {
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

func TestFlow_SupportThenStatus_LogsInOnce(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	ticket := band(h.mustSend(t, support(supportFields())).channel(), "case")["ticketNo"]
	got := h.mustSend(t, request("status", map[string]any{"applicantPhone": phone, "case": map[string]any{"ticketNo": ticket}}))

	if status := band(band(got.attributes(), "case"), "status"); status["code"] != "UnderReview" {
		t.Errorf("status of the filed ticket = %v", got.attributes())
	}
	if logins := h.pmfby.calls[loginPath]; logins != 1 {
		t.Errorf("logged in %d times over two calls, want 1", logins)
	}
}

func TestFlow_AnswerNotJSON_ReturnsErrorWithoutMapping(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{insertPath: `<html>maintenance</html>`}}, 0)
	got, err := h.send(t, support(supportFields()))
	if err == nil || got != nil {
		t.Errorf("Run() = %v, %v; want an error and no answer", got, err)
	}
}

// init included: PMFBY publishes no challenge, so there is no init to serve.
func TestFlow_UnservedAction_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, action := range []string{"init", "select"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			_, err := h.send(t, request(action, statusAttributes()))
			assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
			if len(h.pmfby.calls) != 0 {
				t.Errorf("PMFBY was called %v for an action it does not serve", h.pmfby.calls)
			}
		})
	}
}

func TestFlow_OtherCapability_PassesThroughUntouched(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	body := strings.Replace(string(request("status", statusAttributes())),
		"openagrinet:PMFBYGrievance", "openagrinet:PMKISANGrievance", 1)
	got, err := h.send(t, []byte(body))
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want another capability passed on untouched", got, err)
	}
	if len(h.pmfby.calls) != 0 {
		t.Errorf("PMFBY was called %v for another capability", h.pmfby.calls)
	}
}
