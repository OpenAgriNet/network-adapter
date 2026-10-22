package Grievance_test

// mappings_test.go runs the shipped PMFBY mappings through the real step,
// against a stub PMFBY that logs in, files a ticket and reads it back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	shippedBindingKey = "pmfby|openagrinet:Grievance"
	stubToken         = "stub-login-token"
	knownTicket       = "100626000099001"
	phone             = "9876543210"
	insertPath        = "/krphapi/FGMS/AddKRPHNCIPGrievenceSupportTicket"
	statusPath        = "/krphapi/FGMS/GetGrievenceTicketsStatus"
	loginPath         = "/krphapi/FGMS/NICUsersLogin"
)

// knownTicketRecord is PMFBY's record for knownTicket, personal data included.
// It carries the category by name only.
const knownTicketRecord = `{"responseCode":"1","responseMessage":"Fetched","recordCount":1,"responseDynamic":{
	"GrievenceSupportTicketID":109301,"TicketStatus":"Under Review",
	"ComplaintDate":"2026-10-04","InsertDateTime":"2026-10-04T10:00:00.000Z","ApplicationNo":"040108251010160770605",
	"GrievenceDescription":"Claim not received",
	"TicketCategoryName":"Claim","TicketSubCategoryName":"Claim not received","CropName":"Paddy",
	"FarmerName":"Ramesh","StateMasterName":"Karnataka","DistrictMasterName":"Mysuru",
	"InsuranceCompany":"Insurer"}}`

// pmfbyStub stands in for PMFBY's FGMS. Zero fields give the ordinary answer.
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
			fmt.Fprint(w, `{"responseCode":"0","responseMessage":"No record found","responseDynamic":[]}`)
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
type harness struct {
	pmfby *pmfbyStub
	step  definition.Step
}

