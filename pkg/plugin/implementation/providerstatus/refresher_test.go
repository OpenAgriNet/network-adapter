package providerstatus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedLister answers each call with the next scripted result, then repeats
// the last one.
type scriptedLister struct {
	mu      sync.Mutex
	results []listResult
	calls   int
	block   bool
}

type listResult struct {
	ids []string
	err error
}

func (l *scriptedLister) ActiveProviderIDs(ctx context.Context, _, _ string) ([]string, error) {
	l.mu.Lock()
	i := min(l.calls, len(l.results)-1)
	l.calls++
	block, res := l.block, l.results[i]
	l.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return res.ids, res.err
}

func (l *scriptedLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func TestRefresherServesTheLastList(t *testing.T) {
	r := newRefresher(&scriptedLister{results: []listResult{{ids: []string{"p1", "p2"}}}}, "", "", time.Second, time.Minute)
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	snap, err := r.active()
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	_, has1 := snap.set["p1"]
	_, has2 := snap.set["p2"]
	if fmt.Sprint(snap.ids) != "[p1 p2]" || !has1 || !has2 {
		t.Errorf("snapshot = %v / %v, want [p1 p2] in both", snap.ids, snap.set)
	}
}

func TestRefresherKeepsTheOldListWhenARefreshFails(t *testing.T) {
	l := &scriptedLister{results: []listResult{{ids: []string{"p1"}}, {err: errors.New("registry down")}}}
	r := newRefresher(l, "", "", time.Second, time.Minute)
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := r.refresh(context.Background()); err == nil {
		t.Fatal("second refresh: want the registry error")
	}
	snap, err := r.active()
	if err != nil || fmt.Sprint(snap.ids) != "[p1]" {
		t.Errorf("active = %v, %v; want the old list [p1]", snap, err)
	}
}

func TestRefresherFailsClosedBeforeTheFirstSuccess(t *testing.T) {
	r := newRefresher(&scriptedLister{results: []listResult{{err: errors.New("registry down")}}}, "", "", time.Second, time.Minute)
	_ = r.refresh(context.Background())
	if _, err := r.active(); err == nil || !strings.Contains(err.Error(), "has not been loaded") {
		t.Errorf("active error = %v, want the list named as not loaded", err)
	}
}

func TestRefresherFailsClosedWhenStale(t *testing.T) {
	r := newRefresher(&scriptedLister{results: []listResult{{ids: []string{"p1"}}}}, "", "", time.Second, 60*time.Second)
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	r.now = func() time.Time { return at.Add(61 * time.Second) }
	if _, err := r.active(); err == nil || !strings.Contains(err.Error(), "older than maxStaleness") {
		t.Errorf("active error = %v, want the stale list refused", err)
	}
}

func TestRefresherRecoversAfterAFailure(t *testing.T) {
	down := listResult{err: errors.New("registry down")}
	l := &scriptedLister{results: []listResult{down, down, {ids: []string{"p1"}}}}
	r := newRefresher(l, "", "", 20*time.Millisecond, time.Minute)
	r.start()
	defer r.close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if snap, err := r.active(); err == nil && fmt.Sprint(snap.ids) == "[p1]" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the refresher did not recover within 1s")
}

func TestRefreshUsesADeadline(t *testing.T) {
	l := &scriptedLister{results: []listResult{{}}, block: true}
	r := newRefresher(l, "", "", 30*time.Millisecond, time.Minute)
	start := time.Now()
	err := r.refresh(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 200*time.Millisecond {
		t.Errorf("refresh = %v after %s, want a deadline error within 200ms", err, time.Since(start))
	}
}

func TestCloseStopsTheRefresher(t *testing.T) {
	l := &scriptedLister{results: []listResult{{ids: []string{"p1"}}}}
	r := newRefresher(l, "", "", 10*time.Millisecond, time.Minute)
	r.start()
	time.Sleep(30 * time.Millisecond)
	if err := r.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	<-r.done
	calls := l.callCount()
	time.Sleep(50 * time.Millisecond)
	if l.callCount() != calls {
		t.Errorf("lister called %d more times after close", l.callCount()-calls)
	}
	if err := r.close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}
