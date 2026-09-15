package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// onDiscover builds an on_discover envelope carrying catalogs with the given ids.
func onDiscover(messageID string, ids ...string) []byte {
	catalogs := make([]string, 0, len(ids))
	for _, id := range ids {
		catalogs = append(catalogs, `{"id":"`+id+`"}`)
	}
	return []byte(`{"context":{"messageId":"` + messageID + `","action":"on_discover"},` +
		`"message":{"catalogs":[` + strings.Join(catalogs, ",") + `]}}`)
}

func mergedIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var env struct {
		Message struct {
			Catalogs []struct {
				ID string `json:"id"`
			} `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("merged body is not a readable on_discover envelope: %v", err)
	}
	ids := make([]string, 0, len(env.Message.Catalogs))
	for _, c := range env.Message.Catalogs {
		ids = append(ids, c.ID)
	}
	return ids
}

// merge is the old mergeCatalogs signature, kept for the merge tests: bodies in,
// merged envelope out. Parsing now happens in collect, so the tests parse here.
func merge(bodies [][]byte, limit int, hasLimit bool) ([]byte, error) {
	kept := make([]keptResponse, 0, len(bodies))
	for _, b := range bodies {
		catalogs, present, err := catalogsOf(b)
		if err != nil {
			return nil, err
		}
		kept = append(kept, keptResponse{body: b, catalogs: catalogs, hasCatalogs: present})
	}
	return mergeCatalogs(kept, limit, hasLimit)
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestMergeCatalogsSeveralNetworksInterleavedRoundRobin(t *testing.T) {
	merged, err := merge([][]byte{
		onDiscover("m-1", "bharat-1", "bharat-2", "bharat-3"),
		onDiscover("m-1", "maha-1"),
		onDiscover("m-1", "third-1", "third-2"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}

	want := []string{"bharat-1", "maha-1", "third-1", "bharat-2", "third-2", "bharat-3"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("mergeCatalogs() = %v, want %v", got, want)
	}
}

func TestMergeCatalogsFirstResponseContextPreserved(t *testing.T) {
	merged, err := merge([][]byte{
		onDiscover("m-2", "a"),
		onDiscover("m-2", "b"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}
	var env struct {
		Context struct {
			MessageID string `json:"messageId"`
			Action    string `json:"action"`
		} `json:"context"`
	}
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body has no readable context: %v", err)
	}
	if env.Context.MessageID != "m-2" || env.Context.Action != "on_discover" {
		t.Errorf("mergeCatalogs() context = %+v, want the first response's context kept whole", env.Context)
	}
}

func TestMergeCatalogsRepeatedIDKeepsFirstOccurrence(t *testing.T) {
	merged, err := merge([][]byte{
		onDiscover("m-3", "shared", "bharat-only"),
		onDiscover("m-3", "shared", "maha-only"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}
	want := []string{"shared", "bharat-only", "maha-only"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("mergeCatalogs() = %v, want %v", got, want)
	}
}

func TestMergeCatalogsLimitPresentTruncatesMergedList(t *testing.T) {
	merged, err := merge([][]byte{
		onDiscover("m-4", "a1", "a2", "a3"),
		onDiscover("m-4", "b1", "b2", "b3"),
	}, 3, true)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}
	// Interleaved first, so the cut keeps both networks represented.
	want := []string{"a1", "b1", "a2"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("mergeCatalogs() = %v, want %v", got, want)
	}
}

func TestMergeCatalogsLimitAbsentReturnsEverything(t *testing.T) {
	merged, err := merge([][]byte{
		onDiscover("m-5", "a1", "a2"),
		onDiscover("m-5", "b1", "b2"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}
	if got := mergedIDs(t, merged); len(got) != 4 {
		t.Errorf("mergeCatalogs() returned %d catalogs, want all 4 kept when no limit was sent", len(got))
	}
}

func TestMergeCatalogsNetworkWithNoCatalogsContributesNothing(t *testing.T) {
	merged, err := merge([][]byte{
		onDiscover("m-6", "only"),
		[]byte(`{"context":{"messageId":"m-6"},"message":{}}`),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}
	if got := mergedIDs(t, merged); !sameIDs(got, []string{"only"}) {
		t.Errorf("mergeCatalogs() = %v, want [only]", got)
	}
}

func TestMergeCatalogsUnknownCatalogMembersSurvive(t *testing.T) {
	body := []byte(`{"context":{},"message":{"catalogs":[{"id":"a","futureMember":42}]}}`)
	merged, err := merge([][]byte{body}, 0, false)
	if err != nil {
		t.Fatalf("mergeCatalogs() error = %v", err)
	}
	var env struct {
		Message struct {
			Catalogs []map[string]json.RawMessage `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if _, ok := env.Message.Catalogs[0]["futureMember"]; !ok {
		t.Error("mergeCatalogs() dropped a catalog member it does not know about")
	}
}

func TestMergeCatalogsNonObjectResponseReturnsError(t *testing.T) {
	if _, err := merge([][]byte{[]byte(`["not an envelope"]`)}, 0, false); err == nil {
		t.Error("mergeCatalogs() with a non-object response = nil error, want an error")
	}
}

func TestFanoutQueryNonZeroOffsetIsRefused(t *testing.T) {
	_, _, _, err := fanoutQuery(url.Values{"offset": {"20"}, "limit": {"10"}})
	if err == nil {
		t.Fatal("fanoutQuery() with offset=20 = nil error, want a refusal")
	}
	var coded *model.CodedErr
	if !asCodedErr(err, &coded) {
		t.Fatalf("fanoutQuery() error = %T, want a *model.CodedErr so it NACKs as a bad request", err)
	}
}

func TestFanoutQueryZeroOffsetIsStrippedAndLimitKept(t *testing.T) {
	out, limit, hasLimit, err := fanoutQuery(url.Values{"offset": {"0"}, "limit": {"25"}})
	if err != nil {
		t.Fatalf("fanoutQuery() error = %v", err)
	}
	if out.Has("offset") {
		t.Error("fanoutQuery() forwarded offset; it means 'skip N in each network' and must be stripped")
	}
	if out.Get("limit") != "25" {
		t.Errorf("fanoutQuery() limit = %q, want it forwarded unchanged", out.Get("limit"))
	}
	if !hasLimit || limit != 25 {
		t.Errorf("fanoutQuery() = (%d, %v), want (25, true) for the merged truncation", limit, hasLimit)
	}
}

func TestFanoutQueryNoLimitMeansNoTruncation(t *testing.T) {
	_, limit, hasLimit, err := fanoutQuery(url.Values{})
	if err != nil {
		t.Fatalf("fanoutQuery() error = %v", err)
	}
	if hasLimit || limit != 0 {
		t.Errorf("fanoutQuery() = (%d, %v), want (0, false): the default page size belongs to the discovery service", limit, hasLimit)
	}
}

func TestFanoutQueryOtherParamsPassThrough(t *testing.T) {
	out, _, _, err := fanoutQuery(url.Values{"domain": {"crop-advisory"}})
	if err != nil {
		t.Fatalf("fanoutQuery() error = %v", err)
	}
	if out.Get("domain") != "crop-advisory" {
		t.Error("fanoutQuery() dropped a query parameter that is not paging")
	}
}

// --- executor, over real listeners ------------------------------------------

func discoverServer(t *testing.T, status int, body []byte, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

// runFanout drives the executor against the given targets and returns the
// recorder it wrote to.
func runFanout(t *testing.T, cfg FanoutConfig, rawQuery string, targets ...string) *httptest.ResponseRecorder {
	t.Helper()
	urls := make([]*url.URL, 0, len(targets))
	for _, target := range targets {
		u, err := url.Parse(target + "/discover")
		if err != nil {
			t.Fatalf("parsing target %q: %v", target, err)
		}
		urls = append(urls, u)
	}

	body := []byte(`{"context":{"action":"discover","version":"2.0.0"},"message":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/discover?"+rawQuery, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	ctx := &model.StepContext{
		Context:    req.Context(),
		Request:    req,
		Body:       body,
		RespHeader: rec.Header(),
		Route:      &model.Route{TargetType: "url", URL: urls[0], URLs: urls},
	}

	var responseBody []byte
	fanout(ctx, req, rec, &http.Client{}, nil, nil, cfg, func(*model.StepContext, error) {}, &responseBody)
	return rec
}

// runFanoutWithSteps is runFanout with response steps and the v2 identity an
// ackSigner needs.
func runFanoutWithSteps(t *testing.T, steps []definition.ResponseStep, targets ...string) *httptest.ResponseRecorder {
	t.Helper()
	urls := make([]*url.URL, 0, len(targets))
	for _, target := range targets {
		u, err := url.Parse(target + "/discover")
		if err != nil {
			t.Fatalf("parsing target %q: %v", target, err)
		}
		urls = append(urls, u)
	}

	body := []byte(`{"context":{"action":"discover","version":"2.0.0"},"message":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/discover", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	ctx := &model.StepContext{
		Context:              req.Context(),
		Request:              req,
		Body:                 body,
		RespHeader:           rec.Header(),
		ProtocolVersion:      "2.0.0",
		MessageID:            "msg-sign-1",
		SubID:                "bap.example.com",
		InboundAuthSignature: "inboundSig==",
		Route:                &model.Route{TargetType: "url", URL: urls[0], URLs: urls},
	}

	var responseBody []byte
	var signer *ackSignerStep
	for _, step := range steps {
		if as, ok := step.(*ackSignerStep); ok {
			signer = as
		}
	}
	fanout(ctx, req, rec, &http.Client{}, steps, signer, FanoutConfig{}, func(*model.StepContext, error) {}, &responseBody)
	return rec
}

func TestFanoutEveryTargetAnswersCatalogsAreMerged(t *testing.T) {
	a := discoverServer(t, http.StatusOK, onDiscover("m-7", "a1"), 0)
	defer a.Close()
	b := discoverServer(t, http.StatusOK, onDiscover("m-7", "b1"), 0)
	defer b.Close()

	rec := runFanout(t, FanoutConfig{}, "", a.URL, b.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"a1", "b1"}) {
		t.Errorf("fanout() catalogs = %v, want [a1 b1]", got)
	}
	if h := rec.Header().Get(degradedHeader); h != "" {
		t.Errorf("fanout() set %s=%q with every target healthy", degradedHeader, h)
	}
}

func TestFanoutOneTargetFailsOthersStillAnswerAndItIsReportedDegraded(t *testing.T) {
	good := discoverServer(t, http.StatusOK, onDiscover("m-8", "good-1"), 0)
	defer good.Close()
	bad := discoverServer(t, http.StatusInternalServerError, []byte(`{"error":{}}`), 0)
	defer bad.Close()

	rec := runFanout(t, FanoutConfig{}, "", good.URL, bad.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200: one failing network must not deny the caller the others", rec.Code)
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"good-1"}) {
		t.Errorf("fanout() catalogs = %v, want [good-1]", got)
	}
	if h := rec.Header().Get(degradedHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the one target that did not contribute", degradedHeader, h)
	}
	badHost, _ := url.Parse(bad.URL)
	if strings.Contains(rec.Header().Get(degradedHeader), badHost.Hostname()) {
		t.Error("fanout() put an upstream address in the degraded header; it must not disclose topology")
	}
}

func TestFanoutEveryTargetFailsReturnsNack(t *testing.T) {
	bad := discoverServer(t, http.StatusBadGateway, []byte(`{}`), 0)
	defer bad.Close()
	alsoBad := discoverServer(t, http.StatusBadGateway, []byte(`{}`), 0)
	defer alsoBad.Close()

	rec := runFanout(t, FanoutConfig{}, "", bad.URL, alsoBad.URL)

	if rec.Code == http.StatusOK {
		t.Errorf("fanout() status = 200 with no target answering; an empty catalog list is indistinguishable from no matches")
	}
}

func TestFanoutTargetSlowerThanBudgetIsReportedDegraded(t *testing.T) {
	fast := discoverServer(t, http.StatusOK, onDiscover("m-9", "fast-1"), 0)
	defer fast.Close()
	slow := discoverServer(t, http.StatusOK, onDiscover("m-9", "slow-1"), 2*time.Second)
	defer slow.Close()

	rec := runFanout(t, FanoutConfig{Timeout: 150 * time.Millisecond}, "", fast.URL, slow.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200 with whatever arrived in time", rec.Code)
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"fast-1"}) {
		t.Errorf("fanout() catalogs = %v, want only [fast-1]", got)
	}
	if h := rec.Header().Get(degradedHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target that missed the budget", degradedHeader, h)
	}
}

