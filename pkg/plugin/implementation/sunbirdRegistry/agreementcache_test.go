package sunbirdRegistry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// redisLikeCache answers a miss like the cache plugin: empty value, no error.
type redisLikeCache struct{ *storingCache }

func (c redisLikeCache) Get(ctx context.Context, key string) (string, error) {
	value, err := c.storingCache.Get(ctx, key)
	if err != nil {
		return "", nil
	}
	return value, nil
}

// countingAgreementRegistry serves serve's record (a 500 when nil) and counts requests.
func countingAgreementRegistry(t *testing.T, requests *atomic.Int32, serve func(n int32) *participant) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := serve(requests.Add(1))
		if record == nil {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, recordJSON(t, *record))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cachingClient(t *testing.T, url string, ttl time.Duration) *Client {
	t.Helper()
	return newTestClient(t, url, redisLikeCache{newStoringCache()}, func(c *Config) { c.CacheTTL = ttl })
}

func TestAnAgreementIsAskedForOnce(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := admittedRecord(federationRole{Requester: agreedPacks()})
	srv := countingAgreementRegistry(t, &requests, func(int32) *participant { return &record })
	client := cachingClient(t, srv.URL, 30*time.Second)

	for i := 0; i < 2; i++ {
		if err := discoverFor(t, client, testAgreedContext); err != nil {
			t.Fatalf("call %d: a requester asking within the agreement must be allowed: %v", i+1, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("registry asked %d times, want 1: the second call must come from the cache", got)
	}
}

func TestNoAgreementIsCachedToo(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := activeRecord()
	srv := countingAgreementRegistry(t, &requests, func(int32) *participant { return &record })
	client := cachingClient(t, srv.URL, 30*time.Second)

	for i := 0; i < 2; i++ {
		if err := discoverFor(t, client, testAgreedContext); err != nil {
			t.Fatalf("call %d: a participant with no agreement must be left alone: %v", i+1, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("registry asked %d times, want 1: \"no agreement\" is the common answer and must be cached", got)
	}
}

func TestAnUnknownCallerIsNotCached(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, recordJSON(t))
	}))
	t.Cleanup(srv.Close)
	client := cachingClient(t, srv.URL, 30*time.Second)

	for i := 0; i < 2; i++ {
		if err := discoverFor(t, client, testAgreedContext); err != nil {
			t.Fatalf("call %d: an unknown caller has no agreement to refuse on: %v", i+1, err)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("registry asked %d times, want 2: unknown callers must not be cached", got)
	}
}

func TestARefusalIsAlwaysJudgedOnFreshData(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := admittedRecord(federationRole{Source: agreedPacks()})
	srv := countingAgreementRegistry(t, &requests, func(int32) *participant { return &record })
	client := cachingClient(t, srv.URL, 30*time.Second)

	for i := 0; i < 2; i++ {
		if err := discoverFor(t, client, testAgreedContext); err == nil {
			t.Fatalf("call %d: a network we only read from must be refused, cached or not", i+1)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("registry asked %d times, want 2: a refusal must not rest on a cached answer", got)
	}
}

func TestAReinstatedNetworkIsServedWithoutWaitingOutTheTTL(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	var reinstated atomic.Bool
	readOnly := admittedRecord(federationRole{Source: agreedPacks()})
	giving := admittedRecord(federationRole{Requester: agreedPacks()})
	srv := countingAgreementRegistry(t, &requests, func(int32) *participant {
		if reinstated.Load() {
			return &giving
		}
		return &readOnly
	})
	client := cachingClient(t, srv.URL, 30*time.Second)

	if err := discoverFor(t, client, testAgreedContext); err == nil {
		t.Fatal("before reinstatement the network must be refused")
	}
	reinstated.Store(true)
	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("a reinstated network must be served at once, not after the TTL: %v", err)
	}
	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("and stay served: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("registry asked %d times, want 2: the refreshed answer must be cached again", got)
	}
}

func TestACachedRefusalStandsWhenTheRegistryCannotRecheckIt(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := admittedRecord(federationRole{Source: agreedPacks()})
	srv := countingAgreementRegistry(t, &requests, func(n int32) *participant {
		if n == 1 {
			return &record
		}
		return nil
	})
	client := cachingClient(t, srv.URL, 30*time.Second)

	if err := discoverFor(t, client, testAgreedContext); err == nil {
		t.Fatal("a network we only read from must be refused")
	}
	var coded *model.CodedErr
	err := discoverFor(t, client, testAgreedContext)
	if !errors.As(err, &coded) {
		t.Fatalf("with the recheck failing, the cached refusal must stand, got %T: %v", err, err)
	}
	if coded.HTTPStatus() != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 -- the caller's refusal, not our outage", coded.HTTPStatus())
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("registry asked %d times, want 2: the recheck must still be tried", got)
	}
}

func TestARegistryFailureIsNotCached(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := activeRecord()
	srv := countingAgreementRegistry(t, &requests, func(n int32) *participant {
		if n == 1 {
			return nil
		}
		return &record
	})
	client := cachingClient(t, srv.URL, 30*time.Second)

	if err := discoverFor(t, client, testAgreedContext); err == nil {
		t.Fatal("a registry we could not ask must refuse")
	}
	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("once the registry answers, the earlier failure must not be remembered: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("registry asked %d times, want 2", got)
	}
}

func TestWithoutACacheTTLEveryCallAsks(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := activeRecord()
	srv := countingAgreementRegistry(t, &requests, func(int32) *participant { return &record })
	client := cachingClient(t, srv.URL, 0)

	for i := 0; i < 2; i++ {
		if err := discoverFor(t, client, testAgreedContext); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("registry asked %d times, want 2: cacheTTL 0 means no caching", got)
	}
}

func TestAnUnreadableAgreementEntryIsAMiss(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	record := admittedRecord(federationRole{Source: agreedPacks()})
	srv := countingAgreementRegistry(t, &requests, func(int32) *participant { return &record })
	cache := redisLikeCache{newStoringCache()}
	cache.entries[agreementCacheKey(testParticipantID)] = "not json"
	client := newTestClient(t, srv.URL, cache, func(c *Config) { c.CacheTTL = 30 * time.Second })

	if err := discoverFor(t, client, testAgreedContext); err == nil {
		t.Fatal("an unreadable entry must be ignored and the registry's refusal applied")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("registry asked %d times, want 1", got)
	}
}
