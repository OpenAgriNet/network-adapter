package catalogcrawler

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

const weatherPack = "https://openagrinet.github.io/network-specs/schema/WeatherObservation/v0.1/attributes.yaml"

func targetNetwork() model.AdmittedNetwork {
	return model.AdmittedNetwork{
		ID:           "mahavistara.oan.local",
		DiscoveryURL: "https://mahavistara.oan.local/network",
		Source:       &model.FederationDirection{SchemaPacks: []string{weatherPack}},
	}
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	return payload
}

func TestDiscoverBodyNamesBothEnds(t *testing.T) {
	t.Parallel()

	body, err := discoverBody(targetNetwork(), ourNetwork, "", 0, 20)
	if err != nil {
		t.Fatalf("discoverBody() returned an unexpected error: %v", err)
	}

	beckn, _ := decodeBody(t, body)["context"].(map[string]any)
	for field, want := range map[string]string{
		"action":     "discover",
		"senderId":   ourNetwork,
		"receiverId": targetNetwork().ID,
		"networkId":  ourNetwork,
		"version":    protocolVersion,
	} {
		if got, _ := beckn[field].(string); got != want {
			t.Errorf("context.%s = %q, want %q", field, got, want)
		}
	}
	if beckn["domain"] != nil {
		t.Error("no domain is sent; the network does not route on it")
	}
}

func TestDiscoverBodyIdentifiesEachCall(t *testing.T) {
	t.Parallel()

	first, _ := discoverBody(targetNetwork(), ourNetwork, "", 0, 20)
	second, _ := discoverBody(targetNetwork(), ourNetwork, "", 0, 20)

	one, _ := decodeBody(t, first)["context"].(map[string]any)
	two, _ := decodeBody(t, second)["context"].(map[string]any)

	for _, field := range []string{"messageId", "transactionId"} {
		if one[field] == "" || one[field] == nil {
			t.Errorf("context.%s is missing", field)
		}
		if one[field] == two[field] {
			t.Errorf("context.%s repeated between calls: %v", field, one[field])
		}
	}
	if one["timestamp"] == "" || one["timestamp"] == nil {
		t.Error("context.timestamp is missing")
	}
}

func TestDiscoverBodyAsksAgainstTheContextDocument(t *testing.T) {
	t.Parallel()

	body, _ := discoverBody(targetNetwork(), ourNetwork, "", 0, 20)
	beckn, _ := decodeBody(t, body)["context"].(map[string]any)

	got, _ := beckn["schemaContext"].([]any)
	want := "https://openagrinet.github.io/network-specs/schema/WeatherObservation/v0.1/context.jsonld"
	if len(got) != 1 || got[0] != want {
		t.Errorf("schemaContext = %v, want [%s]", got, want)
	}
}

func TestDiscoverBodyOmitsSchemaContextWhenWeTakeEverything(t *testing.T) {
	t.Parallel()

	network := targetNetwork()
	network.Source = nil

	body, _ := discoverBody(network, ourNetwork, "", 0, 20)
	if _, present := decodeBody(t, body)["context"].(map[string]any)["schemaContext"]; present {
		t.Error("an empty schemaContext narrows to nothing; it should be absent")
	}
}

func TestDiscoverBodyCarriesTheConfiguredIntent(t *testing.T) {
	t.Parallel()

	intent := `{"filters":{"type":"jsonpath","expression":"$.catalogs[*] ? (@.isActive == true)"}}`
	body, err := discoverBody(targetNetwork(), ourNetwork, intent, 0, 20)
	if err != nil {
		t.Fatalf("discoverBody() returned an unexpected error: %v", err)
	}

	message, _ := decodeBody(t, body)["message"].(map[string]any)
	got, _ := json.Marshal(message["intent"])
	if string(got) != `{"filters":{"expression":"$.catalogs[*] ? (@.isActive == true)","type":"jsonpath"}}` {
		t.Errorf("intent = %s, want the configured one", got)
	}
}

func TestDiscoverBodyRefusesAnUnreadableIntent(t *testing.T) {
	t.Parallel()

	if _, err := discoverBody(targetNetwork(), ourNetwork, "not json", 0, 20); err == nil {
		t.Error("expected an unreadable intent to be refused at build time, got none")
	}
}

