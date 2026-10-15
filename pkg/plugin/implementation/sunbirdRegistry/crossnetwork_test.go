package sunbirdRegistry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// admittedRecord is a network we admitted, in the given direction.
func admittedRecord(role string) participant {
	record := activeRecord()
	record.FederationRole = role
	return record
}

// calledBy is the context the adapter serves a request in: reqpreprocessor has
// already resolved who signed it.
func calledBy(caller string) context.Context {
	return context.WithValue(context.Background(), model.ContextKeyRemoteID, caller)
}

func lookupAs(t *testing.T, c *Client, ctx context.Context) ([]model.Subscription, error) {
	t.Helper()
	return c.Lookup(ctx, &model.Subscription{
		Subscriber: model.Subscriber{SubscriberID: testParticipantID},
		KeyID:      testOSID,
	})
}

func crossNetworkClient(t *testing.T, role string, on bool) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, admittedRecord(role)))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL, nil, func(cfg *Config) {
		cfg.CrossNetworkCheck = on
	})
}

func TestADirectionIsOnlyHonouredWhenTheCheckIsOn(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, model.FederationRoleSource, false),
		calledBy(testParticipantID))
	if err != nil {
		t.Fatalf("with the check off a lookup must behave as before: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the key to be returned, got %d results", len(results))
	}
}

func TestACallerWeAgreedToGiveToResolves(t *testing.T) {
	t.Parallel()

	for _, role := range []string{model.FederationRoleRequester, model.FederationRoleBoth} {
		t.Run(role, func(t *testing.T) {
			results, err := lookupAs(t, crossNetworkClient(t, role, true), calledBy(testParticipantID))
			if err != nil {
				t.Fatalf("a %s caller was refused: %v", role, err)
			}
			if len(results) != 1 {
				t.Fatalf("expected the key to be returned, got %d results", len(results))
			}
		})
	}
}

func TestACallerWeOnlyAgreedToReadFromIsRefused(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, model.FederationRoleSource, true),
		calledBy(testParticipantID))
	if err == nil {
		t.Fatal("a network we only agreed to read from was given a key to call us with")
	}
	if len(results) != 0 {
		t.Errorf("expected no key, got %d results", len(results))
	}
	if !strings.Contains(err.Error(), testParticipantID) {
		t.Errorf("error = %v, want it to name the caller", err)
	}
}

func TestARecordWithNoDirectionIsLeftAlone(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, "", true), calledBy(testParticipantID))
	if err != nil {
		t.Fatalf("a record with no federationRole was refused: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the key to be returned, got %d results", len(results))
	}
}

func TestAKeyResolvedOutsideARequestIsNotJudged(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, model.FederationRoleSource, true),
		context.Background())
	if err != nil {
		t.Fatalf("resolving a key outside a request was refused: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the key to be returned, got %d results", len(results))
	}
}

func TestOnlyTheCallerIsJudged(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, model.FederationRoleSource, true),
		calledBy("somebody-else.oan.local"))
	if err != nil {
		t.Fatalf("a lookup for a party other than the caller was refused: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the key to be returned, got %d results", len(results))
	}
}

// memoryCache is a real round trip, because this test is about what a cached
// answer skips.
type memoryCache struct {
	mu    sync.Mutex
	items map[string]string
}

func (c *memoryCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.items[key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("miss")
}

func (c *memoryCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]string{}
	}
	c.items[key] = value
	return nil
}

func (c *memoryCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	return nil
}

func (c *memoryCache) Clear(_ context.Context) error { return nil }

func TestACachedAnswerIsJudgedToo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, admittedRecord(model.FederationRoleSource)))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, &memoryCache{}, func(cfg *Config) {
		cfg.CrossNetworkCheck = true
		cfg.CacheTTL = time.Minute
	})

	if _, err := lookupAs(t, client, context.Background()); err != nil {
		t.Fatalf("warming the cache failed: %v", err)
	}
	if _, err := lookupAs(t, client, calledBy(testParticipantID)); err == nil {
		t.Fatal("a cached record skipped the direction check")
	}
}
