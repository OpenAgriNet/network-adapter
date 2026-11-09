package registrycache

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memCache map[string]string

func (m memCache) Get(_ context.Context, k string) (string, error) { return m[k], nil }
func (m memCache) Set(_ context.Context, k, v string, _ time.Duration) error {
	m[k] = v
	return nil
}
func (m memCache) Delete(_ context.Context, k string) error { delete(m, k); return nil }
func (m memCache) Clear(context.Context) error              { return nil }

// lockedCache is a memCache safe for the loops Start runs.
type lockedCache struct {
	mu   sync.Mutex
	m    memCache
	down bool // every read fails, as when the cache cannot be reached
}

func (c *lockedCache) Get(ctx context.Context, k string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return "", errors.New("cache down")
	}
	return c.m.Get(ctx, k)
}
func (c *lockedCache) Set(ctx context.Context, k, v string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m.Set(ctx, k, v, ttl)
}
func (c *lockedCache) Delete(ctx context.Context, k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m.Delete(ctx, k)
}
func (c *lockedCache) Clear(context.Context) error { return nil }

// registry answers every search with *answer and counts the calls.
func registry(t *testing.T, answer *string) (*Store, memCache, *int32) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/api/v1/Participant/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(*answer))
	}))
	t.Cleanup(srv.Close)
	cache := memCache{}
	s, err := New(cache, Config{URL: srv.URL + "/api/v1", TTL: 24 * time.Hour, Preload: []string{"Participant"}})
	if err != nil {
		t.Fatal(err)
	}
	return s, cache, &calls
}

func ids(t *testing.T, raw string) []string {
	var records []struct {
		ParticipantID string `json:"participantId"`
	}
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		t.Fatalf("cached list %q: %v", raw, err)
	}
	var out []string
	for _, r := range records {
		out = append(out, r.ParticipantID)
	}
	return out
}

func TestPreloadCachesEveryActiveRecordAsOneList(t *testing.T) {
	answer := `{"data":[
		{"participantId":"oan-a","status":"active"},
		{"participantId":"oan-b","status":"inactive"},
		{"participantId":"oan-c","status":"Active"},
		{"name":"no id","status":"active"}
	]}`
	s, cache, _ := registry(t, &answer)

	s.load(context.Background(), "Participant")

	if len(cache) != 1 {
		t.Fatalf("cache = %v, want only the list", cache)
	}
	if got := ids(t, cache[s.listKey("Participant")]); len(got) != 3 || got[0] != "oan-a" || got[1] != "oan-c" {
		t.Fatalf("list = %v, want oan-a, oan-c and the record without an id", got)
	}
}

func TestListMatchesFieldsAndListFields(t *testing.T) {
	answer := `[
		{"participantId":"pocra","type":"upstream","status":"active"},
		{"participantId":"oan-dev","type":"node","role":["consumer","network"],"status":"active"},
		{"participantId":"oan-x","type":"node","role":["network"],"status":"inactive"}
	]`
	s, _, _ := registry(t, &answer)
	ctx := context.Background()
	s.load(ctx, "Participant")

	for _, tc := range []struct {
		match map[string]string
		want  string
	}{
		{map[string]string{"type": "upstream"}, "pocra"},
		{map[string]string{"role": "network"}, "oan-dev"},
		{map[string]string{"type": "node", "role": "consumer"}, "oan-dev"},
	} {
		got, err := s.List(ctx, "Participant", tc.match)
		if err != nil || len(got) != 1 || !matches(got[0], map[string]string{"participantId": tc.want}) {
			t.Fatalf("%v: got %s, %v, want only %s", tc.match, got, err, tc.want)
		}
	}
	if got, _ := s.List(ctx, "Participant", map[string]string{"type": "shop"}); len(got) != 0 {
		t.Fatalf("got %s, want nothing", got)
	}
}

func TestListWithNothingCachedAsksNoRegistry(t *testing.T) {
	answer := `[{"participantId":"pocra","status":"active"}]`
	s, _, calls := registry(t, &answer)

	if _, err := s.List(context.Background(), "Participant", nil); !errors.Is(err, ErrNotCached) {
		t.Fatalf("err=%v, want ErrNotCached", err)
	}
	if *calls != 0 {
		t.Fatalf("registry called %d times: a request must not fetch every record", *calls)
	}
}

func TestAnUnreadableCacheIsNotReadAsAMiss(t *testing.T) {
	answer := `[{"participantId":"pocra","status":"active"}]`
	s, _, calls := registry(t, &answer)
	s.cache = &lockedCache{m: memCache{}, down: true}

	if _, err := s.List(context.Background(), "Participant", nil); err == nil || errors.Is(err, ErrNotCached) {
		t.Fatalf("err=%v, want the cache's own error", err)
	}
	if *calls != 0 {
		t.Fatalf("registry called %d times, want none", *calls)
	}
}

func TestAFailedPreloadKeepsWhatIsCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	cache := memCache{}
	s, _ := New(cache, Config{URL: srv.URL, TTL: time.Hour, Preload: []string{"Participant"}})
	cache[s.listKey("Participant")] = "kept"

	s.load(context.Background(), "Participant")

	if cache[s.listKey("Participant")] != "kept" {
		t.Fatalf("cache = %v", cache)
	}
}

func TestStartReloadsEveryTTLUntilStopped(t *testing.T) {
	answer := `[{"participantId":"oan-a","status":"active"}]`
	s, _, calls := registry(t, &answer)
	s.cache = &lockedCache{m: memCache{}}
	s.ttl = 10 * time.Millisecond
	s.check = 5 * time.Millisecond

	stop := s.Start(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(calls) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if atomic.LoadInt32(calls) < 3 {
		t.Fatalf("loaded %d times, want a reload every TTL", *calls)
	}
	time.Sleep(30 * time.Millisecond)
	after := atomic.LoadInt32(calls)
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(calls) != after {
		t.Fatal("still reloading after stop")
	}
}

func TestAFailedLoadIsRetriedSoonAndOnlyForThatEntity(t *testing.T) {
	var participant, provider int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ProviderSchema/search" {
			atomic.AddInt32(&provider, 1)
			w.Write([]byte(`[]`))
			return
		}
		if atomic.AddInt32(&participant, 1) == 1 {
			http.Error(w, "slow to wake", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`[{"participantId":"pocra","status":"active"}]`))
	}))
	t.Cleanup(srv.Close)
	s, _ := New(&lockedCache{m: memCache{}}, Config{URL: srv.URL, TTL: time.Hour, Preload: []string{"Participant", "ProviderSchema"}})
	s.check = 10 * time.Millisecond

	stop := s.Start(context.Background())
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&participant) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if n := atomic.LoadInt32(&participant); n != 2 {
		t.Fatalf("Participant asked %d times, want one failure and one retry", n)
	}
	if n := atomic.LoadInt32(&provider); n != 1 {
		t.Fatalf("ProviderSchema asked %d times, want once: it did not fail", n)
	}
}

// waitFor polls until calls reaches n, then lets a few more checks run.
func waitFor(t *testing.T, calls *int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(calls) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestAListGoneFromTheCacheIsReloadedWithinACheck(t *testing.T) {
	answer := `[{"participantId":"pocra","status":"active"}]`
	s, _, calls := registry(t, &answer)
	cache := &lockedCache{m: memCache{}}
	s.cache, s.check = cache, 10*time.Millisecond

	stop := s.Start(context.Background())
	defer stop()
	waitFor(t, calls, 1)
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("registry asked %d times while the list was cached, want only the first load", n)
	}

	// Expired while the host slept, or the cache restarted.
	cache.Delete(context.Background(), s.listKey("Participant"))
	waitFor(t, calls, 2)
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("registry asked %d times, want one reload of the missing list", n)
	}
	if raw, _ := cache.Get(context.Background(), s.listKey("Participant")); raw == "" {
		t.Fatal("list not refilled")
	}
}

func TestAnUnreachableCacheDoesNotSendChecksToTheRegistry(t *testing.T) {
	answer := `[{"participantId":"pocra","status":"active"}]`
	s, _, calls := registry(t, &answer)
	s.cache = &lockedCache{m: memCache{}, down: true}
	s.check = 5 * time.Millisecond

	stop := s.Start(context.Background())
	defer stop()
	waitFor(t, calls, 1)
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("registry asked %d times, want only the first load", n)
	}
}

func TestARefreshReloadsAtTheNextCheckOnly(t *testing.T) {
	answer := `[{"participantId":"pocra","status":"active"}]`
	s, _, calls := registry(t, &answer)
	s.cache = &lockedCache{m: memCache{}}
	s.check = 10 * time.Millisecond

	stop := s.Start(context.Background())
	defer stop()
	waitFor(t, calls, 1)

	// However often it is asked, one reload.
	for i := 0; i < 5; i++ {
		s.Refresh("Participant")
	}
	s.Refresh("NotPreloaded")
	waitFor(t, calls, 2)
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("registry asked %d times, want the first load and one refresh", n)
	}
}

func TestListsOfDifferentRegistriesKeepApartInOneCache(t *testing.T) {
	cache := memCache{}
	a, _ := New(cache, Config{URL: "http://registry-a/api/v1", TTL: time.Hour})
	b, _ := New(cache, Config{URL: "http://registry-b/api/v1", TTL: time.Hour})
	a.storeList(context.Background(), "Participant", []json.RawMessage{json.RawMessage(`{"participantId":"a"}`)})

	if _, err := b.List(context.Background(), "Participant", nil); !errors.Is(err, ErrNotCached) {
		t.Fatalf("err=%v, want registry b not to read registry a's list", err)
	}
}