func TestDiscoverURLIsTheAddressAsPublished(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ base, want string }{
		"as published": {"https://network.oan.local/federation/discovery", "https://network.oan.local/federation/discovery"},
		"our mount":    {"https://network.oan.local/network/discover", "https://network.oan.local/network/discover"},
		"padded":       {"  https://network.oan.local/network/discover  ", "https://network.oan.local/network/discover"},
		"root":         {"https://network.oan.local", "https://network.oan.local"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := discoverURL(tc.base)
			if err != nil {
				t.Fatalf("discoverURL() returned an unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("discoverURL(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

func TestDiscoverURLRefusesWhatCannotBeCalled(t *testing.T) {
	t.Parallel()

	for _, base := range []string{"", "   ", "not a url", "/network"} {
		if _, err := discoverURL(base); err == nil {
			t.Errorf("expected %q to be refused, got none", base)
		}
	}
}

// The signer takes a seed, not a full private key.
func testKey(t *testing.T) string {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("could not generate a key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(seed)
}

// collectDiscover drains the pages into one slice.
func collectDiscover(c *networkCaller, network model.AdmittedNetwork) ([]json.RawMessage, error) {
	var all []json.RawMessage
	err := c.discover(context.Background(), network, "", func(page []json.RawMessage) error {
		all = append(all, page...)
		return nil
	})
	return all, err
}

func testCaller(t *testing.T, handler http.HandlerFunc) (*networkCaller, string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	caller, err := newNetworkCaller(context.Background(), networkCallerConfig{
		networkID:  ourNetwork,
		signingKey: testKey(t),
		keyID:      "key-1",
		timeout:    2 * time.Second,
		pageSize:   DefaultFederationPageSize,
	}, quietLog())
	if err != nil {
		t.Fatalf("newNetworkCaller() returned an unexpected error: %v", err)
	}
	return caller, srv.URL
}

func answerCatalogs(count int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		catalogs := make([]map[string]any, count)
		for i := range catalogs {
			catalogs[i] = map[string]any{"id": "catalog-" + string(rune('a'+i))}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"context": map[string]any{"action": "on_discover"},
			"message": map[string]any{"catalogs": catalogs},
		})
	}
}

func TestNetworkCallReturnsWhatTheNetworkAnswered(t *testing.T) {
	t.Parallel()

	caller, url := testCaller(t, answerCatalogs(2))
	network := targetNetwork()
	network.DiscoveryURL = url

	catalogs, err := collectDiscover(caller, network)
	if err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	if len(catalogs) != 2 {
		t.Fatalf("expected 2 catalogs, got %d", len(catalogs))
	}
}

func TestNetworkCallSignsAsUs(t *testing.T) {
	t.Parallel()

	var auth string
	caller, url := testCaller(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		answerCatalogs(0)(w, r)
	})
	network := targetNetwork()
	network.DiscoveryURL = url

	if _, err := collectDiscover(caller, network); err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}

	// The whole line, not pieces of it: the far end rejects a header that drifts
	// in any part, and says nothing we can see.
	want := regexp.MustCompile(`^Signature keyId="` + regexp.QuoteMeta(ourNetwork) +
		`\|key-1\|ed25519",algorithm="ed25519",created="\d+",expires="\d+",` +
		`headers="\(created\) \(expires\) digest",signature="[^"]+"$`)
	if !want.MatchString(auth) {
		t.Errorf("Authorization header = %q, want %s", auth, want)
	}
}

func TestNetworkCallSendsToTheAddressPublished(t *testing.T) {
	t.Parallel()

	// Any path: services.discovery names the endpoint, so a network may serve
	// it from somewhere that looks nothing like our own mount.
	var path string
	caller, url := testCaller(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		answerCatalogs(0)(w, r)
	})
	network := targetNetwork()
	network.DiscoveryURL = url + "/federation/discovery"

	if _, err := collectDiscover(caller, network); err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	if path != "/federation/discovery" {
		t.Errorf("called %q, want /federation/discovery", path)
	}
}

func TestNetworkCallReportsARefusal(t *testing.T) {
	t.Parallel()

	caller, url := testCaller(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	network := targetNetwork()
	network.DiscoveryURL = url

	if _, err := collectDiscover(caller, network); err == nil {
		t.Error("expected a refusal to be reported, got none")
	}
}

func TestNetworkCallReportsAnUnreadableAnswer(t *testing.T) {
	t.Parallel()

	caller, url := testCaller(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	network := targetNetwork()
	network.DiscoveryURL = url

	if _, err := collectDiscover(caller, network); err == nil {
		t.Error("expected an unreadable answer to be reported, got none")
	}
}

func TestNetworkCallAcceptsAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	caller, url := testCaller(t, answerCatalogs(0))
	network := targetNetwork()
	network.DiscoveryURL = url

	catalogs, err := collectDiscover(caller, network)
	if err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	if len(catalogs) != 0 {
		t.Errorf("expected no catalogs, got %d", len(catalogs))
	}
}

func TestNewNetworkCallerNeedsAnIdentity(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]networkCallerConfig{
		"no network id": {signingKey: "a", keyID: "key-1"},
		"no key":        {networkID: ourNetwork, keyID: "key-1"},
		"no key id":     {networkID: ourNetwork, signingKey: "a"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newNetworkCaller(context.Background(), cfg, quietLog()); err == nil {
				t.Error("expected a refusal, got none")
			}
		})
	}
}