// newHarness builds the step. statusRetries is status's retryMax (support
// gets 0); tweak edits the config first.
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
		BindingKey: shippedBindingKey, ParticipantID: "pmfby", CapabilityCode: "openagrinet:Grievance",
		BaseURL: upstream.URL,
		Actions: map[string]model.ActionPlan{
			"support": action(insertPath, "grievance.support.yaml", 0),
			"status":  action(statusPath, "grievance.status.yaml", statusRetries),
		},
	}}

	cfg := &Grievance.Config{
		BindingKeys:      []string{shippedBindingKey},
		ProviderIDAt:     "message.contract.commitments[].offer.provider.id, message.support.channels[].provider.id",
		CapabilityCodeAt: "message.contract.commitments[].commitmentAttributes.@type, message.support.channels[].@type",
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

// band reads a nested object: the grievance, the case, or its status.
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

const packContext = "https://openagrinet.github.io/network-specs/api-schemas/Grievance/v0.1/context.jsonld"

// scheme is what every PMFBY payload names.
var scheme = map[string]any{"code": "PMFBY", "name": "Pradhan Mantri Fasal Bima Yojana"}

// provider is the portal a support channel names.
var provider = map[string]any{"id": "pmfby", "descriptor": map[string]any{"name": "PMFBY Grievance Portal"}}

func becknContext(action string) map[string]any {
	return map[string]any{"version": "2.0.0", "action": action, "transactionId": "txn-1", "messageId": "msg-1"}
}

// request builds a status payload whose one commitment carries attributes.
func request(action string, attributes map[string]any) []byte {
	attributes["@context"] = packContext
	attributes["@type"] = "openagrinet:Grievance"
	attributes["informationMode"] = "OnDemand"
	if _, named := attributes["scheme"]; !named {
		attributes["scheme"] = scheme
	}
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

// support builds a support request from flat fields: categoryCode,
// subCategoryCode and description under grievance, the rest on channels[0].
func support(fields map[string]any) []byte {
	channel := map[string]any{"@context": packContext, "@type": "openagrinet:Grievance",
		"informationMode": "OnDemand", "scheme": scheme, "provider": provider}
	body := map[string]any{"channels": []any{channel}}
	grievance := map[string]any{}
	for field, value := range fields {
		switch field {
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
		"enrolmentId": "040108251010160770605", "categoryCode": "3", "subCategoryCode": "10",
		"description": "  Claim not received  ", "applicantMobile": phone,
		"cropYear": "2025", "season": "Kharif",
	}
}

func statusAttributes() map[string]any {
	return map[string]any{"applicantMobile": phone, "case": map[string]any{"ticketNo": knownTicket}}
}

// refusal is the HTTP status and Beckn error a failed request is answered with.
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

// assertRefused fails unless err is a 400 BIZ_GENERIC_ERROR carrying message.
func assertRefused(t *testing.T, err error, message string) {
	t.Helper()
	if status, refused := refusal(t, err); status != http.StatusBadRequest ||
		refused.Code != "BIZ_GENERIC_ERROR" || refused.Message != message {
		t.Errorf("error = %d %+v, want 400 BIZ_GENERIC_ERROR %q", status, refused, message)
	}
}

// assertFailedUpstream fails unless err is a 502 NET_DOWNSTREAM_UNAVAILABLE carrying message.
func assertFailedUpstream(t *testing.T, err error, message string) {
	t.Helper()
	if status, refused := refusal(t, err); status != http.StatusBadGateway ||
		refused.Code != util.CodeUpstreamUnavailable || refused.Message != message {
		t.Errorf("error = %d %+v, want 502 %s %q", status, refused, util.CodeUpstreamUnavailable, message)
	}
}

// assertJustNowUTC fails unless value is an RFC 3339 UTC instant within a minute of now.
func assertJustNowUTC(t *testing.T, value any) {
	t.Helper()
	text, _ := value.(string)
	at, err := time.Parse("2006-01-02T15:04:05Z", text)
	if err != nil || time.Since(at).Abs() > time.Minute {
		t.Errorf("filedOn = %v, want this moment as YYYY-MM-DDTHH:MM:SSZ", value)
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

	// Every value goes as a string, the season as its code.
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
	// enrolmentId and the grievance are echoed; a name beside a code is dropped.
	channel := got.channel()
	grievance := band(channel, "grievance")
	if channel["enrolmentId"] != "040108251010160770605" || len(band(grievance, "category")) != 1 ||
		band(grievance, "category")["code"] != "3" || band(grievance, "subCategory")["code"] != "10" ||
		grievance["description"] != "Claim not received" {
		t.Errorf("channel = %v, want enrolmentId and grievance echoed, the description trimmed", channel)
	}
	filed := band(channel, "case")
	status := band(filed, "status")
	assertJustNowUTC(t, filed["filedOn"])
	if filed["ticketNo"] != knownTicket || len(status) != 1 || status["code"] != "Registered" ||
		band(channel, "provider")["id"] != "pmfby" ||
		channel["informationMode"] != "Direct" || channel["scheme"] == nil {
		t.Errorf("channel = %v", channel)
	}
	for _, private := range []string{"applicantMobile", "ticketId"} {
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

// Without category codes, PMFBY's published pair is sent and echoed.
func TestSupport_NoCategoryCodes_SendsAndEchoesDefaults(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	fields := supportFields()
	delete(fields, "categoryCode")
	delete(fields, "subCategoryCode")
	got := h.mustSend(t, support(fields))
	sent := h.pmfby.bodies[insertPath]
	if sent["ticketCategoryID"] != "3" || sent["ticketSubCategoryID"] != "10" {
		t.Errorf("sent %v / %v, want 3 / 10", sent["ticketCategoryID"], sent["ticketSubCategoryID"])
	}
	grievance := band(got.channel(), "grievance")
	if band(grievance, "category")["code"] != "3" || band(grievance, "subCategory")["code"] != "10" {
		t.Errorf("grievance = %v, want the defaults echoed", grievance)
	}
}

// The pack lists provider and scheme.name as coming back; the caller may send
// only the ids.
func TestSupport_BareProviderAndScheme_ReturnsPortalNameAndSchemeName(t *testing.T) {
	fields := supportFields()
	fields["provider"] = map[string]any{"id": "pmfby", "availableAt": []any{}}
	fields["scheme"] = map[string]any{"code": "PMFBY"}
	channel := newHarness(t, &pmfbyStub{}, 0).mustSend(t, support(fields)).channel()
	if got := band(channel, "provider"); !reflect.DeepEqual(got, provider) {
		t.Errorf("provider = %v, want %v", got, provider)
	}
	if got := band(channel, "scheme"); !reflect.DeepEqual(got, scheme) {
		t.Errorf("scheme = %v, want %v", got, scheme)
	}
}

// complaintDate goes to PMFBY as its IST date and comes back as filedOn in UTC.
// complaintDate goes to PMFBY as its IST date; filedOn stays the lodge time,
// so a later status read reports the same moment.
func TestSupport_CallerDateAndSource_SentInsteadOfDefaults(t *testing.T) {
	for given, want := range map[string]string{
		"2026-09-20":                "2026-09-20",
		"2026-09-20T04:30:00Z":      "2026-09-20",
		"2026-09-21T01:00:00+05:30": "2026-09-21",
	} {
		t.Run(given, func(t *testing.T) {
			h := newHarness(t, &pmfbyStub{}, 0)
			fields := supportFields()
			fields["complaintDate"], fields["receiptSourceId"] = given, "200001"
			got := h.mustSend(t, support(fields))
			sent := h.pmfby.bodies[insertPath]
			if sent["complaintDate"] != want || sent["receiptSourceID"] != "200001" {
				t.Errorf("PMFBY was sent %v / %v, want %s and the caller's source", sent["complaintDate"], sent["receiptSourceID"], want)
			}
			assertJustNowUTC(t, band(got.channel(), "case")["filedOn"])
		})
	}
}

// "Ticket Already Raised" is a success: the existing ticket comes back.
func TestSupport_TicketAlreadyRaised_ReturnsTheExistingTicket(t *testing.T) {
	h := newHarness(t, &pmfbyStub{answers: map[string]string{insertPath: `{"responseCode":"1","responseMessage":"Ticket Already Raised for !",
		"responseDynamic":{"GrievenceSupportTicketNo":"121026000099003","GrievenceSupportTicketID":13}}`}}, 0)
	filed := band(h.mustSend(t, support(supportFields())).channel(), "case")
	if filed["ticketNo"] != "121026000099003" || band(filed, "status")["code"] != "Registered" {
		t.Errorf("case = %v, want the existing ticket, Registered", filed)
	}
	assertJustNowUTC(t, filed["filedOn"])
}

// A refusal PMFBY is known to give is a 400 in our words, never PMFBY's.
func TestSupport_KnownRefusal_Returns400InOurWords(t *testing.T) {
	for name, tc := range map[string]struct{ reply, message string }{
		"phone not matched": {`{"responseObject":null,"responseDynamic":null,"responseCode":"0","responseMessage":"Mobile Number Not Matched","jsonString":null,"recordCount":0}`,
			"The phone number does not match the one registered for this application number."},
		"no application": {`{"responseCode":"0","responseMessage":"Application Details not Exists","responseDynamic":null}`,
			"No PMFBY application was found for this application number."},
		"case and spaces": {`{"responseCode":"0","responseMessage":" mobile number NOT matched ","responseDynamic":null}`,
			"The phone number does not match the one registered for this application number."},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newHarness(t, &pmfbyStub{answers: map[string]string{insertPath: tc.reply}}, 0).send(t, support(supportFields()))
			assertRefused(t, err, tc.message)
		})
	}
}

// Any other failure is PMFBY's own: a 502 in our words, PMFBY's text withheld.
func TestSupport_PortalFails_Returns502WithoutItsText(t *testing.T) {
	for name, reply := range map[string]string{
		"success, no ticket":    `{"responseCode":"1","responseDynamic":{}}`,
		"success, blank ticket": `{"responseCode":"1","responseDynamic":{"GrievenceSupportTicketNo":""}}`,
		"no message":            `{"responseCode":"0","responseMessage":null}`,
		"internal error":        `{"responseCode":"0","responseMessage":"AxiosError: Request failed with status code 403","responseDynamic":null}`,
		"token rejected":        `{"responseCode":"0","responseMessage":"Token required!","responseDynamic":null}`,
		"unknown wording":       `{"responseCode":"0","responseMessage":"Invalid","responseDynamic":{"GrievenceSupportTicketNo":"100626000099001"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newHarness(t, &pmfbyStub{answers: map[string]string{insertPath: reply}}, 0).send(t, support(supportFields()))
			assertFailedUpstream(t, err, "PMFBY could not register the grievance right now. Please try again later.")
		})
	}
}

// A 4xx from PMFBY is its refusal.
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
	if h.pmfby.calls[insertPath] != 0 {
		t.Error("PMFBY was called for a request naming two channels")
	}
}

// Without the channel paths a support request names no binding the step reads.
func TestSupport_ChannelPathsNotConfigured_PassesThroughUntouched(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0, func(c *Grievance.Config) {
		c.ProviderIDAt = "message.contract.commitments[].offer.provider.id"
		c.CapabilityCodeAt = "message.contract.commitments[].commitmentAttributes.@type"
	})
	got, err := h.send(t, support(supportFields()))
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want the support request passed on untouched", got, err)
	}
	if len(h.pmfby.calls) != 0 {
		t.Errorf("PMFBY was called %v without a way to recognise the request", h.pmfby.calls)
	}
}

// Two channels are refused only when they name this step's binding.
func TestSupport_OtherProviderTwoChannels_PassesThrough(t *testing.T) {
	var payload map[string]any
	_ = json.Unmarshal(support(supportFields()), &payload)
	body := payload["message"].(map[string]any)["support"].(map[string]any)
	channel := body["channels"].([]any)[0].(map[string]any)
	channel["provider"] = map[string]any{"id": "someone-else"}
	body["channels"] = append(body["channels"].([]any), channel)
	raw, _ := json.Marshal(payload)

	h := newHarness(t, &pmfbyStub{}, 0)
	got, err := h.send(t, raw)
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want another provider's request passed on", got, err)
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
	// The category comes back by name, with no code.
	grievance := band(attributes, "grievance")
	category, subCategory := band(grievance, "category"), band(grievance, "subCategory")
	if len(category) != 1 || category["name"] != "Claim" || len(subCategory) != 1 ||
		subCategory["name"] != "Claim not received" || grievance["description"] != "Claim not received" {
		t.Errorf("grievance = %v, want the complaint as PMFBY holds it", grievance)
	}
	filed := band(attributes, "case")
	if attributes["enrolmentId"] != "040108251010160770605" || attributes["informationMode"] != "Direct" ||
		filed["ticketNo"] != knownTicket || filed["filedOn"] != "2026-10-04T10:00:00Z" || filed["cropName"] != "Paddy" {
		t.Errorf("commitmentAttributes = %v", attributes)
	}
	// "Under Review" is not a phrase the lookup lists, so it is UnderReview.
	if status := band(filed, "status"); len(status) != 1 || status["code"] != "UnderReview" {
		t.Errorf("case.status = %v, want code UnderReview alone", status)
	}
	if got.Context["action"] != "on_status" || got.status() != "ACTIVE" {
		t.Errorf("action %v, status %v; want on_status, ACTIVE", got.Context["action"], got.status())
	}
}

// The live record carries ids, crop year and season as numbers, and the
// officer's reply in latestRemark.
func TestStatus_FullRecord_ReturnsCodesCropYearSeasonAndReply(t *testing.T) {
	record := strings.Replace(knownTicketRecord, `"responseDynamic":{`, `"responseDynamic":{
		"TicketCategoryID":3,"TicketSubCategoryID":10,"RequestYear":2025,"RequestSeason":2,
		"latestRemark":" Claim credited on 12 Oct ",`, 1)
	attributes := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes())).attributes()

	grievance := band(attributes, "grievance")
	if category, subCategory := band(grievance, "category"), band(grievance, "subCategory"); category["code"] != "3" ||
		category["name"] != "Claim" || subCategory["code"] != "10" || subCategory["name"] != "Claim not received" {
		t.Errorf("grievance = %v, want code and name on both levels", grievance)
	}
	if attributes["cropYear"] != "2025" || attributes["season"] != "Rabi" {
		t.Errorf("cropYear, season = %v, %v; want 2025, Rabi", attributes["cropYear"], attributes["season"])
	}
	if reply := band(attributes, "case")["reply"]; reply != "Claim credited on 12 Oct" {
		t.Errorf("case.reply = %q, want the trimmed remark", reply)
	}
}

// Values PMFBY uses for "none", or that fit no pack field, are dropped.
func TestStatus_UnusableCodes_Dropped(t *testing.T) {
	for name, fields := range map[string]string{
		"zeros":        `"TicketCategoryID":0,"TicketSubCategoryID":"0","RequestYear":0,"RequestSeason":0,"latestRemark":"",`,
		"out of range": `"RequestYear":25,"RequestSeason":4,"latestRemark":"   ",`,
		"not a number": `"TicketCategoryID":"x","RequestYear":"20x5","RequestSeason":"Kharif",`,
	} {
		t.Run(name, func(t *testing.T) {
			record := strings.Replace(knownTicketRecord, `"responseDynamic":{`, `"responseDynamic":{`+fields, 1)
			attributes := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
				mustSend(t, request("status", statusAttributes())).attributes()
			_, year := attributes["cropYear"]
			_, season := attributes["season"]
			_, reply := band(attributes, "case")["reply"]
			if category := band(band(attributes, "grievance"), "category"); year || season || reply || category["code"] != nil {
				t.Errorf("attributes = %v, want no cropYear, season, reply or category code", attributes)
			}
		})
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

// InsertDateTime comes back as filedOn in UTC, whatever offset PMFBY states.
func TestStatus_InsertDateTimeWithOffset_FiledOnInUTC(t *testing.T) {
	record := strings.Replace(knownTicketRecord, `"2026-10-04T10:00:00.000Z"`, `"2026-10-04T15:30:00+05:30"`, 1)
	got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes()))
	if filed := band(got.attributes(), "case")["filedOn"]; filed != "2026-10-04T10:00:00Z" {
		t.Errorf("filedOn = %v, want 2026-10-04T10:00:00Z", filed)
	}
}

func TestStatus_BareScheme_ReturnsSchemeName(t *testing.T) {
	attributes := statusAttributes()
	attributes["scheme"] = map[string]any{"code": "PMFBY"}
	got := newHarness(t, &pmfbyStub{}, 0).mustSend(t, request("status", attributes))
	if got := band(got.attributes(), "scheme"); !reflect.DeepEqual(got, scheme) {
		t.Errorf("scheme = %v, want %v", got, scheme)
	}
}

// A missing phrase still maps to UnderReview.
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
		"null insert time":    {`"InsertDateTime":"2026-10-04T10:00:00.000Z"`, `"InsertDateTime":null`, "case", "filedOn"},
		"date-only insert":    {`"InsertDateTime":"2026-10-04T10:00:00.000Z"`, `"InsertDateTime":"2026-10-04"`, "case", "filedOn"},
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

// PMFBY's TicketStatus maps to a CaseStatusCode; an unlisted phrase is UnderReview.
func TestStatus_TicketStatus_MapsToCaseStatusCode(t *testing.T) {
	for phrase, want := range map[string]string{"Resolved": "Resolved", " resolved ": "Resolved",
		"Open": "Registered", "Under Review": "UnderReview", "Escalated": "UnderReview"} {
		t.Run(phrase, func(t *testing.T) {
			record := strings.Replace(knownTicketRecord, `"TicketStatus":"Under Review"`, `"TicketStatus":"`+phrase+`"`, 1)
			got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
				mustSend(t, request("status", statusAttributes()))
			if status := band(band(got.attributes(), "case"), "status"); len(status) != 1 || status["code"] != want {
				t.Errorf("case.status = %v, want code %s alone", status, want)
			}
		})
	}
}

// No record, or one for another ticket (PMFBY reads by phone alone), is a 400
// in our words.
func TestStatus_NoMatchingRecord_Returns400(t *testing.T) {
	another := strings.Replace(knownTicketRecord, `"responseDynamic":{`, `"responseDynamic":{"GrievenceSupportTicketNo":"100626000099002",`, 1)
	for name, reply := range map[string]string{
		"no record":       `{"responseCode":"0","responseMessage":"No record found","responseDynamic":[]}`,
		"null":            `{"responseCode":"1","responseDynamic":null}`,
		"empty object":    `{"responseCode":"1","responseDynamic":{}}`,
		"empty in list":   `{"responseCode":"1","responseDynamic":[{}]}`,
		"all fields null": `{"responseCode":"1","responseDynamic":{"ApplicationNo":null,"TicketStatus":null}}`,
		"another ticket":  another,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: reply}}, 0).
				send(t, request("status", statusAttributes()))
			assertRefused(t, err, "No grievance was found for this ticket number and phone number.")
		})
	}
}

// Any other failure is PMFBY's own: a 502 in our words, PMFBY's text withheld.
func TestStatus_PortalFails_Returns502WithoutItsText(t *testing.T) {
	for name, reply := range map[string]string{
		"no envelope":    `{"message":"An error has occurred."}`,
		"no message":     `{"responseCode":"0","responseMessage":"","responseDynamic":null}`,
		"internal error": `{"responseCode":"0","responseMessage":"AxiosError: Request failed with status code 403","responseDynamic":null}`,
		"token rejected": `{"responseCode":"0","responseMessage":"Token required!","responseDynamic":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: reply}}, 0).
				send(t, request("status", statusAttributes()))
			assertFailedUpstream(t, err, "PMFBY could not return the grievance status right now. Please try again later.")
		})
	}
}

func TestStatus_RecordNamingTheTicket_ReturnsIt(t *testing.T) {
	record := strings.Replace(knownTicketRecord, `"responseDynamic":{`, `"responseDynamic":{"GrievenceSupportTicketNo":"`+knownTicket+`",`, 1)
	got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes()))
	if no := band(got.attributes(), "case")["ticketNo"]; no != knownTicket {
		t.Errorf("ticketNo = %v, want %s", no, knownTicket)
	}
}

func TestStatus_RecordInAList_ReadAsTheRecord(t *testing.T) {
	record := strings.Replace(strings.Replace(knownTicketRecord, `"responseDynamic":{`, `"responseDynamic":[{`, 1), `}}`, `}]}`, 1)
	got := newHarness(t, &pmfbyStub{answers: map[string]string{statusPath: record}}, 0).
		mustSend(t, request("status", statusAttributes()))
	if crop := band(got.attributes(), "case")["cropName"]; crop != "Paddy" {
		t.Errorf("case = %v, want the record read from the list", band(got.attributes(), "case"))
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
	got := h.mustSend(t, request("status", map[string]any{"applicantMobile": phone, "case": map[string]any{"ticketNo": ticket}}))

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

// init included: PMFBY has no OTP, so there is no init.
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

// PM-KISAN shares the Grievance type; only the provider tells the desks apart.
func TestFlow_OtherSchemeSameType_PassesThroughUntouched(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	body := strings.Replace(string(request("status", statusAttributes())), `"id":"pmfby"`, `"id":"pmkisan"`, 1)
	if !strings.Contains(body, `"id":"pmkisan"`) {
		t.Fatal("the request names no pmfby provider to replace")
	}
	got, err := h.send(t, []byte(body))
	if err != nil || got != nil || len(h.pmfby.calls) != 0 {
		t.Errorf("Run() = %v, %v, PMFBY calls %v; want PM-KISAN's request passed on untouched", got, err, h.pmfby.calls)
	}
}

func TestFlow_OtherCapability_PassesThroughUntouched(t *testing.T) {
	h := newHarness(t, &pmfbyStub{}, 0)
	body := strings.Replace(string(request("status", statusAttributes())),
		"openagrinet:Grievance", "openagrinet:SchemeApplication", 1)
	got, err := h.send(t, []byte(body))
	if err != nil || got != nil {
		t.Errorf("Run() = %v, %v; want another capability passed on untouched", got, err)
	}
	if len(h.pmfby.calls) != 0 {
		t.Errorf("PMFBY was called %v for another capability", h.pmfby.calls)
	}
}
