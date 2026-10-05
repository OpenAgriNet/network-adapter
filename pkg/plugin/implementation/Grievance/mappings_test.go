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

// pmfbyStub answers like PMFBY: 200 for everything, success or failure in
// responseCode. Every field left zero gives PMFBY's ordinary answer.
type pmfbyStub struct {
	down      bool              // 503 on every call but the login
	loginDown bool              // 401 on the login
	answers   map[string]string // overrides the answer for a path
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
		fmt.Fprint(w, `{"responseCode":1,"responseMessage":"OTP sent","responseDynamic":{"otpValidityInSeconds":300}}`)
	case insertPath:
		if body["otp"] != validOTP {
			fmt.Fprint(w, `{"responseCode":0,"responseMessage":"Invalid OTP","responseDynamic":null}`)
			return
		}
		fmt.Fprintf(w, `{"responseCode":1,"responseMessage":"Ticket created","responseDynamic":{"GrievenceSupportTicketNo":%q}}`, knownTicket)
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
// status; init and confirm get 0, as the seed ships them.
func newHarness(t *testing.T, pmfby *pmfbyStub, statusRetries int) *harness {
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
			"confirm": action(insertPath, "grievance.confirm.yaml", 0),
			"status":  action(statusPath, "grievance.status.yaml", statusRetries),
		},
	}}

	step, closeStep, err := Grievance.New(context.Background(), registry, mapper, &Grievance.Config{
		BindingKeys:      []string{shippedBindingKey},
		ProviderIDAt:     "message.contract.commitments[].offer.provider.id",
		CapabilityCodeAt: "message.contract.commitments[].commitmentAttributes.@type",
		AuthByProvider: map[string]*common.AuthProfile{"pmfby": {
			Scheme: util.AuthSchemeTokenHeader, TokenURL: upstream.URL + loginPath,
			TokenUserField: "appAccessUID", TokenUserEnv: "PMFBY_USER",
			TokenSecretName: "appAccessPWD", TokenSecretEnv: "PMFBY_PASSWORD",
			TokenResponseField: "token", TokenTTLRaw: "10m", HeaderName: "Authorization",
		}},
	})
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
	} `json:"message"`
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