// pagedNetwork answers like discovery: a slice chosen by limit and offset, and
// it records every page it was asked for.
type pagedNetwork struct {
	total      int // resources, which is what discovery pages by
	perCatalog int // resources per catalog; 0 reads as one each
	mu         sync.Mutex
	asked      []string
	fail       int  // the 1-based page to refuse; 0 refuses none
	noPage     bool // answer without a pagination block, as 0.1.0 does
	stuck      bool // always report offset 0, as a network ignoring the page would
}

func (n *pagedNetwork) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, offset := pageAsked(r)

		n.mu.Lock()
		n.asked = append(n.asked, fmt.Sprintf("limit=%d&offset=%d", limit, offset))
		page := len(n.asked)
		n.mu.Unlock()

		if n.fail == page {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		per := n.perCatalog
		if per <= 0 {
			per = 1
		}
		seen := map[int]bool{}
		var ids []string
		for i := offset; i < offset+limit && i < n.total; i++ {
			if c := i / per; !seen[c] {
				seen[c] = true
				ids = append(ids, fmt.Sprintf(`{"id":"catalog-%d"}`, c))
			}
		}
		if n.noPage {
			fmt.Fprintf(w, `{"context":{"action":"on_discover"},"message":{"catalogs":[%s]}}`,
				strings.Join(ids, ","))
			return
		}
		echoed := offset
		if n.stuck {
			echoed = 0
		}
		fmt.Fprintf(w, `{"context":{"action":"on_discover"},"message":{"catalogs":[%s],`+
			`"pagination":{"limit":%d,"offset":%d,"total":%d}}}`,
			strings.Join(ids, ","), limit, echoed, n.total)
	}
}

