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

const testAgreedPack = "https://x/MandiPrice/v0.1/attributes.yaml"
const testAgreedContext = "https://x/MandiPrice/v0.1/context.jsonld"

// admittedRecord is a network we admitted, in the given directions. A pack list
// is required on a real record, so each direction carries one.
func admittedRecord(agreed federationRole) participant {
	record := activeRecord()
	record.FederationRole = agreed
	return record
}

func agreedPacks() *federationDirection {
	return &federationDirection{SchemaPacks: []string{testAgreedPack}}
}

// askedFor is the context a served discover carries: who signed it, and the
// capabilities their body named.
func askedFor(caller string, contexts ...string) context.Context {
	return actionAskedFor(caller, discoverAction, contexts...)
}

func actionAskedFor(caller, action string, contexts ...string) context.Context {
	body := []byte(`{"context":{"action":"` + action + `","senderId":"` + caller + `"}}`)
	if contexts != nil {
		quoted := make([]string, 0, len(contexts))
		for _, c := range contexts {
			quoted = append(quoted, `"`+c+`"`)
		}
		body = []byte(`{"context":{"action":"` + action + `","senderId":"` + caller +
			`","schemaContext":[` + strings.Join(quoted, ",") + `]}}`)
	}
	return &model.StepContext{Context: calledBy(caller), Body: body}
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

func crossNetworkClient(t *testing.T, role federationRole, on bool) *Client {
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

	results, err := lookupAs(t, crossNetworkClient(t, federationRole{Source: agreedPacks()}, false),
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

	for name, role := range map[string]federationRole{
		"requester": {Requester: agreedPacks()},
		"both":      {Source: agreedPacks(), Requester: agreedPacks()},
	} {
		t.Run(name, func(t *testing.T) {
			results, err := lookupAs(t, crossNetworkClient(t, role, true),
				askedFor(testParticipantID, testAgreedContext))
			if err != nil {
				t.Fatalf("a %s caller asking within the agreement was refused: %v", name, err)
			}
			if len(results) != 1 {
				t.Fatalf("expected the key to be returned, got %d results", len(results))
			}
		})
	}
}

func TestACallerWeOnlyAgreedToReadFromIsRefused(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, federationRole{Source: agreedPacks()}, true),
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

	results, err := lookupAs(t, crossNetworkClient(t, federationRole{}, true), calledBy(testParticipantID))
	if err != nil {
		t.Fatalf("a record with no federationRole was refused: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected the key to be returned, got %d results", len(results))
	}
}

func TestAKeyResolvedOutsideARequestIsNotJudged(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, federationRole{Source: agreedPacks()}, true),
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

	results, err := lookupAs(t, crossNetworkClient(t, federationRole{Source: agreedPacks()}, true),
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
		fmt.Fprint(w, recordJSON(t, admittedRecord(federationRole{Source: agreedPacks()})))
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

func TestADiscoverNamingNoCapabilityIsRefused(t *testing.T) {
	t.Parallel()

	for name, ctx := range map[string]context.Context{
		"no schemaContext": askedFor(testParticipantID),
		"empty list":       askedFor(testParticipantID, []string{}...),
		"blank entry":      askedFor(testParticipantID, "  "),
	} {
		t.Run(name, func(t *testing.T) {
			results, err := lookupAs(t, crossNetworkClient(t, federationRole{Requester: agreedPacks()}, true), ctx)
			if err == nil {
				t.Fatal("a discover naming no capability asks for everything and must be refused")
			}
			if len(results) != 0 {
				t.Errorf("expected no key, got %d results", len(results))
			}
		})
	}
}

func TestADiscoverAskingOutsideTheAgreementIsRefused(t *testing.T) {
	t.Parallel()

	results, err := lookupAs(t, crossNetworkClient(t, federationRole{Requester: agreedPacks()}, true),
		askedFor(testParticipantID, testAgreedContext, "https://x/Weather/v0.1/context.jsonld"))
	if err == nil {
		t.Fatal("a capability outside the agreement was given a key")
	}
	if len(results) != 0 {
		t.Errorf("expected no key, got %d results", len(results))
	}
	if strings.Contains(err.Error(), "Weather") {
		t.Errorf("error = %v, want it not to name what we hold", err)
	}
}

func TestOnlyADiscoverIsCheckedAgainstTheAgreedSubset(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"select", "init", "confirm", "status"} {
		t.Run(action, func(t *testing.T) {
			results, err := lookupAs(t, crossNetworkClient(t, federationRole{Requester: agreedPacks()}, true),
				actionAskedFor(testParticipantID, action))
			if err != nil {
				t.Fatalf("a %s from a network we agreed to give to was refused: %v", action, err)
			}
			if len(results) != 1 {
				t.Fatalf("expected the key to be returned, got %d results", len(results))
			}
		})
	}
}

func TestASelectFromANetworkWeOnlyReadFromIsStillRefused(t *testing.T) {
	t.Parallel()

	_, err := lookupAs(t, crossNetworkClient(t, federationRole{Source: agreedPacks()}, true),
		actionAskedFor(testParticipantID, "select"))
	if err == nil {
		t.Fatal("the direction still governs an action that names no capability")
	}
}
