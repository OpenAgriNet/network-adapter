package publish

// run_execute_test.go covers the half of a run that run_test.go deliberately
// stops short of: everything after the schedule says "go". It drives Run ->
// execute against a FAKE Agmarknet that speaks the raw upstream JSON the
// embedded mappings actually read, so the whole chain -- token, state list,
// master markets, per-state rows, join, verdict, build, write -- runs with no
// network and no live credentials.
//
// The fixture bodies below are RAW UPSTREAM SHAPES (agm_state_code, mkt_name,
// cmdt_details and so on), not this package's Go field names. That is the
// point: a mapping edit that renames a field it reads has to fail here rather
// than at midnight against the live service.
//
// The frame's own decisions -- the registry gate, the run log, the schedule --
// are tested in internal/pipeline against a fake collector. What is here is
// what only this pipeline can prove: that ITS mappings read the upstream's
// real field names.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// publishingRecord is the live registry's answer for this capability, in the
// shape the frame's gate reads.
func publishingRecord() *model.ProviderRecord {
	return &model.ProviderRecord{
		BindingKey: "agmarknet|" + Capability,
		Actions: map[string]model.ActionPlan{
			"select":  {Method: "GET", Path: "/v1/fetch"},
			"publish": {Mappings: RegistryPipelinePath},
		},
	}
}

// fakeRunLog records what a run asked of it, so a test can prove a run that
// did not finish also did not claim to have happened.
type fakeRunLog struct {
	last     time.Time
	recorded []time.Time
}

func (f *fakeRunLog) LastPipelineRun(context.Context, string) (time.Time, error) {
	return f.last, nil
}

func (f *fakeRunLog) RecordPipelineRun(_ context.Context, _ string, at time.Time) error {
	f.recorded = append(f.recorded, at)
	return nil
}

// upstreamState is one state as the fake upstream knows it: how it appears in
// the master state list, and how it answers that state's rows call.
//
// status is what makes this fixture worth having: the three answers a state
// can give -- rows, "no data", and a genuine failure -- are the three cases
// execute must tell apart, and only the first two may leave the run healthy.
type upstreamState struct {
	code   string
	name   string
	status int    // 0 means 200 OK
	rows   string // the raw body this state's rows call returns
}

// The raw market-commodity rows for two states. Maharashtra carries two
// markets and one of them trades two commodities, because the mapping wraps
// both the outer and the inner list for exactly that reason.
const (
	rowsMH = `[
  {"market_id":101,"mkt_name":"Pune","state_code":"MH","state_name":"Maharashtra",
   "district_id":501,"district_name":"Pune ",
   "cmdt_details":[{"cmdt_id":23,"cmdt_name":"Onion"},{"cmdt_id":24,"cmdt_name":"Potato"}]},
  {"market_id":102,"mkt_name":"Nashik","state_code":"MH","state_name":"Maharashtra",
   "district_id":502,"district_name":"Nashik",
   "cmdt_details":[{"cmdt_id":23,"cmdt_name":"Onion"}]}
]`

	rowsKA = `[
  {"market_id":201,"mkt_name":"Hubli","state_code":"KA","state_name":"Karnataka",
   "district_id":601,"district_name":"Dharwad",
   "cmdt_details":[{"cmdt_id":23,"cmdt_name":"Onion"}]}
]`

	// The upstream's own way of saying "this state traded nothing", measured
	// against the live service: an HTTP 400 whose body is not an error.
	noDataBody = `{"success":false,"message":"No data available."}`

	// Master data option 6. Nashik's null coordinates are not padding: 1176 of
	// 3310 real rows arrive that way, and a run that quietly turned them into
	// 0,0 would still produce a perfectly valid catalog.
	masterMarketsBody = `[
  {"market_id":101,"market_name":"Pune","state_name":"Maharashtra","district_name":"Pune ",
   "market_latitude":"18.5204","market_longitude":"73.8567"},
  {"market_id":102,"market_name":"Nashik","state_name":"Maharashtra","district_name":"Nashik",
   "market_latitude":null,"market_longitude":null},
  {"market_id":201,"market_name":"Hubli","state_name":"Karnataka","district_name":"Dharwad",
   "market_latitude":"15.3647","market_longitude":"75.1240"}
]`
)

