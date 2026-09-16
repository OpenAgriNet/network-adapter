package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
		Route:      &model.Route{TargetType: "url", URL: urls[0], URLs: urls, MergeFieldPath: "message.catalogs"},
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
		Route:                &model.Route{TargetType: "url", URL: urls[0], URLs: urls, MergeFieldPath: "message.catalogs"},
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
	if h := rec.Header().Get(degradedCountHeader); h != "" {
		t.Errorf("fanout() set %s=%q with every target healthy", degradedCountHeader, h)
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
	if h := rec.Header().Get(degradedCountHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the one target that did not contribute", degradedCountHeader, h)
	}
	badHost, _ := url.Parse(bad.URL)
	if strings.Contains(rec.Header().Get(degradedCountHeader), badHost.Hostname()) {
		t.Error("fanout() put an upstream address in the degraded header; it must not disclose topology")
	}
}

func TestFanoutEveryTargetFailsReturnsNack(t *testing.T) {
	bad := discoverServer(t, http.StatusBadGateway, []byte(`{}`), 0)
	defer bad.Close()
	alsoBad := discoverServer(t, http.StatusBadGateway, []byte(`{}`), 0)
	defer alsoBad.Close()

	rec := runFanout(t, FanoutConfig{}, "", bad.URL, alsoBad.URL)

	// 502, not the default 500: every network being unreachable is upstream's
	// failure, not this adapter's.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("fanout() status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	badHost, _ := url.Parse(bad.URL)
	if strings.Contains(rec.Body.String(), badHost.Hostname()) {
		t.Error("fanout() put an upstream address in the NACK body; it must not disclose topology")
	}
}

func TestFanoutCapsPerTargetResponseSize(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxTargetResponseBytes+1))
	}))
	defer huge.Close()
	good := discoverServer(t, http.StatusOK, onDiscover("m-cap", "good-1"), 0)
	defer good.Close()

	rec := runFanout(t, FanoutConfig{}, "", good.URL, huge.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200: an oversized target must not deny the caller the others", rec.Code)
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"good-1"}) {
		t.Errorf("fanout() catalogs = %v, want [good-1]", got)
	}
	if h := rec.Header().Get(degradedCountHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target over the response cap", degradedCountHeader, h)
	}
}

