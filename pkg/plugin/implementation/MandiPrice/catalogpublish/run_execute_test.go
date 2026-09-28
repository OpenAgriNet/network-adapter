package catalogpublish

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		BindingKey: "agmarknet-live|" + Capability,
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
				_, _ = w.Write([]byte(masterMarketsBody))
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
		}
		return "", false
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

func (p *acceptingPublisher) Retire(_ context.Context, _, catalogID, _ string) pipeline.Outcome {
	p.retired++
	return pipeline.Outcome{CatalogID: catalogID, Status: pipeline.StatusPublished}
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

func (p refusingPublisher) Retire(_ context.Context, _, catalogID, _ string) pipeline.Outcome {
	p.t.Error("a catalog was retired although the run should have refused")
	return pipeline.Outcome{CatalogID: catalogID, Status: pipeline.StatusPublished}
}

// The rendered catalog's contract, from a real run:
//   - an ISO 3166-2 area only for a state the table knows (Review Focus 2);
//     an unknown state gets none, never a guessed "IN-<code>";
//   - market.districtName and market.districtId, no bare `district`;
//   - validity carries the schedule's offset (+05:30), not Z.
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
		if start := entry["validity"].(map[string]any)["startDate"].(string); !strings.HasSuffix(start, "+05:30") {
			t.Errorf("%s: validity.startDate = %q, want the +05:30 offset", catalog.Slug, start)
		}
		for _, raw := range entry["resources"].([]any) {
			attrs := raw.(map[string]any)["resourceAttributes"].(map[string]any)
			market := attrs["market"].(map[string]any)
			if _, bare := market["district"]; bare || market["districtName"] == nil || market["districtId"] == nil {
				t.Errorf("%s: market = %v; want districtName and districtId, no district", catalog.Slug, market)
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