// pricesByPair is how the fake price call answers each (marketcode,
// commoditycode) the Direct half asks about, in the upstream's raw shape. A
// pair not listed answers "No data available.", which is the upstream's way
// of saying the pair traded nothing in the window.
//
// Pune onion carries three rows on purpose: two days, and two varieties on
// the newer day. The record must be the newer day's HIGHER modal price.
// Nashik onion's minimum is the "NR" marker the upstream writes for an
// unreported price: the row is still usable, the minimum is simply absent.
var pricesByPair = map[string]string{
	"101|23": `[
  {"Grade":"FAQ","Group":"Vegetables","State":"Maharashtra","Market":"Pune ","Variety":"Red",
   "District":"Pune","Commodity":"Onion","Max Price":"2400","Min Price":"1500",
   "Price Unit":"Rs./Qtl","Modal Price":"2000","Arrival Date":"28-08-2026"},
  {"Grade":"FAQ","Group":"Vegetables","State":"Maharashtra","Market":"Pune ","Variety":"Local",
   "District":"Pune","Commodity":"Onion","Max Price":"3000","Min Price":"2000",
   "Price Unit":"Rs./Qtl","Modal Price":"2600","Arrival Date":"01-09-2026"},
  {"Grade":"FAQ","Group":"Vegetables","State":"Maharashtra","Market":"Pune ","Variety":"Red",
   "District":"Pune","Commodity":"Onion","Max Price":"3600","Min Price":"2200",
   "Price Unit":"Rs./Qtl","Modal Price":"3100","Arrival Date":"01-09-2026"}
]`,
	"102|23": `[
  {"Grade":"Non-FAQ","Group":"Vegetables","State":"Maharashtra","Market":"Nashik","Variety":"Other",
   "District":"Nashik","Commodity":"Onion","Max Price":"2800","Min Price":"NR",
   "Price Unit":"Rs./Qtl","Modal Price":"2500","Arrival Date":"30-08-2026"}
]`,
	"201|23": `[
  {"Grade":"FAQ","Group":"Vegetables","State":"Karnataka","Market":"Hubli","Variety":"Bellary",
   "District":"Dharwad","Commodity":"Onion","Max Price":"2200","Min Price":"1200",
   "Price Unit":"Rs./Qtl","Modal Price":"1800","Arrival Date":"31-08-2026"}
]`,
}

// districtByMarket is the district each fixture market is in, so the fake can
// refuse a price call carrying the wrong district code.
var districtByMarket = map[string]string{"101": "501", "102": "502", "201": "601", "1001": "9001"}

// twoGoodStates is the healthy fixture every case below starts from and then
// breaks in one specific way.
func twoGoodStates() []upstreamState {
	return []upstreamState{
		{code: "MH", name: "Maharashtra", rows: rowsMH},
		{code: "KA", name: "Karnataka", rows: rowsKA},
	}
}

// fakeAgmarknet serves the four upstream interactions a run makes.
//
// The paths below are written out rather than taken from Go constants,
// because there are no Go constants any more: they are declared in
// agmarknet.yaml. If the file's paths and these diverge, this
// fixture stops matching and the test fails -- which is exactly the coupling
// worth having.
//
// masterStatesBody is rendered from the fixture rather than written out, so a
// case can add or remove a state in one place and the state list, the rows
// router and the expected catalog count all move together.
func fakeAgmarknet(t *testing.T, states []upstreamState) *httptest.Server {
	t.Helper()
	return fakeAgmarknetWith(t, states, defaultFixture)
}

// upstreamFixture is the data the fake serves besides the state list and the
// per-state rows: the master markets (option 6), the price rows per
// "marketId|commodityCode" pair, and the district each market is in.
type upstreamFixture struct {
	master    string
	prices    map[string]string
	districts map[string]string
}

// defaultFixture is the edge-case data most tests here assert against.
var defaultFixture = upstreamFixture{master: masterMarketsBody, prices: pricesByPair, districts: districtByMarket}