func TestFanoutForwardsClientIPAndPreservesTeTrailers(t *testing.T) {
	// Buffered for both targets (below): a full channel would block a
	// handler goroutine before it ever writes a response, and the test would
	// only finish once the fan-out's own deadline gave up on it.
	seen := make(chan http.Header, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onDiscover("m-xff", "x"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/discover")
	body := []byte(`{"context":{"action":"discover","version":"2.0.0"},"message":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/discover", strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("Te", "trailers, gzip")
	rec := httptest.NewRecorder()
	ctx := &model.StepContext{
		Context:    req.Context(),
		Request:    req,
		Body:       body,
		RespHeader: rec.Header(),
		Route:      &model.Route{TargetType: "url", URL: u, URLs: []*url.URL{u, u}, MergeFieldPath: "message.catalogs"},
	}
	var responseBody []byte
	fanout(ctx, req, rec, &http.Client{}, nil, nil, FanoutConfig{}, func(*model.StepContext, error) {}, &responseBody)

	got := <-seen
	if got.Get("X-Forwarded-For") != "203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q, want the caller's IP", got.Get("X-Forwarded-For"))
	}
	if got.Get("Te") != "trailers" {
		t.Errorf("Te = %q, want \"trailers\" preserved for a client that named it", got.Get("Te"))
	}
	<-seen // drain the second target so its handler goroutine is not left blocked
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
	if h := rec.Header().Get(degradedCountHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target that missed the budget", degradedCountHeader, h)
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
	if h := rec.Header().Get(degradedCountHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target that sent an unreadable body", degradedCountHeader, h)
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
		Route:      &model.Route{TargetType: "url", URL: baked, URLs: []*url.URL{baked, plain}, MergeFieldPath: "message.catalogs"},
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
		Route:      &model.Route{TargetType: "url", URL: u, URLs: []*url.URL{u, u}, MergeFieldPath: "message.catalogs"},
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
	if strings.Contains(rec.Body.String(), "catalogs") {
		t.Error("fanout() wrote a catalogs member into a response that had none")
	}
}

// TestFanoutSameExecutorMergesSelectByConfigAlone proves the executor itself
// (not just merge.go's functions) handles select the same way it handles
// discover, purely because the route's MergeFieldPath says where -- fanout()
// has no branch for "this is discover" or "this is select" anywhere in it.
func TestFanoutSameExecutorMergesSelectByConfigAlone(t *testing.T) {
	onSelect := func(offerID string) []byte {
		return []byte(`{"context":{"action":"on_select","version":"2.0.0"},"message":{"contract":{` +
			`"status":{"descriptor":{"code":"ACTIVE"}},` +
			`"commitments":[{"offer":{"id":"` + offerID + `"}}]}}}`)
	}
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onSelect("offer:agmarknet"))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onSelect("offer:mausamgram"))
	}))
	defer b.Close()

	urlA, _ := url.Parse(a.URL + "/select")
	urlB, _ := url.Parse(b.URL + "/select")
	body := []byte(`{"context":{"action":"select","version":"2.0.0"},"message":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/select", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	ctx := &model.StepContext{
		Context:    req.Context(),
		Request:    req,
		Body:       body,
		RespHeader: rec.Header(),
		Route:      &model.Route{TargetType: "url", URL: urlA, URLs: []*url.URL{urlA, urlB}, MergeFieldPath: "message.contract.commitments"},
	}
	var responseBody []byte
	fanout(ctx, req, rec, &http.Client{}, nil, nil, FanoutConfig{}, func(*model.StepContext, error) {}, &responseBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Message struct {
			Contract struct {
				Commitments []struct {
					Offer struct {
						ID string `json:"id"`
					} `json:"offer"`
				} `json:"commitments"`
			} `json:"contract"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if len(env.Message.Contract.Commitments) != 2 {
		t.Fatalf("fanout() merged %d commitments, want 2 (one per target): %s", len(env.Message.Contract.Commitments), rec.Body.String())
	}
	if h := rec.Header().Get(degradedCountHeader); h != "" {
		t.Errorf("fanout() set %s=%q with both select targets healthy", degradedCountHeader, h)
	}
}

// TestFanoutEveryTargetAnsweringEmptyIsNotTreatedAsNoTarget covers the
// "present but empty" state itemsOf/mergeResponses key their donor and
// no-target-carried-it decisions on: every network genuinely having zero
// matches is a valid 200 with catalogs: [], not the NACK a response
// carrying no catalogs member AT ALL gets refused for.
func TestFanoutEveryTargetAnsweringEmptyIsNotTreatedAsNoTarget(t *testing.T) {
	a := discoverServer(t, http.StatusOK, onDiscover("m-empty"), 0)
	defer a.Close()
	b := discoverServer(t, http.StatusOK, onDiscover("m-empty"), 0)
	defer b.Close()

	rec := runFanout(t, FanoutConfig{}, "", a.URL, b.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200: every network answering zero matches is a real answer, not a failure. body: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Message struct {
			Catalogs []json.RawMessage `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if env.Message.Catalogs == nil {
		t.Error("fanout() omitted the catalogs member entirely; want an empty array, not absent")
	}
	if len(env.Message.Catalogs) != 0 {
		t.Errorf("fanout() catalogs = %v, want none", env.Message.Catalogs)
	}
	if h := rec.Header().Get(degradedCountHeader); h != "" {
		t.Errorf("fanout() set %s=%q; every target answered, none should be degraded", degradedCountHeader, h)
	}
}

// TestFanoutMaxConcurrencyBoundsInFlightTargets proves MaxConcurrency is an
// enforced cap, not just a config field that gets read and forgotten.
func TestFanoutMaxConcurrencyBoundsInFlightTargets(t *testing.T) {
	const targets, maxConcurrent = 5, 2

	var (
		mu      sync.Mutex
		current int
		peak    int
	)
	enter := func() {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()
	}
	leave := func() {
		mu.Lock()
		current--
		mu.Unlock()
	}

	urls := make([]string, targets)
	for i := 0; i < targets; i++ {
		srv := discoverServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
			enter()
			time.Sleep(80 * time.Millisecond)
			leave()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(onDiscover("m-cc", "x"))
		})
		defer srv.Close()
		urls[i] = srv.URL
	}

	runFanout(t, FanoutConfig{MaxConcurrency: maxConcurrent}, "", urls...)

	if peak > maxConcurrent {
		t.Errorf("peak concurrent targets = %d, want <= %d (MaxConcurrency): the cap was not enforced", peak, maxConcurrent)
	}
	if peak < 2 {
		t.Errorf("peak concurrent targets = %d, want >= 2: with a slot to spare and %d slower targets this should never have run fully serial", peak, targets)
	}
}

// discoverServerFunc is discoverServer with a caller-supplied handler, for
// tests that need to observe concurrency rather than just answer a fixed body.
func discoverServerFunc(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

// TestFanoutSkipsAckSignerThroughItsTelemetryWrapperToo is the production
// shape isAckSigner has to handle: stdHandler.go always wraps the ack signer
// in InstrumentedResponseStep before handing it to fanout(), so a test built
// only on a bare *ackSignerStep (as the other signing tests are) never
// exercises the unwrap this depends on. If isAckSigner ever failed to see
// through the wrapper, this would sign every target's own response instead
// of the merged one -- and every other signing test would stay green,
// because none of them wrap it.
func TestFanoutSkipsAckSignerThroughItsTelemetryWrapperToo(t *testing.T) {
	a := discoverServer(t, http.StatusOK, onDiscover("m-wrap", "a1"), 0)
	defer a.Close()
	b := discoverServer(t, http.StatusOK, onDiscover("m-wrap", "b1"), 0)
	defer b.Close()

	signer := &mockSigner{returnSig: "wrappedsig=="}
	km := &mockKM{keyset: &model.Keyset{UniqueKeyID: "key-1", SigningPrivate: "priv"}}
	step, err := newAckSignerStep(signer, km)
	if err != nil {
		t.Fatalf("newAckSignerStep(): %v", err)
	}
	concrete := step.(*ackSignerStep)
	// The exact production shape: stdHandler.go keeps the CONCRETE step
	// (passed to fanout() as ackSigner, run once over the merged body) and
	// puts a WRAPPED one in responseSteps (what collect() iterates per
	// target) -- two references to the same signer, one plain and one
	// wrapped. runFanoutWithSteps only ever hands fanout() a bare
	// *ackSignerStep in both places, so it can't exercise this.
	wrapped, err := NewInstrumentedResponseStep(concrete, "signAck", "test-module")
	if err != nil {
		t.Fatalf("NewInstrumentedResponseStep(): %v", err)
	}

	urls := []*url.URL{}
	for _, target := range []string{a.URL, b.URL} {
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
		MessageID:            "msg-wrap-1",
		SubID:                "bap.example.com",
		InboundAuthSignature: "inboundSig==",
		Route:                &model.Route{TargetType: "url", URL: urls[0], URLs: urls, MergeFieldPath: "message.catalogs"},
	}
	var responseBody []byte
	fanout(ctx, req, rec, &http.Client{}, []definition.ResponseStep{wrapped}, concrete, FanoutConfig{}, func(*model.StepContext, error) {}, &responseBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"a1", "b1"}) {
		t.Errorf("fanout() catalogs = %v, want [a1 b1]: a wrapped ack signer not recognized as one would have run per-target instead of merging", got)
	}
	if sig := rec.Header().Get("Signature"); sig == "" {
		t.Error("fanout() sent the merged response with no Signature header")
	}
	if !bytes.Equal(signer.signedBody, rec.Body.Bytes()) {
		t.Errorf("fanout() signed %s\nbut sent   %s", signer.signedBody, rec.Body.Bytes())
	}
	// The real regression this test exists for: an unrecognized wrapper does
	// not change the FINAL signature (it's overwritten by the correct merged
	// call that runs after collect()), so only a call count catches it -- if
	// isAckSigner failed to see through InstrumentedResponseStep, collect()
	// would run it as an ordinary response step against BOTH targets too,
	// signing each one's own body before the merged call overwrites the result.
	if signer.signAckCalls != 1 {
		t.Errorf("SignAck called %d times, want 1: it must run once over the merged body, not once per target plus once for the merge", signer.signAckCalls)
	}
}

// TestFanoutOnlyOneOfSeveralHasAnsweredWhenBudgetExpires is the scenario
// asked about directly: discover is slow, and by the time the shared budget
// expires only one network out of several has actually answered. The
// merged reply must be exactly that one network's catalogs, with every
// other target counted degraded -- not a NACK, and not a wait for the rest.
func TestFanoutOnlyOneOfSeveralHasAnsweredWhenBudgetExpires(t *testing.T) {
	fast := discoverServer(t, http.StatusOK, onDiscover("m-slow-others", "fast-1"), 0)
	defer fast.Close()

	slowURLs := make([]string, 4)
	for i := range slowURLs {
		slow := discoverServer(t, http.StatusOK, onDiscover("m-slow-others", fmt.Sprintf("slow-%d", i)), 2*time.Second)
		defer slow.Close()
		slowURLs[i] = slow.URL
	}
	targets := append([]string{fast.URL}, slowURLs...)

	rec := runFanout(t, FanoutConfig{Timeout: 150 * time.Millisecond}, "", targets...)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200 with the one answer that arrived in time. body: %s", rec.Code, rec.Body.String())
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"fast-1"}) {
		t.Errorf("fanout() catalogs = %v, want only [fast-1]: the 4 that had not answered yet must not block or leak into the merge", got)
	}
	if h := rec.Header().Get(degradedCountHeader); h != "4" {
		t.Errorf("fanout() %s = %q, want \"4\" for the four still in flight when the budget expired", degradedCountHeader, h)
	}
}

// TestFanoutMoreTargetsThanConcurrencyStillMergesEveryOne proves more than
// ten targets, well past the default MaxConcurrency of 8, still all get
// called and merged correctly -- the second wave is not dropped, starved, or
// raced against the first.
func TestFanoutMoreTargetsThanConcurrencyStillMergesEveryOne(t *testing.T) {
	const n = 15
	urls := make([]string, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("network-%02d", i)
		ids[i] = id
		srv := discoverServer(t, http.StatusOK, onDiscover("m-many", id), 5*time.Millisecond)
		defer srv.Close()
		urls[i] = srv.URL
	}

	// Default MaxConcurrency (8): with 15 targets this runs in waves.
	rec := runFanout(t, FanoutConfig{}, "", urls...)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	got := mergedIDs(t, rec.Body.Bytes())
	if len(got) != n {
		t.Fatalf("fanout() merged %d catalogs, want all %d -- a later wave was dropped or starved", len(got), n)
	}
	sortedGot := append([]string(nil), got...)
	sortedWant := append([]string(nil), ids...)
	sort.Strings(sortedGot)
	sort.Strings(sortedWant)
	if !sameIDs(sortedGot, sortedWant) {
		t.Errorf("fanout() catalogs = %v, want every one of %v (any order)", got, ids)
	}
	if h := rec.Header().Get(degradedCountHeader); h != "" {
		t.Errorf("fanout() set %s=%q with all %d targets healthy", degradedCountHeader, h, n)
	}
}

// TestFanoutCutsOffATargetThatStallsMidBody covers the case the whole-round-
// trip deadline exists for: a target that answers 200 promptly, starts
// writing its body, then stalls -- no more bytes, connection held open --
// past the fan-out's own budget. The single-target httpClientConfig.timeout
// is a separate, per-request bound; this proves the fan-out budget alone,
// with no httpClientConfig configured, still cuts a stalled body read off
// rather than hanging until some other timeout (or never).
func TestFanoutCutsOffATargetThatStallsMidBody(t *testing.T) {
	stalling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"context":{"action":"on_discover"},"message":{"catalogs":`)) // deliberately unterminated
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // held open until the fan-out's own deadline cancels it
	}))
	defer stalling.Close()
	fast := discoverServer(t, http.StatusOK, onDiscover("m-stall", "fast-1"), 0)
	defer fast.Close()

	// Not a goroutine-leak check here: that depends on the TEST SERVER
	// noticing its client disconnected (r.Context().Done()), a TCP-layer
	// detail outside fanout.go's own control and prone to outlast the
	// deadline in exactly the way this test would otherwise flag as a leak.
	// callTarget's own goroutine hygiene is already covered, on a harness
	// that doesn't depend on that, by
	// TestFanoutLeavesNoGoroutineBehindWhenTargetsMissTheBudget.
	rec := runFanout(t, FanoutConfig{Timeout: 150 * time.Millisecond}, "", fast.URL, stalling.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200: the healthy target must not be denied by one that stalled mid-body", rec.Code)
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"fast-1"}) {
		t.Errorf("fanout() catalogs = %v, want only [fast-1]", got)
	}
	if h := rec.Header().Get(degradedCountHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target that stalled mid-body", degradedCountHeader, h)
	}
}

// TestFanoutDuplicateTargetURLsAreCalledSeparatelyAndDedupedByItemID covers a
// rule that (accidentally or not) names the same target twice: both are
// still called as distinct targets (not collapsed to one call), and their
// answers -- identical here, as a real duplicate would be -- are deduped by
// item id in the merge, the same as if two DIFFERENT networks had returned
// the same catalog.
func TestFanoutDuplicateTargetURLsAreCalledSeparatelyAndDedupedByItemID(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(onDiscover("m-dup", "shared-1"))
	}))
	defer srv.Close()

	rec := runFanout(t, FanoutConfig{}, "", srv.URL, srv.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("target received %d calls, want 2: a duplicated target entry must still be called once per occurrence, not collapsed", got)
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"shared-1"}) {
		t.Errorf("fanout() catalogs = %v, want [shared-1] once, deduped by id", got)
	}
}

// countingRejectStep is a definition.ResponseStep that rejects its Nth
// invocation and accepts every other. collect() runs response steps
// serially in target order, so rejectOn deterministically picks which
// target's answer gets rejected regardless of which one actually answered
// first over the network.
type countingRejectStep struct {
	calls    int
	rejectOn int
}

func (s *countingRejectStep) RunOnResponse(_ *model.StepContext, _ *model.ResponseStepContext) error {
	s.calls++
	if s.calls == s.rejectOn {
		return fmt.Errorf("rejected by policy")
	}
	return nil
}

// TestFanoutResponseStepRejectionDegradesOneTargetWithoutFailingOthers covers
// the one collect() branch every other test skips: a configured response
// step other than the ack signer (validateAckSign, in production) rejecting
// one target's answer. That target must be degraded, not merged, and must
// not deny the caller what the other target returned.
func TestFanoutResponseStepRejectionDegradesOneTargetWithoutFailingOthers(t *testing.T) {
	a := discoverServer(t, http.StatusOK, onDiscover("m-reject", "a1"), 0)
	defer a.Close()
	b := discoverServer(t, http.StatusOK, onDiscover("m-reject", "b1"), 0)
	defer b.Close()

	step := &countingRejectStep{rejectOn: 2} // rejects the second target processed, i.e. b (target index 1)
	rec := runFanoutWithSteps(t, []definition.ResponseStep{step}, a.URL, b.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("fanout() status = %d, want 200: one rejected target must not deny the caller the other. body: %s", rec.Code, rec.Body.String())
	}
	if got := mergedIDs(t, rec.Body.Bytes()); !sameIDs(got, []string{"a1"}) {
		t.Errorf("fanout() catalogs = %v, want [a1]: the rejected target must not be merged", got)
	}
	if h := rec.Header().Get(degradedCountHeader); h != "1" {
		t.Errorf("fanout() %s = %q, want \"1\" for the target its response step rejected", degradedCountHeader, h)
	}
	if step.calls != 2 {
		t.Errorf("response step called %d times, want 2 (once per target)", step.calls)
	}
}