// pageAsked reads the page out of the signed body, where discovery reads it.
func pageAsked(r *http.Request) (limit, offset int) {
	var body struct {
		Message struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		} `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 20, 0
	}
	return body.Message.Limit, body.Message.Offset
}

func pagedCaller(t *testing.T, n *pagedNetwork, pageSize int) (*networkCaller, string) {
	t.Helper()
	srv := httptest.NewServer(n.handler())
	t.Cleanup(srv.Close)

	caller, err := newNetworkCaller(context.Background(), networkCallerConfig{
		networkID:  ourNetwork,
		signingKey: testKey(t),
		keyID:      "key-1",
		timeout:    2 * time.Second,
		pageSize:   pageSize,
	}, quietLog())
	if err != nil {
		t.Fatalf("newNetworkCaller() returned an unexpected error: %v", err)
	}
	return caller, srv.URL
}

func fetchAll(t *testing.T, n *pagedNetwork, pageSize int) (int, error) {
	t.Helper()
	caller, url := pagedCaller(t, n, pageSize)
	network := targetNetwork()
	network.DiscoveryURL = url
	var collected int
	err := caller.discover(context.Background(), network, "", func(page []json.RawMessage) error {
		collected += len(page)
		return nil
	})
	return collected, err
}

func TestEveryCatalogIsCollectedAcrossPages(t *testing.T) {
	network := &pagedNetwork{total: 25}
	got, err := fetchAll(t, network, 10)
	if err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	if got != 25 {
		t.Errorf("collected %d catalogs, want all 25", got)
	}
}

func TestEachPageAsksForTheNextSlice(t *testing.T) {
	network := &pagedNetwork{total: 25}
	if _, err := fetchAll(t, network, 10); err != nil {
		t.Fatal(err)
	}
	want := []string{"limit=10&offset=0", "limit=10&offset=10", "limit=10&offset=20"}
	if len(network.asked) != len(want) {
		t.Fatalf("asked %d times %v, want %d", len(network.asked), network.asked, len(want))
	}
	for i, q := range want {
		if network.asked[i] != q {
			t.Errorf("page %d asked %q, want %q", i+1, network.asked[i], q)
		}
	}
}

func TestAShortFirstPageEndsTheWalk(t *testing.T) {
	network := &pagedNetwork{total: 3}
	got, err := fetchAll(t, network, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("collected %d, want 3", got)
	}
	if len(network.asked) != 1 {
		t.Errorf("asked %d times, want 1", len(network.asked))
	}
}

func TestAnExactlyFullLastPageIsNotFollowedByAnEmptyOne(t *testing.T) {
	network := &pagedNetwork{total: 20}
	got, err := fetchAll(t, network, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 20 {
		t.Errorf("collected %d, want 20", got)
	}
	if len(network.asked) != 2 {
		t.Errorf("asked %d times, want 2 -- the total says the second page is the last", len(network.asked))
	}
}

func TestAFailedPageFailsTheWholeFetch(t *testing.T) {
	network := &pagedNetwork{total: 25, fail: 2}
	got, err := fetchAll(t, network, 10)
	if err == nil {
		t.Fatal("a fetch that lost a page reported success")
	}
	if got != 10 {
		t.Errorf("delivered %d catalogs before the failure, want the first page", got)
	}
}

func callerWithKey(t *testing.T, key string) *networkCaller {
	t.Helper()
	caller, err := newNetworkCaller(context.Background(), networkCallerConfig{
		networkID: ourNetwork, signingKey: key, keyID: "key-1", timeout: 2 * time.Second,
	}, quietLog())
	if err != nil {
		t.Fatalf("newNetworkCaller: %v", err)
	}
	return caller
}

func TestANetworkThatCannotBeReachedIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := srv.URL
	srv.Close()

	network := targetNetwork()
	network.DiscoveryURL = closed

	_, err := collectDiscover(callerWithKey(t, testKey(t)), network)
	if err == nil {
		t.Fatal("an unreachable network was reported as having shared nothing")
	}
	if !strings.Contains(err.Error(), network.ID) {
		t.Errorf("error %q does not name the network that could not be reached", err)
	}
}

func TestANetworkWithNoUsableAddressIsNeverAsked(t *testing.T) {
	network := targetNetwork()
	network.DiscoveryURL = "not-an-address"

	if _, err := collectDiscover(callerWithKey(t, testKey(t)), network); err == nil {
		t.Fatal("a network with no usable address was reported as having shared nothing")
	}
}

func TestAnIntentThatIsNotJSONStopsTheCall(t *testing.T) {
	caller, url := testCaller(t, answerCatalogs(1))
	network := targetNetwork()
	network.DiscoveryURL = url

	err := caller.discover(context.Background(), network, `{"filters":`, func([]json.RawMessage) error {
		t.Error("a page was handed over despite the intent being unreadable")
		return nil
	})
	if err == nil {
		t.Fatal("an unreadable intent was sent as if it were empty")
	}
}

func TestAKeyThatCannotSignStopsTheCall(t *testing.T) {
	caller, url := testCaller(t, answerCatalogs(1))
	caller.cfg.signingKey = "not-base64!!"
	network := targetNetwork()
	network.DiscoveryURL = url

	if _, err := collectDiscover(caller, network); err == nil {
		t.Fatal("a discover went out unsigned, which the far end would refuse")
	}
}

func TestAPageTheHandlerRejectsStopsTheWalk(t *testing.T) {
	pages := &pagedNetwork{total: 25}
	caller, url := pagedCaller(t, pages, 10)
	network := targetNetwork()
	network.DiscoveryURL = url

	err := caller.discover(context.Background(), network, "", func([]json.RawMessage) error {
		return errors.New("queue full")
	})
	if err == nil {
		t.Fatal("a rejected page was reported as a complete fetch")
	}
	if len(pages.asked) != 1 {
		t.Errorf("asked %d times, want the walk to stop at the first rejected page", len(pages.asked))
	}
}

func TestOnlyTheSubsetWeTakeIsAskedFor(t *testing.T) {
	network := targetNetwork()
	network.Source = &model.FederationDirection{SchemaPacks: []string{
		"https://openagrinet.github.io/network-specs/schema/MandiPrice/v0.1/attributes.yaml",
		"https://openagrinet.github.io/network-specs/schema/WeatherObservation/v0.1/attributes.yaml",
	}}

	body, err := discoverBody(network, ourNetwork, "", 0, 20)
	if err != nil {
		t.Fatalf("discoverBody: %v", err)
	}
	got := decodeBody(t, body)["context"].(map[string]any)["schemaContext"].([]any)
	want := []string{
		"https://openagrinet.github.io/network-specs/schema/MandiPrice/v0.1/context.jsonld",
		"https://openagrinet.github.io/network-specs/schema/WeatherObservation/v0.1/context.jsonld",
	}
	if len(got) != len(want) {
		t.Fatalf("schemaContext = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("schemaContext[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestTakingNoSubsetAsksForEverything(t *testing.T) {
	network := targetNetwork()
	network.Source = nil

	body, err := discoverBody(network, ourNetwork, "", 0, 20)
	if err != nil {
		t.Fatalf("discoverBody: %v", err)
	}
	if _, narrowed := decodeBody(t, body)["context"].(map[string]any)["schemaContext"]; narrowed {
		t.Error("the discover was narrowed with no subset recorded, so a network's other packs would never arrive")
	}
}

func TestThePageIsAskedForInTheSignedBody(t *testing.T) {
	body, err := discoverBody(targetNetwork(), ourNetwork, "", 40, 80)
	if err != nil {
		t.Fatalf("discoverBody: %v", err)
	}
	message := decodeBody(t, body)["message"].(map[string]any)
	if message["limit"] != float64(80) {
		t.Errorf("limit = %v, want 80", message["limit"])
	}
	if message["offset"] != float64(40) {
		t.Errorf("offset = %v, want 40", message["offset"])
	}
}

func TestTheFirstPageSaysOffsetZeroRatherThanOmittingIt(t *testing.T) {
	body, err := discoverBody(targetNetwork(), ourNetwork, "", 0, 80)
	if err != nil {
		t.Fatalf("discoverBody: %v", err)
	}
	message := decodeBody(t, body)["message"].(map[string]any)
	if _, named := message["offset"]; !named {
		t.Error("offset was omitted, and absent falls back to the query string we no longer send")
	}
}

func TestThePageIsNotInTheQueryString(t *testing.T) {
	var asked string
	caller, url := testCaller(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		answerCatalogs(1)(w, r)
	})
	network := targetNetwork()
	network.DiscoveryURL = url

	if _, err := collectDiscover(caller, network); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if asked != "" {
		t.Errorf("query string = %q, want none -- a signature does not cover it", asked)
	}
}

func TestTheWalkFollowsTheTotalNotTheCatalogCount(t *testing.T) {
	t.Parallel()

	n := &pagedNetwork{total: 10, perCatalog: 5}
	got, err := fetchAll(t, n, 5)
	if err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("collected %d catalogs, want 2 -- the walk stopped on a page holding fewer catalogs than the limit", got)
	}
	if len(n.asked) != 2 {
		t.Errorf("asked for %v, want both pages", n.asked)
	}
}

func TestANetworkThatReportsNoPageIsStillWalked(t *testing.T) {
	t.Parallel()

	n := &pagedNetwork{total: 25, noPage: true}
	got, err := fetchAll(t, n, 10)
	if err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	if got != 25 {
		t.Errorf("collected %d, want 25", got)
	}
}

func TestANetworkStuckOnOnePageIsNotWalkedForever(t *testing.T) {
	t.Parallel()

	n := &pagedNetwork{total: 100, stuck: true}
	if _, err := fetchAll(t, n, 10); err != nil {
		t.Fatalf("discover() returned an unexpected error: %v", err)
	}
	// Two: the second page is what shows the offset did not move.
	if len(n.asked) != 2 {
		t.Errorf("asked %d times, want 2 -- a network that never advances must not be walked forever", len(n.asked))
	}
}
