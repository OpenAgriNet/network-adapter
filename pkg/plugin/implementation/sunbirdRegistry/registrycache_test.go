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

func (fakeRecords) Refresh(string) {}

// watchedRecords is fakeRecords recording which entities it was asked to refresh.
type watchedRecords struct {
	fakeRecords
	refreshed []string
}

func (w *watchedRecords) Refresh(entity string) { w.refreshed = append(w.refreshed, entity) }

func wantRefreshed(t *testing.T, w *watchedRecords, want ...string) {
	t.Helper()
	if fmt.Sprint(w.refreshed) != fmt.Sprint(want) {
		t.Fatalf("refreshed %v, want %v", w.refreshed, want)
	}
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

func TestThroughTheRegistryCacheAParticipantNotCachedIsAskedOfTheRegistry(t *testing.T) {
	// Joined since the last load.
	url, calls := countingSearches(t, recordJSON(t, activeRecord()))
	client := newTestClient(t, url, nil)
	records := &watchedRecords{fakeRecords: fakeRecords{}}
	client.UseRegistryCache(records)

	results, err := lookup(t, client)
	if err != nil || len(results) != 1 || results[0].Status != statusSubscribed {
		t.Fatalf("got %+v, %v, want the registry's participant", results, err)
	}
	if *calls != 1 {
		t.Fatalf("registry asked %d times, want once", *calls)
	}
	wantRefreshed(t, records, DefaultEntity)
}

func TestThroughTheRegistryCacheARotatedKeyIsRecheckedWithTheRegistry(t *testing.T) {
	// The cached copy predates the key the participant now signs with.
	cached := activeRecord()
	cached.Keys[0].OSID = "1-retired"
	url, calls := countingSearches(t, recordJSON(t, activeRecord()))
	client := newTestClient(t, url, nil)
	records := &watchedRecords{fakeRecords: fakeRecords{DefaultEntity: {cached}}}
	client.UseRegistryCache(records)

	results, err := lookup(t, client)
	if err != nil || len(results) != 1 || results[0].Status != statusSubscribed {
		t.Fatalf("got %+v, %v, want the rotated key to verify", results, err)
	}
	if *calls != 1 {
		t.Fatalf("registry asked %d times, want one recheck", *calls)
	}
	wantRefreshed(t, records, DefaultEntity)
}

func TestThroughTheRegistryCacheAnActionAddedSinceIsServed(t *testing.T) {
	// The cached binding predates its only active action.
	cached := bindingRecord()
	cached.Actions[0].Status = "inactive"
	srv := newRegistryServer(t, arrayJSON(t, bindingRecord()), arrayJSON(t, upstreamRecord()))
	t.Cleanup(srv.Close)
	client := newTestClient(t, srv.URL, nil)
	records := &watchedRecords{fakeRecords: fakeRecords{
		DefaultProviderEntity: {cached},
		DefaultEntity:         {upstreamRecord()},
	}}
	client.UseRegistryCache(records)

	if _, err := resolvePlan(t, client); err != nil {
		t.Fatalf("a binding refused from its cached copy must be rechecked: %v", err)
	}
	wantRefreshed(t, records, DefaultProviderEntity, DefaultEntity)
}

func TestThroughTheRegistryCacheAnInactiveNetworkIsStillRefused(t *testing.T) {
	// Not in the cache, which holds active records only. Read as "no
	// agreement", it would pass as an ordinary participant.
	record := admittedRecord(federationRole{Source: agreedPacks()})
	record.Status = "inactive"
	url, _ := countingSearches(t, recordJSON(t, record))
	client := newTestClient(t, url, nil)
	records := &watchedRecords{fakeRecords: fakeRecords{}}
	client.UseRegistryCache(records)

	if err := discoverFor(t, client, testAgreedContext); err == nil {
		t.Fatal("a network we only read from must be refused, inactive or not")
	}
	// An inactive record is not missing from the cache: nothing to refresh.
	wantRefreshed(t, records)
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
	records := &watchedRecords{fakeRecords: fakeRecords{DefaultEntity: {cached}}}
	client.UseRegistryCache(records)

	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("an agreement made since the copy must not be refused: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("registry asked %d times, want one recheck", *calls)
	}
	wantRefreshed(t, records, DefaultEntity)
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

func TestThroughTheRegistryCacheAnUnreadableCacheFallsBackToTheRegistry(t *testing.T) {
	url, calls := countingSearches(t, recordJSON(t, activeRecord()))
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(failingRecords{})

	results, err := lookup(t, client)
	if err != nil || len(results) != 1 || results[0].Status != statusSubscribed {
		t.Fatalf("got %+v, %v, want the registry's answer while the cache cannot give one", results, err)
	}
	if *calls != 1 {
		t.Fatalf("registry asked %d times, want once", *calls)
	}
}

type failingRecords struct{}

func (failingRecords) List(context.Context, string, map[string]string) ([]json.RawMessage, error) {
	return nil, errors.New("registry down")
}

func (failingRecords) Refresh(string) {}

func TestThroughTheRegistryCacheAnUnknownCallerCostsOneRegistryCall(t *testing.T) {
	url, calls := countingSearches(t, `[]`)
	client := newTestClient(t, url, nil)
	records := &watchedRecords{fakeRecords: fakeRecords{}}
	client.UseRegistryCache(records)

	if results, err := lookup(t, client); err != nil || len(results) != 0 {
		t.Fatalf("got %+v, %v, want not found", results, err)
	}
	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("an unknown caller is an ordinary participant here: %v", err)
	}
	// One search each: a refusal the registry gave is not asked again.
	if *calls != 2 {
		t.Fatalf("registry asked %d times, want once per check", *calls)
	}
	wantRefreshed(t, records)
}

func TestThroughTheRegistryCacheAnUnreadableCachedRecordFallsBackToTheRegistry(t *testing.T) {
	url, calls := countingSearches(t, recordJSON(t, activeRecord()))
	client := newTestClient(t, url, nil)
	client.UseRegistryCache(fakeRecords{DefaultEntity: {map[string]any{"participantId": testParticipantID, "keys": "not a list"}}})

	results, err := lookup(t, client)
	if err != nil || len(results) != 1 || results[0].Status != statusSubscribed {
		t.Fatalf("got %+v, %v, want the registry's answer", results, err)
	}
	if *calls != 1 {
		t.Fatalf("registry asked %d times, want once", *calls)
	}
}