func fakeAgmarknetWith(t *testing.T, states []upstreamState, fixture upstreamFixture) *httptest.Server {
	t.Helper()

	byCode := make(map[string]upstreamState, len(states))
	list := make([]map[string]any, 0, len(states))
	for _, state := range states {
		byCode[state.code] = state
		list = append(list, map[string]any{
			"agm_state_code": state.code,
			"state_name":     state.name,
		})
	}
	masterStatesBody, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshalling the fake state list: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every data call carries the exchanged token, put there by the
		// engine: no mapping is given it any more.
		if r.Method == http.MethodGet && r.URL.Query().Get("token") != "tok-fake" {
			t.Errorf("GET %s carried token %q, want the exchanged tok-fake", r.URL.Path, r.URL.Query().Get("token"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/generate-dynamic-token-agmarknet":
			_, _ = w.Write([]byte(`{"token":"tok-fake"}`))

		case r.URL.Path == "/v1/fetch-agmarknet-master-data":
			// The state list and the master market list are the SAME path and
			// are told apart only by the option the mapping sends -- 4 for the
			// states the run loops over, 6 for every market's coordinates.
			// Swapping the two mappings would leave both calls succeeding and
			// the output empty, so the fake refuses to guess.
			switch option := r.URL.Query().Get("option"); option {
			case "4":
				_, _ = w.Write(masterStatesBody)
			case "6":
				_, _ = w.Write([]byte(fixture.master))
			default:
				t.Errorf("master data called with option=%q, want 4 or 6", option)
				http.Error(w, `{"error":"Option must be between 1 and 6"}`, http.StatusBadRequest)
			}

		case r.URL.Path == "/v1/fetch-agmarknet-market-commodity-mapping":
			code := r.URL.Query().Get("statecode")
			state, ok := byCode[code]
			if !ok {
				t.Errorf("rows requested for statecode %q, which is not in the state list", code)
				http.Error(w, `{"error":"unknown state"}`, http.StatusBadRequest)
				return
			}
			if state.status != 0 {
				http.Error(w, state.rows, state.status)
				return
			}
			_, _ = w.Write([]byte(state.rows))

		case r.URL.Path == "/v1/fetch-agmarknet-vistaar":
			// The price call names one market, its district and one
			// commodity. Every parameter is checked, because a mapping that
			// sent the wrong code would still get an answer from the real
			// service -- just not the one it meant.
			q := r.URL.Query()
			market, commodity := q.Get("marketcode"), q.Get("commoditycode")
			if want := fixture.districts[market]; q.Get("districtcode") != want {
				t.Errorf("price call for market %s sent districtcode %q, want %q", market, q.Get("districtcode"), want)
			}
			for _, name := range []string{"token", "statecode", "from_date", "to_date"} {
				if q.Get(name) == "" {
					t.Errorf("price call for %s|%s sent no %s", market, commodity, name)
				}
			}
			body, ok := fixture.prices[market+"|"+commodity]
			if !ok {
				http.Error(w, noDataBody, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(body))

		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeUpstreamEnv points the run at the fake and supplies the credentials the
// run refuses to start without. CATALOG_PUBLISH_URL is present so that a refusal
// to publish is unambiguously about the collection and not about a missing
// address.
func fakeUpstreamEnv(baseURL string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch name {
		case "MANDI_API_URI":
			return baseURL, true
		case "MANDI_TOKEN_USER":
			return "test-user", true
		case "MANDI_TOKEN_SECRET":
			return "test-secret", true
		case "CATALOG_PUBLISH_URL":
			return "http://publish.invalid", true
		case "MANDI_PARTICIPANT_ID":
			return "test-participant", true
		case "APP_NETWORK_ID":
			return "test-network", true
		case "MANDI_PUBLISH_MODE":
			// The cases in this file are about the OnDemand half and predate
			// the Direct one; they keep its meaning. modeEnv switches mode.
			return "onDemand", true
		}
		return "", false
	}
}

// modeEnv is fakeUpstreamEnv with the publish mode set.
func modeEnv(baseURL, mode string) func(string) (string, bool) {
	env := fakeUpstreamEnv(baseURL)
	return func(name string) (string, bool) {
		if name == "MANDI_PUBLISH_MODE" {
			return mode, true
		}
		return env(name)
	}
}

// firingTime is an instant the pipeline's midnight-IST schedule has already
// fired for, so a run with a never-used run log is due.
func firingTime(t *testing.T) time.Time {
	t.Helper()
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	return time.Date(2026, 9, 21, 6, 0, 0, 0, ist)
}

// TestRunBuildsCatalogsFromAFakeUpstream is the whole happy path: two
// states, three markets, catalogs on disk, one run recorded.
//
// Nothing is published -- Publish defaults to false -- so what is asserted is
// the artefact a reviewer would look at before allowing a publish.
func TestRunBuildsCatalogsFromAFakeUpstream(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	outDir := t.TempDir()
	runLog := &fakeRunLog{}
	now := firingTime(t)

	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record:   publishingRecord(),
		Pipeline: Pipeline(),
		RunLog:   runLog,
		Lookup:   fakeUpstreamEnv(upstream.URL),
		Now:      now,
		OutDir:   outDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Due {
		t.Fatalf("not due: %s", report.Reason)
	}

	if report.Counters["step:states"] != 2 {
		t.Errorf("States = %d, want 2", report.Counters["step:states"])
	}
	if report.Counters["emptyGroups"] != 0 {
		t.Errorf("emptyGroups = %d, want 0; every state answered with rows", report.Counters["emptyGroups"])
	}
	// Three rows across two states, none deduplicated away.
	if report.Counters["step:dedupe"] != 3 {
		t.Errorf("Markets = %d, want 3", report.Counters["step:dedupe"])
	}
	if len(report.Catalogs) != 2 {
		t.Fatalf("Catalogs = %d, want one per state", len(report.Catalogs))
	}
	// Nashik's null coordinates must survive the join as "no geometry" rather
	// than as 0,0. If the master market list were never fetched, or joined on
	// the wrong key, every market would land here instead of just this one.
	// Nashik has no upstream coordinate, so it must still be PUBLISHED and
	// must cost nothing against the geometry budget. The annotation that used
	// to record this is gone with the quality vocabulary; what matters is the
	// behaviour, so assert the behaviour: three markets in, three published,
	// and the one without a point carries no Point in the document.
	if report.Counters["published"] != 3 {
		t.Errorf("published = %d, want 3 (a market with no coordinate is still published)",
			report.Counters["published"])
	}
	// Not outDir itself: each pipeline gets its OWN subdirectory beneath the
	// configured one, because the stale sweep and the publish glob both work
	// by filename prefix and two pipelines sharing a directory would delete
	// and then republish each other's catalogs.
	wantDir := filepath.Join(outDir, "openagrinet-MandiPrice")
	if report.OutDir != wantDir {
		t.Errorf("OutDir = %q, want %q", report.OutDir, wantDir)
	}

	// The files, not just the in-memory report: the write step is what the
	// publish step reads back, so a run that built two catalogs and wrote
	// none would look successful everywhere except here.
	for _, slug := range []string{"MH", "KA"} {
		path := filepath.Join(wantDir, "mandi-price-"+slug+".json")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the catalog written for %s: %v", slug, err)
		}
		doc := decodeCatalog(t, content)
		if len(doc.Message.Catalogs) != 1 {
			t.Fatalf("%s: catalogs = %d, want 1", slug, len(doc.Message.Catalogs))
		}
		if got, want := doc.Message.Catalogs[0].ID, "catalog:mandi-price:"+slug; got != want {
			t.Errorf("%s: catalog id = %q, want %q", slug, got, want)
		}
		wantResources := map[string]int{"MH": 2, "KA": 1}[slug]
		if got := len(doc.Message.Catalogs[0].Resources); got != wantResources {
			t.Errorf("%s: resources = %d, want %d", slug, got, wantResources)
		}
	}

	// A completed run is recorded exactly once, at the instant it was judged
	// against -- that record is the only thing standing between a restart and
	// a second publish.
	if len(runLog.recorded) != 1 {
		t.Fatalf("recorded %d runs, want exactly 1", len(runLog.recorded))
	}
	if !runLog.recorded[0].Equal(now) {
		t.Errorf("recorded run at %s, want %s", runLog.recorded[0], now)
	}
}

// TestRunCountsANoDataStateAsEmptyRatherThanBroken is the case this whole
// classification exists for.
//
// "No data available." means the state traded nothing -- a Sunday does it to
// all 36 at once -- so it must be counted and skipped, not failed. Collapsing
// it into a generic error once turned 27 quiet states into 27 reported
// outages, and the states that DID trade must still publish.
func TestRunCountsANoDataStateAsEmptyRatherThanBroken(t *testing.T) {
	states := twoGoodStates()
	states[1] = upstreamState{
		code: "KA", name: "Karnataka",
		status: http.StatusBadRequest, rows: noDataBody,
	}

	upstream := fakeAgmarknet(t, states)
	outDir := t.TempDir()
	runLog := &fakeRunLog{}

	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record:   publishingRecord(),
		Pipeline: Pipeline(),
		RunLog:   runLog,
		Lookup:   fakeUpstreamEnv(upstream.URL),
		Now:      firingTime(t),
		OutDir:   outDir,
	})
	if err != nil {
		t.Fatalf("a quiet state failed the run: %v", err)
	}

	if report.Counters["emptyGroups"] != 1 {
		t.Errorf("emptyGroups = %d, want 1", report.Counters["emptyGroups"])
	}
	// excludedGroups counts a DIFFERENT thing: a group that returned
	// markets but had none worth publishing. KA returned nothing at all, so it
	// never reached the build and must not be counted here too. Collapsing the
	// two would report one number for "the upstream was quiet" and "we threw
	// everything away", which call for opposite responses.
	if report.Counters["excludedGroups"] != 0 {
		t.Errorf("excludedGroups = %d, want 0; a state the upstream had no data for "+
			"never reached the build", report.Counters["excludedGroups"])
	}
	if report.Counters["step:states"] != 2 {
		t.Errorf("States = %d, want 2; a quiet state is still a state", report.Counters["step:states"])
	}
	// The trading state is unaffected: an empty neighbour must not cost
	// Maharashtra its catalog.
	if report.Counters["step:dedupe"] != 2 {
		t.Errorf("Markets = %d, want 2 (Maharashtra's rows only)", report.Counters["step:dedupe"])
	}
	if len(report.Catalogs) != 1 || report.Catalogs[0].Slug != "MH" {
		t.Fatalf("Catalogs = %+v, want one for MH", report.Catalogs)
	}
	if _, err := os.Stat(filepath.Join(outDir, "openagrinet-MandiPrice", "mandi-price-KA.json")); !os.IsNotExist(err) {
		t.Error("a state that traded nothing produced a catalog; publishing it would retire its markets")
	}
	if len(runLog.recorded) != 1 {
		t.Errorf("recorded %d runs, want 1: a quiet state is a successful run", len(runLog.recorded))
	}
}

// TestRunRefusesToPublishWhenAStateFailedForARealReason is the other half of
// that distinction.
//
// A 500 is not "this state traded nothing", so the state's catalog is
// missing rather than empty. Publishing the rest would MERGE a partial picture
// over the network's current one, so execute's groupErrors count has to reach
// publishCatalogs and stop it -- and the failed run must not be recorded, or
// the next tick would skip the retry.
func TestRunRefusesToPublishWhenAStateFailedForARealReason(t *testing.T) {
	// Four failed states: one more than the file tolerates.
	states := twoGoodStates()
	states[1] = brokenState("KA", "Karnataka")
	states = append(states, brokenState("GJ", "Gujarat"), brokenState("RJ", "Rajasthan"), brokenState("PB", "Punjab"))

	upstream := fakeAgmarknet(t, states)
	runLog := &fakeRunLog{}

	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record:   publishingRecord(),
		Pipeline: Pipeline(),
		RunLog:   runLog,
		Lookup:   fakeUpstreamEnv(upstream.URL),
		Now:      firingTime(t),
		OutDir:   t.TempDir(),
		Publish:  true,
		// Must never be reached: the refusal comes before any send.
		Publisher: refusingPublisher{t},
	})
	if err == nil {
		t.Fatal("a collection missing a whole state was published as though it were whole")
	}
	if !strings.Contains(err.Error(), "refusing to publish") {
		t.Errorf("error %q does not report the refuseWhen rule as the reason", err)
	}
	// A failure must never be filed as a quiet state; that is the miscount the
	// no-data classification exists to prevent, in the opposite direction.
	if report.Counters["emptyGroups"] != 0 {
		t.Errorf("emptyGroups = %d, want 0; a 500 is a failure, not an empty state", report.Counters["emptyGroups"])
	}
	// The healthy state was still built -- the refusal is about sending, not
	// about collecting -- so an operator can see what would have gone out.
	if len(report.Catalogs) != 1 {
		t.Errorf("Catalogs = %d, want 1 (Maharashtra was collected fine)", len(report.Catalogs))
	}
	if len(runLog.recorded) != 0 {
		t.Error("a refused publish was recorded as a completed run; the day's retry is now lost")
	}
	if report.Errors != 4 {
		t.Errorf("groupErrors = %d, want 4", report.Errors)
	}
}