func TestFanoutNonZeroOffsetReturnsNackWithoutCallingAnyTarget(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onDiscover("m-10", "x"))
	}))
	defer srv.Close()

	rec := runFanout(t, FanoutConfig{}, "offset=20&limit=10", srv.URL, srv.URL)

	if rec.Code == http.StatusOK {
		t.Error("fanout() accepted a non-zero offset; it cannot be expressed across networks")
	}
	if called {
		t.Error("fanout() called a target despite refusing the request")
	}
}

func TestFanoutTargetReceivesBodyAndDropsOffset(t *testing.T) {
	type seen struct {
		body  string
		query url.Values
	}
	got := make(chan seen, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		got <- seen{body: string(b), query: r.URL.Query()}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onDiscover("m-11", "x"))
	}))
	defer srv.Close()

	runFanout(t, FanoutConfig{}, "offset=0&limit=5", srv.URL, srv.URL)

	for i := 0; i < 2; i++ {
		s := <-got
		if s.body == "" {
			t.Fatal("a target received an empty body: ctx.Body must be re-read per target, not streamed from r.Body")
		}
		if s.query.Has("offset") {
			t.Error("a target received offset; it must be stripped before fan-out")
		}
		if s.query.Get("limit") != "5" {
			t.Errorf("a target received limit=%q, want 5 forwarded unchanged", s.query.Get("limit"))
		}
	}
}