// request builds a Beckn grievance payload for action with these attributes.
func request(action string, attributes map[string]any) []byte {
	attributes["@context"] = "https://schemas.openagrinet.global/schema/PMFBYGrievance/v0.1/context.jsonld"
	attributes["@type"] = "openagrinet:PMFBYGrievance"
	body, _ := json.Marshal(map[string]any{
		"context": map[string]any{"version": "2.0.0", "action": action, "transactionId": "txn-1", "messageId": "msg-1"},
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

func initAttributes() map[string]any { return map[string]any{"applicantPhone": phone} }

func confirmAttributes() map[string]any {
	return map[string]any{
		"applicantPhone": phone, "otp": validOTP, "applicationNo": "040108251010160770605",
		"cropYear": "2025", "season": "Kharif", "grievanceCategory": map[string]any{"code": "3.10"},
		"grievanceDescription": "  Claim not received  ",
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
	challenge, _ := got.attributes()["otpChallenge"].(map[string]any)
	if challenge["sentTo"] != "98XXXXXX10" {
		t.Errorf("sentTo = %v, want 98XXXXXX10", challenge["sentTo"])
	}
	expiresAt, err := time.Parse(time.RFC3339, fmt.Sprint(challenge["expiresAt"]))
	if err != nil {
		t.Fatalf("expiresAt %v is not RFC 3339: %v", challenge["expiresAt"], err)
	}
	if wait := expiresAt.Sub(before); wait < 299*time.Second || wait > 301*time.Second {
		t.Errorf("expiresAt is %v away, want the 300s PMFBY reported", wait)
	}
	if got.Context["action"] != "on_init" || got.status() != "DRAFT" {
		t.Errorf("action %v, status %v; want on_init, DRAFT", got.Context["action"], got.status())
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

func TestInit_PortalRefuses_Returns502(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{
		sendOTPPath: `{"responseCode":0,"responseMessage":"Mobile 9876543210 not registered"}`}}, 0)
	_, err := h.send(t, request("init", initAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
	if strings.Contains(err.Error(), phone) {
		t.Errorf("error %q leaks PMFBY's message", err)
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

// --- confirm ---------------------------------------------------------------

func TestConfirm_ValidOTP_SendsPMFBYFieldsAndReturnsTicket(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	got := h.mustSend(t, request("confirm", confirmAttributes()))

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

	attributes := got.attributes()
	if attributes["ticketNo"] != knownTicket || attributes["caseStatus"] != "REGISTERED" ||
		attributes["filedOn"] != todayIST() || attributes["source"] != "PMFBY" {
		t.Errorf("commitmentAttributes = %v", attributes)
	}
	if got.Context["action"] != "on_confirm" || got.status() != "ACTIVE" {
		t.Errorf("action %v, status %v; want on_confirm, ACTIVE", got.Context["action"], got.status())
	}
	for _, private := range []string{"otp", "applicantPhone"} {
		if _, echoed := attributes[private]; echoed {
			t.Errorf("%s must never be echoed", private)
		}
	}
}

func TestConfirm_NumericCropYear_SentAsString(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	attributes := confirmAttributes()
	attributes["cropYear"] = 2025
	h.mustSend(t, request("confirm", attributes))
	if got := h.pmfby.bodies[insertPath]["requestYear"]; got != "2025" {
		t.Errorf("requestYear = %#v, want the string \"2025\" PMFBY takes", got)
	}
}

func TestConfirm_EachSeason_SendsItsCode(t *testing.T) {
	for season, code := range map[string]float64{"Kharif": 1, "Rabi": 2, "Zaid": 3} {
		t.Run(season, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			attributes := confirmAttributes()
			attributes["season"] = season
			h.mustSend(t, request("confirm", attributes))
			if got := h.pmfby.bodies[insertPath]["requestSeason"]; got != code {
				t.Errorf("requestSeason = %v, want %v", got, code)
			}
		})
	}
}

func TestConfirm_CategoryCode_SplitsOnTheDot(t *testing.T) {
	for category, want := range map[string][2]float64{"3.10": {3, 10}, "12.5": {12, 5}, "1.1": {1, 1}} {
		t.Run(category, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			attributes := confirmAttributes()
			attributes["grievanceCategory"] = map[string]any{"code": category}
			h.mustSend(t, request("confirm", attributes))
			sent := h.pmfby.bodies[insertPath]
			if sent["ticketCategoryID"] != want[0] || sent["ticketSubCategoryID"] != want[1] {
				t.Errorf("sent %v / %v, want %v", sent["ticketCategoryID"], sent["ticketSubCategoryID"], want)
			}
		})
	}
}

func TestConfirm_MissingField_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, field := range []string{"applicantPhone", "otp", "applicationNo", "cropYear", "season",
		"grievanceCategory", "grievanceDescription"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			attributes := confirmAttributes()
			delete(attributes, field)
			_, err := h.send(t, request("confirm", attributes))
			assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called for a request missing a required field")
			}
		})
	}
}

func TestConfirm_CategoryWithoutCode_Returns400(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	attributes := confirmAttributes()
	attributes["grievanceCategory"] = map[string]any{"name": "Claim"}
	_, err := h.send(t, request("confirm", attributes))
	assertCoded(t, err, http.StatusBadRequest, "SCH_REQUIRED_FIELD_MISSING")
}

func TestConfirm_MalformedCategory_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, category := range []string{"3", "3.", ".10", "3.10.1", "a.b", "3-10", ""} {
		t.Run(category, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			attributes := confirmAttributes()
			attributes["grievanceCategory"] = map[string]any{"code": category}
			_, err := h.send(t, request("confirm", attributes))
			assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called with a malformed category")
			}
		})
	}
}

func TestConfirm_UnknownSeason_Returns400WithoutCallingPMFBY(t *testing.T) {
	for _, season := range []string{"Summer", "kharif", "KHARIF", ""} {
		t.Run(season, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			attributes := confirmAttributes()
			attributes["season"] = season
			_, err := h.send(t, request("confirm", attributes))
			assertCoded(t, err, http.StatusBadRequest, "SCH_INVALID_FORMAT")
			if h.pmfby.calls[insertPath] != 0 {
				t.Error("PMFBY was called with an unknown season")
			}
		})
	}
}

func TestConfirm_WrongOrExpiredOTP_Returns400BizGenericError(t *testing.T) {
	for _, message := range []string{"Invalid OTP", "OTP expired", "otp mismatch"} {
		t.Run(message, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{answers: map[string]string{
				insertPath: fmt.Sprintf(`{"responseCode":0,"responseMessage":%q}`, message)}}, 0)
			_, err := h.send(t, request("confirm", confirmAttributes()))
			assertCoded(t, err, http.StatusBadRequest, "BIZ_GENERIC_ERROR")
			if strings.Contains(err.Error(), message) {
				t.Errorf("error %q leaks PMFBY's message", err)
			}
		})
	}
}

func TestConfirm_WrongOTPFromStub_Returns400BizGenericError(t *testing.T) {
	attributes := confirmAttributes()
	attributes["otp"] = "000000"
	_, err := newHarness(t, &pmfbyStub{}, 0).send(t, request("confirm", attributes))
	assertCoded(t, err, http.StatusBadRequest, "BIZ_GENERIC_ERROR")
}