// brokenState is a state whose rows call fails for a real reason.
func brokenState(code, name string) upstreamState {
	return upstreamState{code: code, name: name,
		status: http.StatusInternalServerError, rows: `{"error":"upstream is down"}`}
}

// acceptingPublisher accepts everything it is sent and counts it.
type acceptingPublisher struct{ published, retired int }

func (p *acceptingPublisher) Publish(context.Context, string, []byte) pipeline.Outcome {
	p.published++
	return pipeline.Outcome{Status: pipeline.StatusPublished}
}

func (p *acceptingPublisher) Retire(_ context.Context, _ string, r pipeline.Retirement) pipeline.Outcome {
	p.retired++
	return pipeline.Outcome{CatalogID: r.CatalogID, Status: pipeline.StatusPublished}
}

// One failed state is within the tolerance: the healthy states still reach
// the network, and the failure is still counted in the report.
func TestRunPublishesTheRestWhenOneStateFailed(t *testing.T) {
	states := twoGoodStates()
	states[1] = brokenState("KA", "Karnataka")
	upstream := fakeAgmarknet(t, states)
	publisher := &acceptingPublisher{}

	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: fakeUpstreamEnv(upstream.URL), Now: firingTime(t), OutDir: t.TempDir(),
		Publish: true, Publisher: publisher,
	})
	if err != nil {
		t.Fatalf("one failed state refused the whole publish: %v", err)
	}
	if publisher.published != 1 || report.Errors != 1 {
		t.Errorf("published %d catalogs with %d group errors; want Maharashtra published and 1 error counted",
			publisher.published, report.Errors)
	}
}

