package sunbirdRegistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// fakeRecords is a registry cache holding the given records per entity,
// matching on top-level string fields.
type fakeRecords map[string][]any

func (f fakeRecords) List(_ context.Context, entity string, match map[string]string) ([]json.RawMessage, error) {
	var found []json.RawMessage
	for _, record := range f[entity] {
		raw, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		matched := true
		for field, want := range match {
			if fields[field] != want {
				matched = false
			}
		}
		if matched {
			found = append(found, raw)
		}
	}
	return found, nil
}

func (f fakeRecords) Get(ctx context.Context, entity, field, value string) (json.RawMessage, error) {
	found, err := f.List(ctx, entity, map[string]string{field: value})
	if err != nil {
		return nil, err
	}
	if len(found) != 1 {
		return nil, definition.ErrRegistryRecordNotFound
	}
	return found[0], nil
}

// countingSearches answers every search with body and counts the calls.
func countingSearches(t *testing.T, body string) (string, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func TestThroughTheRegistryCacheALookupAsksNoRegistry(t *testing.T) {
	url, calls := countingSearches(t, `[]`)
	cache := &mockCache{}
	client := newTestClient(t, url, cache, func(c *Config) { c.CacheTTL = time.Minute })
	client.UseRegistryCache(fakeRecords{DefaultEntity: {activeRecord()}})

	results, err := lookup(t, client)
	if err != nil || len(results) != 1 || results[0].Status != statusSubscribed {
		t.Fatalf("got %+v, %v, want the cached participant's key", results, err)
	}
	if *calls != 0 {
		t.Fatalf("registry asked %d times, want none", *calls)
	}
	if cache.getCalls != 0 || cache.setCalls != 0 {
		t.Fatalf("own answer cache used (get %d, set %d), want it replaced", cache.getCalls, cache.setCalls)
	}
}

func TestThroughTheRegistryCacheAnAbsentParticipantIsNotFound(t *testing.T) {
	url, calls := countingSearches(t, recordJSON(t, activeRecord()))
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{})

	results, err := lookup(t, client)
	if err != nil || len(results) != 0 {
		t.Fatalf("got %+v, %v, want not found", results, err)
	}
	if *calls != 0 {
		t.Fatalf("registry asked %d times, want the registry cache to answer", *calls)
	}
}

func TestThroughTheRegistryCacheAProviderRecordAsksNoRegistry(t *testing.T) {
	url, calls := countingSearches(t, `[]`)
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{
		DefaultProviderEntity: {bindingRecord()},
		DefaultEntity:         {upstreamRecord()},
	})

	plan, err := resolvePlan(t, client)
	if err != nil || plan.BaseURL != testBaseURL {
		t.Fatalf("got %+v, %v, want the plan from the cached records", plan, err)
	}
	if *calls != 0 {
		t.Fatalf("registry asked %d times, want none", *calls)
	}
}

func TestThroughTheRegistryCacheADuplicatedBindingStillServes(t *testing.T) {
	url, _ := countingSearches(t, `[]`)
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{
		DefaultProviderEntity: {bindingRecord(), bindingRecord()},
		DefaultEntity:         {upstreamRecord()},
	})

	if _, err := resolvePlan(t, client); err != nil {
		t.Fatalf("a duplicated binding must be served from the first, as without the cache: %v", err)
	}
}

func TestThroughTheRegistryCacheARefusalIsRecheckedWithTheRegistry(t *testing.T) {
	// The cached copy predates the agreement to give; the registry has it.
	cached := admittedRecord(federationRole{Source: agreedPacks()})
	fresh := admittedRecord(federationRole{Requester: agreedPacks()})
	url, calls := countingSearches(t, recordJSON(t, fresh))
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{DefaultEntity: {cached}})

	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("an agreement made since the copy must not be refused: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("registry asked %d times, want one recheck", *calls)
	}
}

func TestThroughTheRegistryCacheAnAllowedCallerAsksNoRegistry(t *testing.T) {
	url, calls := countingSearches(t, `[]`)
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{DefaultEntity: {admittedRecord(federationRole{Requester: agreedPacks()})}})

	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Fatalf("registry asked %d times, want none", *calls)
	}
}

func TestThroughTheRegistryCacheARefusalTheRegistryConfirmsStands(t *testing.T) {
	record := admittedRecord(federationRole{Source: agreedPacks()})
	url, _ := countingSearches(t, recordJSON(t, record))
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{DefaultEntity: {record}})

	if err := discoverFor(t, client, testAgreedContext); err == nil {
		t.Fatal("a network we only read from must still be refused")
	}
}

func TestThroughTheRegistryCacheAnUnreadableCacheIsAnError(t *testing.T) {
	url, _ := countingSearches(t, `[]`)
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(failingRecords{})

	if _, err := lookup(t, client); err == nil {
		t.Fatal("a registry cache that cannot answer must surface as an error, not a not-found")
	}
}

type failingRecords struct{}

func (failingRecords) List(context.Context, string, map[string]string) ([]json.RawMessage, error) {
	return nil, errors.New("registry down")
}
func (failingRecords) Get(context.Context, string, string, string) (json.RawMessage, error) {
	return nil, errors.New("registry down")
}
