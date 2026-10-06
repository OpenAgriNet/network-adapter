package catalogcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/sink"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogcrawler/internal/store"
)

// fakeProjections is the projection table in memory, with the same ordering
// guarantees the real one has: none.
type fakeProjections struct {
	rows      map[string]map[string]time.Time // network -> catalog -> expiry
	listErr   error
	recordErr error
	forgotten []string
}

func newFakeProjections() *fakeProjections {
	return &fakeProjections{rows: map[string]map[string]time.Time{}}
}

func (f *fakeProjections) RecordProjections(_ context.Context, networkID string, catalogIDs []string, expiresAt time.Time) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	if f.rows[networkID] == nil {
		f.rows[networkID] = map[string]time.Time{}
	}
	for _, id := range catalogIDs {
		f.rows[networkID][id] = expiresAt
	}
	return nil
}

func (f *fakeProjections) ProjectionsFor(_ context.Context, networkID string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var ids []string
	for id := range f.rows[networkID] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (f *fakeProjections) ExpiredProjections(_ context.Context, asOf time.Time) ([]store.Projection, error) {
	var expired []store.Projection
	for network, catalogs := range f.rows {
		for id, expiry := range catalogs {
			if !expiry.After(asOf) {
				expired = append(expired, store.Projection{NetworkID: network, CatalogID: id})
			}
		}
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].CatalogID < expired[j].CatalogID })
	return expired, nil
}

func (f *fakeProjections) ProjectedNetworks(_ context.Context) ([]string, error) {
	var networks []string
	for network := range f.rows {
		networks = append(networks, network)
	}
	sort.Strings(networks)
	return networks, nil
}

func (f *fakeProjections) ForgetProjection(_ context.Context, networkID, catalogID string) error {
	delete(f.rows[networkID], catalogID)
	if len(f.rows[networkID]) == 0 {
		delete(f.rows, networkID)
	}
	f.forgotten = append(f.forgotten, networkID+"/"+catalogID)
	return nil
}

func (f *fakeProjections) holds(networkID, catalogID string) bool {
	_, ok := f.rows[networkID][catalogID]
	return ok
}

// recordingPusher captures what was published, so a test can tell a withdrawal
// from an ordinary push.
type recordingPusher struct {
	pushes []map[string]any
	err    error
}

func (r *recordingPusher) Push(_ context.Context, _ string, body []byte) (sink.BatchOutcome, error) {
	if r.err != nil {
		return sink.BatchOutcome{}, r.err
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return sink.BatchOutcome{}, err
	}
	r.pushes = append(r.pushes, decoded)
	return sink.BatchOutcome{}, nil
}