// Review Focus 1: with no default, a deployment that never set its identity
// is refused before anything is fetched -- never published under an empty id.
func TestRunRefusesWithoutAParticipantID(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	env := fakeUpstreamEnv(upstream.URL)
	_, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: func(name string) (string, bool) {
			if name == "MANDI_PARTICIPANT_ID" {
				return "", false
			}
			return env(name)
		},
		Now: firingTime(t), OutDir: t.TempDir(),
		Publish: true, Publisher: refusingPublisher{t},
	})
	if err == nil || !strings.Contains(err.Error(), "participantId") || !strings.Contains(err.Error(), "MANDI_PARTICIPANT_ID") {
		t.Fatalf("err = %v; want a refusal naming participantId and MANDI_PARTICIPANT_ID", err)
	}
}

// With publishing off the refusal above never fires, so a state that failed
// for a real reason would otherwise show up only as a missing catalog --
// indistinguishable from a state that traded nothing. The count has to be in
// the report itself.
func TestRunReportsAStateFailureEvenWhenNotPublishing(t *testing.T) {
	states := twoGoodStates()
	states[1] = upstreamState{
		code: "KA", name: "Karnataka",
		status: http.StatusInternalServerError, rows: `{"error":"upstream is down"}`,
	}

	upstream := fakeAgmarknet(t, states)
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record:   publishingRecord(),
		Pipeline: Pipeline(),
		Lookup:   fakeUpstreamEnv(upstream.URL),
		Now:      firingTime(t),
		OutDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("a build-only run failed: %v", err)
	}
	if report.Errors != 1 {
		t.Errorf("StateErrors = %d, want 1; a failed state is invisible in this report", report.Errors)
	}
	if report.Counters["emptyGroups"] != 0 {
		t.Errorf("emptyGroups = %d, want 0; a 500 is not a quiet state", report.Counters["emptyGroups"])
	}
}