func asCodedErr(err error, target **model.CodedErr) bool {
	return errors.As(err, target)
}

func TestFanoutTargetAnswers200WithAnUnreadableBodyIsDegradedNotFatal(t *testing.T) {
	good := discoverServer(t, http.StatusOK, onDiscover("m-12", "good-1"), 0)
	defer good.Close()
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer junk.Close()

	rec := runFanout(t, FanoutConfig{}, "", good.URL, junk.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200: one target's unreadable body must not deny the caller the others", rec.Code)
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"good-1"}) {
		t.Errorf("fanout() catalogs = %v, want [good-1]", got)
	}
	if h := rec.Header().Get(degradedHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target that sent an unreadable body", degradedHeader, h)
	}
}

func TestFanoutEveryTargetAnswersUnreadablyReturnsNack(t *testing.T) {
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`["an array, not an envelope"]`))
	}))
	defer junk.Close()

	rec := runFanout(t, FanoutConfig{}, "", junk.URL, junk.URL)

	if rec.Code == http.StatusOK {
		t.Error("fanout() answered 200 with no readable response from any target")
	}
}

func TestFanoutSignsTheMergedBodyAndTheSignatureReachesTheCaller(t *testing.T) {
	a := discoverServer(t, http.StatusOK, onDiscover("m-sign", "a1"), 0)
	defer a.Close()
	b := discoverServer(t, http.StatusOK, onDiscover("m-sign", "b1"), 0)
	defer b.Close()

	signer := &mockSigner{returnSig: "mergedsig=="}
	km := &mockKM{keyset: &model.Keyset{UniqueKeyID: "key-1", SigningPrivate: "priv"}}
	ackSigner, err := newAckSignerStep(signer, km)
	if err != nil {
		t.Fatalf("newAckSignerStep(): %v", err)
	}

	rec := runFanoutWithSteps(t, []definition.ResponseStep{ackSigner}, a.URL, b.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}

	// The caller must actually receive the signature. rctx.Header on this path
	// is the upstream's own response header, which is never copied out -- so a
	// signature written there reaches nobody.
	if sig := rec.Header().Get("Signature"); sig == "" {
		t.Error("fanout() sent the merged response with no Signature header")
	}

	// And it must cover what the caller was sent, not one upstream's body.
	if !bytes.Equal(signer.signedBody, rec.Body.Bytes()) {
		t.Errorf("fanout() signed %s\nbut sent   %s", signer.signedBody, rec.Body.Bytes())
	}
}