// withdrawals returns the catalog ids this pusher was asked to deactivate.
func (r *recordingPusher) withdrawals() []string {
	var ids []string
	for _, push := range r.pushes {
		message, _ := push["message"].(map[string]any)
		catalogs, _ := message["catalogs"].([]any)
		if len(catalogs) != 1 {
			continue
		}
		catalog, _ := catalogs[0].(map[string]any)
		if active, ok := catalog["isActive"].(bool); ok && !active {
			id, _ := catalog["id"].(string)
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func quietCrawl(projections projectionStore, pusher catalogPusher) *peerCrawl {
	return &peerCrawl{
		localNetwork: "bharat-vistar",
		subscriberID: "bharat-vistar",
		push:         pusher,
		pushEndpoint: "http://discovery.invalid/publish",
		projections:  projections,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// reconcile takes the ids a pass published, not the documents: each audience
// releases its documents as it publishes them, so what reaches here does not
// grow with the size of the peer.
func catalogsNamed(ids ...string) []string {
	return ids
}

// --- the six cases the plan tabulates -----------------------------------

// Case 1: peer unreachable, ttl not lapsed -> keep serving what we have.
//
// Nothing is withdrawn, because an unreachable peer has told us nothing about
// what it still publishes. The ttl, and only the ttl, empties the cache.
func TestAnUnreachablePeerInsideItsTtlKeepsBeingServed(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{}
	crawl := quietCrawl(projections, pusher)
	target := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Hour}

	// It answered once.
	if err := crawl.reconcile(context.Background(), target, catalogsNamed("c1", "c2")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Now it is unreachable: the pass runs, the fetch fails, nothing reconciles.
	crawl.purgeExpired(context.Background(), time.Now())

	if got := pusher.withdrawals(); len(got) != 0 {
		t.Fatalf("withdrew %v while the ttl was still running", got)
	}
	if !projections.holds("maha-vistar", "c1") {
		t.Fatal("a projection inside its ttl was dropped")
	}
}

// Case 2: peer unreachable, ttl lapsed -> stop serving it.
func TestAnUnreachablePeerPastItsTtlStopsBeingServed(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{}
	crawl := quietCrawl(projections, pusher)
	target := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Minute}

	if err := crawl.reconcile(context.Background(), target, catalogsNamed("c1", "c2")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// No crawl succeeds after this; only time passes.
	crawl.purgeExpired(context.Background(), time.Now().Add(2*time.Minute))

	want := []string{"c1", "c2"}
	if got := pusher.withdrawals(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("withdrew %v, want %v once the ttl lapsed", got, want)
	}
	if projections.holds("maha-vistar", "c1") {
		t.Fatal("an expired projection is still recorded as held")
	}
}

// Case 3: peer drops a catalog it used to return -> that catalog stops being
// served, and the others do not.
//
// Acted on immediately rather than at ttl: a successful crawl that omits it is
// positive evidence of withdrawal, not an absence of evidence.
func TestACatalogThePeerStoppedPublishingIsWithdrawn(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{}
	crawl := quietCrawl(projections, pusher)
	target := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Hour}
	ctx := context.Background()

	if err := crawl.reconcile(ctx, target, catalogsNamed("c1", "c2", "c3")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The next pass returns c2 only.
	if err := crawl.reconcile(ctx, target, catalogsNamed("c2")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := pusher.withdrawals()
	if len(got) != 2 || got[0] != "c1" || got[1] != "c3" {
		t.Fatalf("withdrew %v, want exactly the two the peer dropped", got)
	}
	if !projections.holds("maha-vistar", "c2") {
		t.Fatal("a catalog the peer still publishes was withdrawn")
	}
}

// Case 4: peer suspended -> stop crawling AND stop serving.
//
// The one the plan calls out as mattering most: suspension already stopped the
// crawl, and before this it left the cache answering on a network we had
// decided to stop dealing with.
func TestASuspendedPeersProjectionsAreWithdrawn(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{}
	crawl := quietCrawl(projections, pusher)
	ctx := context.Background()

	suspended := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Hour}
	kept := peerTarget{NetworkID: "tamil-vistar", ProjectionTtl: time.Hour}
	if err := crawl.reconcile(ctx, suspended, catalogsNamed("m1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := crawl.reconcile(ctx, kept, catalogsNamed("t1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The registry now admits only tamil-vistar.
	crawl.purgeUnadmitted(ctx, []peerTarget{kept})

	got := pusher.withdrawals()
	if len(got) != 1 || got[0] != "m1" {
		t.Fatalf("withdrew %v, want only the suspended peer's catalog", got)
	}
	if !projections.holds("tamil-vistar", "t1") {
		t.Fatal("a still-admitted peer's projection was withdrawn")
	}
}

// Case 5: peer shortens its ttl on a new revision -> the shorter one takes
// effect, on the next crawl, without waiting out the old one.
func TestAShortenedTtlTakesEffectOnTheNextCrawl(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{}
	crawl := quietCrawl(projections, pusher)
	ctx := context.Background()

	generous := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: 24 * time.Hour}
	if err := crawl.reconcile(ctx, generous, catalogsNamed("c1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// A new revision shortens it. The same catalog is returned.
	strict := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Minute}
	if err := crawl.reconcile(ctx, strict, catalogsNamed("c1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	crawl.purgeExpired(ctx, time.Now().Add(2*time.Minute))
	if got := pusher.withdrawals(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("withdrew %v, want c1 -- the shortened ttl did not take effect", got)
	}
}

// Case 6: peer returns nothing at all -> treated as "has nothing", not as a
// failure. Everything it used to publish is withdrawn, because a successful
// empty answer IS an answer.
func TestAPeerThatReturnsNothingHasEverythingWithdrawn(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{}
	crawl := quietCrawl(projections, pusher)
	target := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Hour}
	ctx := context.Background()

	if err := crawl.reconcile(ctx, target, catalogsNamed("c1", "c2")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := crawl.reconcile(ctx, target, nil); err != nil {
		t.Fatalf("an empty answer was treated as a failure: %v", err)
	}

	got := pusher.withdrawals()
	if len(got) != 2 {
		t.Fatalf("withdrew %v, want both catalogs", got)
	}
}

// --- the properties the six cases rest on --------------------------------

// A withdrawal must actually remove the resources, which is what every discover
// retriever filters on. FULL plus no resources is the whole mechanism.
func TestAWithdrawalIsAFullPublishWithNoResources(t *testing.T) {
	pusher := &recordingPusher{}
	crawl := quietCrawl(newFakeProjections(), pusher)

	if err := crawl.purge(context.Background(), "maha-vistar", "c1"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(pusher.pushes) != 1 {
		t.Fatalf("pushed %d times, want 1", len(pusher.pushes))
	}

	message := pusher.pushes[0]["message"].(map[string]any)
	directive := message["publishDirectives"].([]any)[0].(map[string]any)
	if directive["updateMode"] != sink.UpdateModeFull {
		t.Fatalf("updateMode = %v, want FULL so omitted resources are deleted", directive["updateMode"])
	}
	catalog := message["catalogs"].([]any)[0].(map[string]any)
	if _, present := catalog["resources"]; present {
		t.Fatal("the withdrawal carried resources, so FULL would keep them")
	}
	if catalog["isActive"] != false {
		t.Fatalf("isActive = %v, want false", catalog["isActive"])
	}
}

// A projection is forgotten only after the withdrawal succeeded. Forgetting
// first would leave a catalog served with nothing left that could name it.
func TestAFailedWithdrawalKeepsTheProjectionForTheNextSweep(t *testing.T) {
	projections := newFakeProjections()
	pusher := &recordingPusher{err: errors.New("discovery is down")}
	crawl := quietCrawl(projections, pusher)
	target := peerTarget{NetworkID: "maha-vistar", ProjectionTtl: time.Minute}
	ctx := context.Background()

	crawl.push = &recordingPusher{}
	if err := crawl.reconcile(ctx, target, catalogsNamed("c1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	crawl.push = pusher

	crawl.purgeExpired(ctx, time.Now().Add(time.Hour))

	if !projections.holds("maha-vistar", "c1") {
		t.Fatal("a projection was forgotten even though its withdrawal failed")
	}
}

// A peer whose registry record predates projectionTtl still gets a finite
// licence. There is no state in which a projection never expires.
func TestAPeerWithNoRecordedTtlStillExpires(t *testing.T) {
	crawl := quietCrawl(newFakeProjections(), &recordingPusher{})
	if got := crawl.ttlFor(peerTarget{NetworkID: "maha-vistar"}); got <= 0 {
		t.Fatalf("ttl = %v, want a finite default", got)
	}
	if got := crawl.ttlFor(peerTarget{NetworkID: "maha-vistar"}); got != defaultProjectionTtl {
		t.Fatalf("ttl = %v, want %v", got, defaultProjectionTtl)
	}
}

// A catalog with no id cannot be recorded, so it cannot be purged later. It is
// counted and reported rather than silently published.
func TestCatalogsWithNoIdAreCountedNotRecorded(t *testing.T) {
	documents := []json.RawMessage{
		json.RawMessage(`{"id":"c1"}`),
		json.RawMessage(`{"descriptor":{"name":"nameless"}}`),
		json.RawMessage(`not json`),
	}
	ids, unnamed := catalogIDsOf(documents)
	if len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("ids = %v, want only c1", ids)
	}
	if unnamed != 2 {
		t.Fatalf("unnamed = %d, want 2", unnamed)
	}
}

// --- refresh pacing -------------------------------------------------------

// The shortest licence any peer granted governs the loop: a pass visits every
// peer, so pacing for the most permissive one lets the strictest peer's data
// lapse between passes.
func TestTheRefreshIntervalFollowsTheStrictestPeer(t *testing.T) {
	targets := []peerTarget{
		{NetworkID: "generous", ProjectionTtl: 24 * time.Hour},
		{NetworkID: "strict", ProjectionTtl: 30 * time.Minute},
	}
	got := refreshInterval(time.Hour, targets)
	if want := 10 * time.Minute; got != want {
		t.Fatalf("interval = %v, want %v (the strictest ttl over %d)", got, want, refreshesPerTtl)
	}
}

// The configured interval is an upper bound, never a licence to exceed what a
// peer permitted -- so a long configured interval never lengthens the loop past
// a peer's ttl, and a short one is respected.
func TestTheConfiguredIntervalIsOnlyAnUpperBound(t *testing.T) {
	targets := []peerTarget{{NetworkID: "generous", ProjectionTtl: 24 * time.Hour}}
	if got := refreshInterval(5*time.Minute, targets); got != 5*time.Minute {
		t.Fatalf("interval = %v, want the shorter configured 5m", got)
	}
}

// Refreshing exactly at the ttl means one failed pass is an expiry. Every
// licence has to contain more than one attempt.
func TestARefreshHappensWellBeforeTheTtlLapses(t *testing.T) {
	ttl := 30 * time.Minute
	got := refreshInterval(time.Hour, []peerTarget{{NetworkID: "p", ProjectionTtl: ttl}})
	if got >= ttl {
		t.Fatalf("interval %v does not refresh before the ttl %v lapses", got, ttl)
	}
	if ttl/got < 2 {
		t.Fatalf("interval %v leaves no retry inside the ttl %v", got, ttl)
	}
}

// A peer declaring a very short ttl must not turn the crawler into a hot loop.
func TestAVeryShortTtlIsFlooredRatherThanBecomingAHotLoop(t *testing.T) {
	got := refreshInterval(time.Hour, []peerTarget{{NetworkID: "p", ProjectionTtl: time.Second}})
	if got < minRefreshInterval {
		t.Fatalf("interval = %v, want it floored at %v", got, minRefreshInterval)
	}
}

// A peer with no recorded ttl must not be read as having granted a long one.
func TestAnUnrecordedTtlDoesNotLengthenTheLoop(t *testing.T) {
	targets := []peerTarget{{NetworkID: "old-record"}, {NetworkID: "strict", ProjectionTtl: 30 * time.Minute}}
	if got := refreshInterval(time.Hour, targets); got != 10*time.Minute {
		t.Fatalf("interval = %v, want the strict peer's pace to still govern", got)
	}
}