func TestConfirm_PortalRefuses_Returns502(t *testing.T) {
	for name, answer := range map[string]string{
		"failure code":        `{"responseCode":0,"responseMessage":"Service error for 9876543210"}`,
		"no message":          `{"responseCode":0}`,
		"no code":             `{"responseMessage":"something"}`,
		"null message":        `{"responseCode":0,"responseMessage":null}`,
		"empty message":       `{"responseCode":0,"responseMessage":""}`,
		"code as other value": `{"responseCode":2,"responseMessage":"duplicate"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{answers: map[string]string{insertPath: answer}}, 0)
			_, err := h.send(t, request("confirm", confirmAttributes()))
			assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
			if strings.Contains(err.Error(), phone) {
				t.Errorf("error %q leaks PMFBY's message", err)
			}
		})
	}
}

func TestConfirm_PortalDown_Returns502AfterOneAttempt(t *testing.T) {
	h := newHarness(t, &pmfbyStub{down: true}, 0)
	_, err := h.send(t, request("confirm", confirmAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
	if got := h.pmfby.calls[insertPath]; got != 1 {
		t.Errorf("InsertGrievenceTicket called %d times, want 1: a retry files a duplicate", got)
	}
}

// --- status ----------------------------------------------------------------

func TestStatus_KnownTicket_ReturnsAllowListedDetails(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	got := h.mustSend(t, request("status", statusAttributes()))

	sent := h.pmfby.bodies[statusPath]
	if len(sent) != 2 || sent["GrievenceSupportTicketNo"] != knownTicket || sent["requestorMobileNo"] != phone {
		t.Errorf("PMFBY was sent %v, want the ticket and the phone alone", sent)
	}
	attributes := got.attributes()
	category, _ := attributes["grievanceCategory"].(map[string]any)
	for field, want := range map[string]any{
		"ticketNo": knownTicket, "caseStatus": "UNDER_REVIEW", "filedOn": "2026-10-04",
		"grievanceDescription": "Claim not received", "officerReply": "Forwarded to insurer", "source": "PMFBY",
	} {
		if attributes[field] != want {
			t.Errorf("%s = %v, want %v", field, attributes[field], want)
		}
	}
	if category["code"] != "3.10" || category["name"] != "Claim / Claim not received" {
		t.Errorf("grievanceCategory = %v", category)
	}
	if got.Context["action"] != "on_status" || got.status() != "ACTIVE" {
		t.Errorf("action %v, status %v; want on_status, ACTIVE", got.Context["action"], got.status())
	}
}

func TestStatus_KnownTicket_DropsPersonalAndInternalFields(t *testing.T) {
	got := newHarness(t, &pmfbyStub{}, 0).mustSend(t, request("status", statusAttributes()))
	allowed := map[string]bool{"@context": true, "@type": true, "ticketNo": true, "caseStatus": true,
		"grievanceCategory": true, "grievanceDescription": true, "filedOn": true, "officerReply": true, "source": true}
	for field := range got.attributes() {
		if !allowed[field] {
			t.Errorf("%s is mapped; the response is an allow-list", field)
		}
	}
}

func TestStatus_NoRemarkYet_OmitsOfficerReply(t *testing.T) {
	record := strings.Replace(knownTicketRecord, `"latestRemark":"Forwarded to insurer"`, `"latestRemark":""`, 1)
	got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes()))
	if _, present := got.attributes()["officerReply"]; present {
		t.Errorf("officerReply = %v, want it omitted while PMFBY has none", got.attributes()["officerReply"])
	}
}

func TestStatus_UnknownTicket_ReturnsTicketNumberAlone(t *testing.T) {
	attributes := statusAttributes()
	attributes["ticketNo"] = "999"
	got := newHarness(t, &pmfbyStub{}, 0).mustSend(t, request("status", attributes))

	for field := range got.attributes() {
		if field != "@context" && field != "@type" && field != "ticketNo" && field != "source" {
			t.Errorf("%s = %v on an unknown ticket, want no grievance details", field, got.attributes()[field])
		}
	}
	if got.attributes()["ticketNo"] != "999" {
		t.Errorf("ticketNo = %v, want the one asked about", got.attributes()["ticketNo"])
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

func TestStatus_PortalRefuses_Returns502(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: `{"responseCode":0,"responseMessage":"error"}`}}, 0)
	_, err := h.send(t, request("status", statusAttributes()))
	assertCoded(t, err, http.StatusBadGateway, util.CodeUpstreamUnavailable)
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

func TestFlow_InitConfirmStatus_LogsInOnce(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	h.mustSend(t, request("init", initAttributes()))
	ticket := h.mustSend(t, request("confirm", confirmAttributes())).attributes()["ticketNo"]
	got := h.mustSend(t, request("status", map[string]any{"applicantPhone": phone, "ticketNo": ticket}))

	if got.attributes()["caseStatus"] != "UNDER_REVIEW" {
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