func TestFanoutLeavesNoGoroutineBehindWhenTargetsMissTheBudget(t *testing.T) {
	slow := discoverServer(t, http.StatusOK, onDiscover("m-leak", "slow-1"), 3*time.Second)
	defer slow.Close()
	fast := discoverServer(t, http.StatusOK, onDiscover("m-leak", "fast-1"), 0)
	defer fast.Close()

	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		runFanout(t, FanoutConfig{Timeout: 100 * time.Millisecond}, "", fast.URL, slow.URL)
	}

	// fanout waits on its workers before returning, so the only thing that can
	// still be running is the transport's own connection handling.
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Errorf("goroutines: %d before, %d after five timed-out fan-outs — workers are outliving the call", before, after)
	}
}

func TestFanoutKeepsATargetsOwnQueryAndStillDropsOffset(t *testing.T) {
	seen := make(chan url.Values, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onDiscover("m-q", "x"))
	}))
	defer srv.Close()

	// A target configured with a query of its own, as a rule naming several
	// networks may well be.
	baked, err := url.Parse(srv.URL + "/discover?network=maha")
	if err != nil {
		t.Fatalf("parsing target: %v", err)
	}
	plain, _ := url.Parse(srv.URL + "/discover")

	body := []byte(`{"context":{"action":"discover","version":"2.0.0"},"message":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/discover?limit=5&offset=0", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	ctx := &model.StepContext{
		Context:    req.Context(),
		Request:    req,
		Body:       body,
		RespHeader: rec.Header(),
		Route:      &model.Route{TargetType: "url", URL: baked, URLs: []*url.URL{baked, plain}},
	}
	var responseBody []byte
	fanout(ctx, req, rec, &http.Client{}, nil, nil, FanoutConfig{}, func(*model.StepContext, error) {}, &responseBody)

	// Both targets are read: they answer in whatever order they finish, and
	// only one of them carries a configured query.
	baked_seen := 0
	for i := 0; i < 2; i++ {
		got := <-seen
		if got.Get("limit") != "5" {
			t.Errorf("target query = %v, want the inbound limit=5 carried to every target", got)
		}
		if got.Has("offset") {
			t.Errorf("target query = %v, want offset dropped whatever its source", got)
		}
		if got.Get("network") == "maha" {
			baked_seen++
		}
	}
	if baked_seen != 1 {
		t.Errorf("network=maha reached %d targets, want exactly the one configured with it", baked_seen)
	}
}

func TestFanoutStripsHeadersNamedByConnection(t *testing.T) {
	seen := make(chan http.Header, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onDiscover("m-h", "x"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/discover")
	body := []byte(`{"context":{"action":"discover","version":"2.0.0"},"message":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/discover", strings.NewReader(string(body)))
	req.Header.Set("Connection", "X-Internal-Token")
	req.Header.Set("X-Internal-Token", "secret")
	req.Header.Set("X-Keep-Me", "yes")
	rec := httptest.NewRecorder()
	ctx := &model.StepContext{
		Context:    req.Context(),
		Request:    req,
		Body:       body,
		RespHeader: rec.Header(),
		Route:      &model.Route{TargetType: "url", URL: u, URLs: []*url.URL{u, u}},
	}
	var responseBody []byte
	fanout(ctx, req, rec, &http.Client{}, nil, nil, FanoutConfig{}, func(*model.StepContext, error) {}, &responseBody)

	got := <-seen
	if got.Get("X-Internal-Token") != "" {
		t.Error("a header named by Connection reached the target; the proxy path strips those")
	}
	if got.Get("Connection") != "" {
		t.Error("Connection itself reached the target")
	}
	if got.Get("X-Keep-Me") != "yes" {
		t.Error("an ordinary header was stripped")
	}
}

func TestFanoutRefusesAnActionWhoseRepliesCarryNoCatalogs(t *testing.T) {
	// An ACK, which is what a transactional action answers with. Fanning one
	// out and merging it would mean writing a catalogs array into a body whose
	// schema forbids it -- and then signing over those bytes.
	ack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"context":{"action":"on_select"},"message":{"status":"ACK","messageId":"m-1"}}`))
	}))
	defer ack.Close()

	rec := runFanout(t, FanoutConfig{}, "", ack.URL, ack.URL)

	if rec.Code == http.StatusOK {
		t.Errorf("fanout() merged replies that carry no catalogs: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), catalogsKey) {
		t.Error("fanout() wrote a catalogs member into a response that had none")
	}
}

func TestMergeCatalogsErrorEnvelopeDoesNotDonateTheEnvelope(t *testing.T) {
	// 200 with a Beckn error envelope and no catalogs: kept, but it must not
	// be the envelope the caller receives.
	merged, err := merge([][]byte{
		[]byte(`{"context":{"messageId":"m-e"},"error":{"code":"NET_SOMETHING"}}`),
		onDiscover("m-e", "real-1"),
	}, 0, false)
	if err != nil {
		t.Fatalf("merge() error = %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if _, ok := env["error"]; ok {
		t.Error("merge() carried one network's error member into the merged answer")
	}
	if got := mergedIDs(t, merged); !sameIDs(got, []string{"real-1"}) {
		t.Errorf("merge() = %v, want [real-1]", got)
	}
}