// TestRunFailsWhenTheUpstreamNamesNoStates: an empty state list is not an
// empty day. Walking it would publish nothing, report success, and read as
// "India has no markets today" -- a silent outage rather than a loud one.
func TestRunFailsWhenTheUpstreamNamesNoStates(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
	}{
		// The mapping's $map over an empty array yields an empty array, so
		// this reaches fetchStates as a well-formed list of nothing.
		"an empty array": {body: `[]`},
		// An upstream error object, which the client refuses before the
		// mapping can turn it into one phantom state.
		"an error object": {body: `{"message":"no data found"}`},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"token":"tok-fake"}`))
					return
				}
				if r.URL.Path == "/v1/fetch-agmarknet-market-commodity-mapping" {
					t.Error("rows were fetched although no state list was returned")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			runLog := &fakeRunLog{}
			_, err := pipeline.Run(context.Background(), pipeline.RunOptions{
				Record:   publishingRecord(),
				Pipeline: Pipeline(),
				RunLog:   runLog,
				Lookup:   fakeUpstreamEnv(upstream.URL),
				Now:      firingTime(t),
				OutDir:   t.TempDir(),
			})
			if err == nil {
				t.Fatal("a run that collected no states reported success")
			}
			if len(runLog.recorded) != 0 {
				t.Error("a run that collected nothing was recorded as having run")
			}
		})
	}
}

// renderedCatalog is the part of a rendered catalog these tests assert on.
// Carried over from the deleted catalog_test.go, because what it checks --
// that the document really carries the ids and resources the file describes --
// did not stop mattering when the building moved into the frame.
type renderedCatalog struct {
	Context struct {
		TransactionID string `json:"transactionId"`
		MessageID     string `json:"messageId"`
		Timestamp     string `json:"timestamp"`
	} `json:"context"`
	Message struct {
		Catalogs []struct {
			ID         string `json:"id"`
			Descriptor struct {
				Name string `json:"name"`
			} `json:"descriptor"`
			Resources []struct {
				ID         string `json:"id"`
				Descriptor struct {
					Name string `json:"name"`
				} `json:"descriptor"`
			} `json:"resources"`
		} `json:"catalogs"`
	} `json:"message"`
}

func decodeCatalog(t *testing.T, content []byte) renderedCatalog {
	t.Helper()
	var out renderedCatalog
	if err := json.Unmarshal(content, &out); err != nil {
		t.Fatalf("unmarshal rendered catalog: %v\nbody: %s", err, content)
	}
	return out
}

// refusingPublisher fails the test if anything is published through it.
type refusingPublisher struct{ t *testing.T }

func (p refusingPublisher) Publish(context.Context, string, []byte) pipeline.Outcome {
	p.t.Error("a catalog was published although the run should have refused")
	return pipeline.Outcome{Status: pipeline.StatusPublished}
}

func (p refusingPublisher) Retire(_ context.Context, _ string, r pipeline.Retirement) pipeline.Outcome {
	p.t.Error("a catalog was retired although the run should have refused")
	return pipeline.Outcome{CatalogID: r.CatalogID, Status: pipeline.StatusPublished}
}

// The rendered catalog's contract, from a real run:
//   - an ISO 3166-2 area only for a state the table knows (Review Focus 2);
//     an unknown state gets none, never a guessed "IN-<code>";
//   - market.district carries the district's NAME, and neither districtId nor
//     districtName appears: market's properties are a closed set in the
//     MandiPrice schema, and the publish endpoint rejects the whole catalog
//     over a property outside it (measured: "property \"districtId\" is
//     unsupported", every catalog 400);
//   - resource validity carries the schedule's offset (+05:30), not Z;
//   - the catalog itself carries no validity (see the listing-validity test).
func TestRunRendersTheCatalogContract(t *testing.T) {
	states := append(twoGoodStates(), upstreamState{code: "ZZ", name: "Nowhere", rows: `[
  {"market_id":1001,"mkt_name":"Nowhere Market","state_code":"ZZ","state_name":"Nowhere",
   "district_id":9001,"district_name":"Nowhere District",
   "cmdt_details":[{"cmdt_id":23,"cmdt_name":"Onion"}]}
]`})
	upstream := fakeAgmarknet(t, states)
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: fakeUpstreamEnv(upstream.URL), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantISO := map[string]string{"MH": "IN-MH", "KA": "IN-KA", "ZZ": ""}
	for _, catalog := range report.Catalogs {
		var doc map[string]any
		if err := json.Unmarshal(catalog.Content, &doc); err != nil {
			t.Fatalf("%s: %v", catalog.Slug, err)
		}
		entry := doc["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)
		for _, raw := range entry["resources"].([]any) {
			attrs := raw.(map[string]any)["resourceAttributes"].(map[string]any)
			market := attrs["market"].(map[string]any)
			_, hasID := market["districtId"]
			_, hasName := market["districtName"]
			if market["district"] == nil || hasID || hasName {
				t.Errorf("%s: market = %v; want district (the name), and no districtId/districtName -- "+
					"the schema's market is a closed set and /publish 400s on anything else", catalog.Slug, market)
			}
			iso := ""
			for _, area := range attrs["coverageAreas"].([]any) {
				if a := area.(map[string]any); a["codeScheme"] == "ISO-3166-2" {
					iso = a["areaCode"].(string)
				}
			}
			if iso != wantISO[catalog.Slug] {
				t.Errorf("%s: ISO area = %q, want %q", catalog.Slug, iso, wantISO[catalog.Slug])
			}
			if start := attrs["validity"].(map[string]any)["startsAt"].(string); !strings.HasSuffix(start, "+05:30") {
				t.Errorf("%s: resource validity.startsAt = %q, want +05:30", catalog.Slug, start)
			}
		}
	}
	if len(report.Catalogs) != 3 {
		t.Errorf("built %d catalogs, want 3 (MH, KA, ZZ)", len(report.Catalogs))
	}
}

// A published resource must be valid WHEN it is published, and stay valid until
// the next daily run lands: discovery hides a RESOURCE past its validity (tested
// 2026-10-06, dev_docs/mandi-catalog-contents-and-reasons.md section 9), and the
// price window this pipeline queries is yesterday, so a validity copied from
// that window is already over at publish. The CATALOG carries no validity: it
// hid nothing in that test, and it is optional in the pack.
func TestPublishedListingIsValidFromTheRunUntilAfterTheNextOne(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	now := firingTime(t)
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: fakeUpstreamEnv(upstream.URL), Now: now, OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	check := func(where, from, to string) {
		t.Helper()
		start, err1 := time.Parse(time.RFC3339, from)
		end, err2 := time.Parse(time.RFC3339, to)
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: validity %q..%q is not RFC 3339", where, from, to)
		}
		if start.After(now) || !end.After(now.Add(24*time.Hour)) {
			t.Errorf("%s: validity %s..%s; want it to start by the run (%s) and last beyond the next daily run",
				where, from, to, now.Format(time.RFC3339))
		}
	}
	for _, catalog := range report.Catalogs {
		var doc map[string]any
		if err := json.Unmarshal(catalog.Content, &doc); err != nil {
			t.Fatalf("%s: %v", catalog.Slug, err)
		}
		entry := doc["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)
		if _, has := entry["validity"]; has {
			t.Errorf("%s: the catalog carries a validity; only its resources should", catalog.Slug)
		}
		for _, raw := range entry["resources"].([]any) {
			rv := raw.(map[string]any)["resourceAttributes"].(map[string]any)["validity"].(map[string]any)
			check(catalog.Slug+" resource", rv["startsAt"].(string), rv["endsAt"].(string))
		}
	}
}

// catalogBySlug decodes each built catalog's single catalog entry, by slug.
func catalogBySlug(t *testing.T, report pipeline.RunReport) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, catalog := range report.Catalogs {
		var doc map[string]any
		if err := json.Unmarshal(catalog.Content, &doc); err != nil {
			t.Fatalf("%s: %v", catalog.Slug, err)
		}
		out[catalog.Slug] = doc["message"].(map[string]any)["catalogs"].([]any)[0].(map[string]any)
	}
	return out
}

// resourcesByID indexes a decoded catalog entry's resources by id.
func resourcesByID(entry map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, raw := range entry["resources"].([]any) {
		resource := raw.(map[string]any)
		out[resource["id"].(string)] = resource
	}
	return out
}

// mode=both builds, from ONE run, each state's OnDemand catalog exactly as
// before and beside it a Direct catalog of latest prices, one resource per
// (market, commodity) that reported a price in the window.
func TestRunBuildsBothModesFromOneRun(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(upstream.URL, "both"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	catalogs := catalogBySlug(t, report)
	var slugs []string
	for slug := range catalogs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	// Direct catalogs are one per state (MH-current), beside the OnDemand one
	// (MH); a state over the budget would continue as MH-current-2.
	if got, want := strings.Join(slugs, ","), "KA,KA-current,MH,MH-current"; got != want {
		t.Fatalf("slugs = %s, want %s", got, want)
	}

	// The OnDemand catalogs keep their ids and their market resources.
	if got := catalogs["MH"]["id"]; got != "catalog:mandi-price:MH" {
		t.Errorf("MH id = %v", got)
	}
	if _, ok := resourcesByID(catalogs["MH"])["resource:mandi-price:market:101"]; !ok {
		t.Error("the OnDemand MH catalog lost its market resource for Pune")
	}

	direct := catalogs["MH-current"]
	if got := direct["id"]; got != "catalog:mandi-price:MH-current" {
		t.Errorf("Direct MH id = %v", got)
	}
	if got := direct["descriptor"].(map[string]any)["code"]; got != "MANDI_PRICE_CURRENT_MH" {
		t.Errorf("Direct MH descriptor.code = %v", got)
	}
	// The catalog carries no validity of its own; its resources do.
	if _, has := direct["validity"]; has {
		t.Error("the Direct catalog carries a catalog-level validity")
	}

	// Pune onion priced, Pune potato reported nothing; Nashik is in the same
	// catalog.
	resources := resourcesByID(direct)
	if len(resources) != 2 {
		t.Fatalf("Direct MH resources = %d (%v), want 2", len(resources), resources)
	}
	if report.Counters["emptyPrices"] != 1 {
		t.Errorf("emptyPrices = %d, want 1 (Pune potato)", report.Counters["emptyPrices"])
	}

	pune, ok := resources["resource:mandi-price:price:101:23"]
	if !ok {
		t.Fatalf("no Direct resource for Pune onion; have %v", resources)
	}
	attrs := pune["resourceAttributes"].(map[string]any)
	if attrs["informationMode"] != "Direct" {
		t.Errorf("informationMode = %v, want Direct", attrs["informationMode"])
	}
	// The latest day, and on that day the higher modal price.
	if attrs["arrivalDate"] != "2026-09-01" || attrs["variety"] != "Red" {
		t.Errorf("Pune onion picked %v / %v; want the 2026-09-01 Red row", attrs["arrivalDate"], attrs["variety"])
	}
	prices := attrs["prices"].(map[string]any)
	if prices["modal"] != 3100.0 || prices["minimum"] != 2200.0 || prices["maximum"] != 3600.0 {
		t.Errorf("Pune onion prices = %v; want min 2200, max 3600, modal 3100 as numbers", prices)
	}
	if prices["currency"] != "INR" || prices["unit"] != "quintal" {
		t.Errorf("Pune onion currency/unit = %v/%v, want INR/quintal", prices["currency"], prices["unit"])
	}
	market := attrs["market"].(map[string]any)
	if market["marketCode"] != "101" || market["state"] != "Maharashtra" {
		t.Errorf("Pune market = %v", market)
	}
	if _, ok := market["location"]; !ok {
		t.Error("Pune has a coordinate, so its Direct resource must carry a location")
	}
	// Valid for two days from when it was generated: past the next daily
	// run, which republishes it in place, and one missed run.
	validity := attrs["validity"].(map[string]any)
	starts, err1 := time.Parse(time.RFC3339, validity["startsAt"].(string))
	ends, err2 := time.Parse(time.RFC3339, validity["endsAt"].(string))
	if err1 != nil || err2 != nil || validity["startsAt"] != attrs["generatedAt"] || ends.Sub(starts) != 48*time.Hour {
		t.Errorf("validity = %v, generatedAt = %v; want startsAt = generatedAt and endsAt two days later",
			validity, attrs["generatedAt"])
	}

	nashik := resources["resource:mandi-price:price:102:23"]["resourceAttributes"].(map[string]any)
	// Nashik has no coordinate: published, but with no point.
	if _, ok := nashik["market"].(map[string]any)["location"]; ok {
		t.Error("Nashik has no coordinate, yet its Direct resource carries a location")
	}
	// "NR" is an unreported price: absent, not zero.
	if _, ok := nashik["prices"].(map[string]any)["minimum"]; ok {
		t.Error("an NR minimum was published as a price")
	}
}

// mode=direct publishes only the Direct catalogs, though it still reads the
// markets the price calls need.
func TestRunBuildsOnlyDirectCatalogsInDirectMode(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(upstream.URL, "direct"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var slugs []string
	for _, catalog := range report.Catalogs {
		slugs = append(slugs, catalog.Slug)
	}
	sort.Strings(slugs)
	if got, want := strings.Join(slugs, ","), "KA-current,MH-current"; got != want {
		t.Fatalf("slugs = %s, want %s", got, want)
	}
}

// mode=onDemand makes no price calls at all.
func TestOnDemandModeMakesNoPriceCalls(t *testing.T) {
	var priceCalls int
	inner := fakeAgmarknet(t, twoGoodStates())
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/fetch-agmarknet-vistaar" {
			priceCalls++
		}
		proxy, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.RequestURI(), r.Body)
		if err != nil {
			t.Fatalf("proxy: %v", err)
		}
		proxy.Header = r.Header
		resp, err := http.DefaultClient.Do(proxy)
		if err != nil {
			t.Fatalf("proxy: %v", err)
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(counting.Close)

	if _, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(counting.URL, "onDemand"), Now: firingTime(t), OutDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if priceCalls != 0 {
		t.Errorf("onDemand mode made %d price calls, want 0", priceCalls)
	}
}

// The OnDemand resources do not publish historicalDataAvailable, historyPeriod or
// updateFrequency: none could be verified in a way worth asserting (see
// dev_docs/mandi-catalog-contents-and-reasons.md sections 10-11). Add one back
// only with a measurement and a test of its own.
func TestOnDemandResourceDescriptorNamesTheMarket(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(upstream.URL, "onDemand"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Pune is market 101: code MANDI_PRICE_MARKET_<marketId>, name "<market> mandi prices".
	resource := resourcesByID(catalogBySlug(t, report)["MH"])["resource:mandi-price:market:101"]
	descriptor, _ := resource["descriptor"].(map[string]any)
	if descriptor["code"] != "MANDI_PRICE_MARKET_101" || descriptor["name"] != "Pune mandi prices" {
		t.Errorf("Pune descriptor = %v, want code MANDI_PRICE_MARKET_101 and name \"Pune mandi prices\"", descriptor)
	}
}

// Catalog and resource descriptor codes: OnDemand catalogs are MANDI_PRICE_MARKET_<STATE>
// (a split state continues as _2), Direct catalogs MANDI_PRICE_CURRENT_<STATE>, and a
// Direct resource is MANDI_PRICE_OBSERVATION named with its arrival date.
func TestDescriptorCodesFollowTheNamingScheme(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(upstream.URL, "both"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	catalogs := catalogBySlug(t, report)
	code := func(entry map[string]any) any { return entry["descriptor"].(map[string]any)["code"] }
	if got := code(catalogs["MH"]); got != "MANDI_PRICE_MARKET_MH" {
		t.Errorf("OnDemand MH catalog code = %v, want MANDI_PRICE_MARKET_MH", got)
	}
	if got := code(catalogs["MH-current"]); got != "MANDI_PRICE_CURRENT_MH" {
		t.Errorf("Direct MH catalog code = %v, want MANDI_PRICE_CURRENT_MH", got)
	}
	for id, resource := range resourcesByID(catalogs["MH-current"]) {
		d := resource["descriptor"].(map[string]any)
		name, _ := d["name"].(string)
		if d["code"] != "MANDI_PRICE_OBSERVATION" || !strings.Contains(name, " prices at ") || !strings.Contains(name, " on 20") {
			t.Errorf("%s descriptor = %v, want code MANDI_PRICE_OBSERVATION and a name like %q", id, d, "Onion prices at Pune on 2026-09-01")
		}
	}
}

func TestOnDemandResourcesPublishNoHistoryOrUpdateClaims(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	report, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(upstream.URL, "onDemand"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	checked := 0
	for slug, catalog := range catalogBySlug(t, report) {
		for id, resource := range resourcesByID(catalog) {
			attrs := resource["resourceAttributes"].(map[string]any)
			for _, field := range []string{"historicalDataAvailable", "historyPeriod", "updateFrequency"} {
				if _, has := attrs[field]; has {
					t.Errorf("%s %s publishes %s", slug, id, field)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no OnDemand resource was checked")
	}
}

// A mode the file does not list is refused before anything is fetched.
func TestRunRefusesAnUnknownMode(t *testing.T) {
	upstream := fakeAgmarknet(t, twoGoodStates())
	_, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Record: publishingRecord(), Pipeline: Pipeline(),
		Lookup: modeEnv(upstream.URL, "everything"), Now: firingTime(t), OutDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("err = %v, want a refusal naming the mode input", err)
	}
}
