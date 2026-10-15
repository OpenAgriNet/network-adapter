package sunbirdRegistry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	testNetworkID  = "mahavistara.oan.local"
	testNetworkURL = "https://mahavistara.oan.local/network/discover"
	testPackURL    = "https://openagrinet.github.io/network-specs/schema/WeatherObservation/v0.1/attributes.yaml"
)

// networkRecord is an admitted network as the registry stores one.
func networkRecord(tweak ...func(*participant)) participant {
	p := participant{
		ParticipantID:  testNetworkID,
		Type:           "node",
		Role:           roleList{"network"},
		Status:         "active",
		BaseURL:        testNetworkURL,
		FederationRole: federationRole{Source: &federationDirection{SchemaPacks: []string{testPackURL}}},
		ProjectionTtl:  3600,
		Keys:           []key{signingKey()},
	}
	for _, apply := range tweak {
		apply(&p)
	}
	return p
}

// serveRecords answers every search with the same records.
func serveRecords(t *testing.T, records ...participant) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, records...))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL, nil)
}

func TestAdmittedNetworksReadsTheRecord(t *testing.T) {
	t.Parallel()

	networks, err := serveRecords(t, networkRecord()).AdmittedNetworks(context.Background())
	if err != nil {
		t.Fatalf("AdmittedNetworks() returned an unexpected error: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(networks))
	}
	got := networks[0]
	if got.ID != testNetworkID {
		t.Errorf("ID = %q, want %q", got.ID, testNetworkID)
	}
	if got.DiscoveryURL != testNetworkURL {
		t.Errorf("DiscoveryURL = %q, want %q", got.DiscoveryURL, testNetworkURL)
	}
	if !got.IsSource() || got.IsRequester() {
		t.Errorf("agreed %v, want source alone", got.Directions())
	}
	if got.ProjectionTtl != time.Hour {
		t.Errorf("ProjectionTtl = %v, want %v -- seconds are converted once, here", got.ProjectionTtl, time.Hour)
	}
	if packs := got.SourcePacks(); len(packs) != 1 || packs[0] != testPackURL {
		t.Errorf("SourcePacks() = %v, want [%s]", packs, testPackURL)
	}
}

func TestAdmittedNetworksOmitsWhatIsNotAnAdmittedNetwork(t *testing.T) {
	t.Parallel()

	for name, tweak := range map[string]func(*participant){
		"suspended":      func(p *participant) { p.Status = "inactive" },
		"not a network":  func(p *participant) { p.Role = roleList{"provider"} },
		"no discoverUrl": func(p *participant) { p.BaseURL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			networks, err := serveRecords(t, networkRecord(tweak)).AdmittedNetworks(context.Background())
			if err != nil {
				t.Fatalf("AdmittedNetworks() returned an unexpected error: %v", err)
			}
			if len(networks) != 0 {
				t.Errorf("expected no networks, got %d: %+v", len(networks), networks)
			}
		})
	}
}

func TestAdmittedNetworksOmitsAnUnrecognisedRelationship(t *testing.T) {
	t.Parallel()

	networks, err := serveRecords(t, networkRecord(func(p *participant) {
		p.FederationRole = federationRole{}
	})).AdmittedNetworks(context.Background())
	if err != nil {
		t.Fatalf("AdmittedNetworks() returned an unexpected error: %v", err)
	}
	if len(networks) != 0 {
		t.Errorf("expected no networks with no direction agreed, got %+v", networks)
	}
}

func TestAdmittedNetworksReturnsEveryDirection(t *testing.T) {
	t.Parallel()

	packs := &federationDirection{SchemaPacks: []string{testPackURL}}
	for name, agreed := range map[string]federationRole{
		"source":    {Source: packs},
		"requester": {Requester: packs},
		"both":      {Source: packs, Requester: packs},
	} {
		t.Run(name, func(t *testing.T) {
			networks, err := serveRecords(t, networkRecord(func(p *participant) {
				p.FederationRole = agreed
			})).AdmittedNetworks(context.Background())
			if err != nil {
				t.Fatalf("AdmittedNetworks() returned an unexpected error: %v", err)
			}
			if len(networks) != 1 {
				t.Fatalf("expected 1 network, got %d", len(networks))
			}
			if networks[0].IsSource() != (agreed.Source != nil) ||
				networks[0].IsRequester() != (agreed.Requester != nil) {
				t.Errorf("agreed %v, want %s", networks[0].Directions(), name)
			}
		})
	}
}

func TestAdmittedNetworkFindsOneByID(t *testing.T) {
	t.Parallel()

	network, err := serveRecords(t, networkRecord()).AdmittedNetwork(context.Background(), testNetworkID)
	if err != nil {
		t.Fatalf("AdmittedNetwork() returned an unexpected error: %v", err)
	}
	if network == nil {
		t.Fatal("expected a network, got nil")
	}
	if network.ID != testNetworkID {
		t.Errorf("ID = %q, want %q", network.ID, testNetworkID)
	}
}

func TestAdmittedNetworkReportsAnUnknownNetworkAsNil(t *testing.T) {
	t.Parallel()

	network, err := serveRecords(t).AdmittedNetwork(context.Background(), testNetworkID)
	if err != nil {
		t.Fatalf("AdmittedNetwork() returned an unexpected error: %v", err)
	}
	if network != nil {
		t.Errorf("expected nil for a network we never admitted, got %+v", network)
	}
}

func TestAdmittedNetworkLookupsReportAnUnreachableRegistry(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, nil)

	if _, err := c.AdmittedNetworks(context.Background()); err == nil {
		t.Error("AdmittedNetworks(): expected an error when the registry cannot be asked")
	}
	if _, err := c.AdmittedNetwork(context.Background(), testNetworkID); err == nil {
		t.Error("AdmittedNetwork(): expected an error when the registry cannot be asked")
	}
}

func TestAdmittedNetworksAsksTheRegistryForNetworks(t *testing.T) {
	t.Parallel()

	var got searchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, recordJSON(t))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv.URL, nil).AdmittedNetworks(context.Background()); err != nil {
		t.Fatalf("AdmittedNetworks() returned an unexpected error: %v", err)
	}
	if got.Filters["role"].Contains != roleNetwork {
		t.Errorf("role filter = %+v, want contains %q -- role is a list, and eq matches no list",
			got.Filters["role"], roleNetwork)
	}
}

// Not parallel: otel.SetMeterProvider is global. See TestEmitMetricsSuccessPartition.
func TestAdmittedLookupsEmitMetrics(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		operation string
		call      func(*Client) error
	}{
		{
			name:      "listing networks",
			operation: operationAdmittedNetworks,
			call: func(c *Client) error {
				_, err := c.AdmittedNetworks(ctx)
				return err
			},
		},
		{
			name:      "finding one network",
			operation: operationAdmittedNetwork,
			call: func(c *Client) error {
				_, err := c.AdmittedNetwork(ctx, testNetworkID)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := otel.GetMeterProvider()
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			otel.SetMeterProvider(mp)
			t.Cleanup(func() {
				otel.SetMeterProvider(previous)
				_ = mp.Shutdown(ctx)
			})

			if err := tc.call(serveRecords(t, networkRecord())); err != nil {
				t.Fatalf("lookup returned an unexpected error: %v", err)
			}

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(ctx, &rm); err != nil {
				t.Fatalf("failed to collect metrics: %v", err)
			}
			if !recordedOperation(rm, tc.operation) {
				t.Errorf("expected a duration recorded for operation %q", tc.operation)
			}
		})
	}
}

func recordedOperation(rm metricdata.ResourceMetrics, operation string) bool {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			hist, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, point := range hist.DataPoints {
				if value, found := point.Attributes.Value(telemetry.AttrOperation); found && value.AsString() == operation {
					return true
				}
			}
		}
	}
	return false
}

func TestANetworkWithNoIDIsNotLookedUp(t *testing.T) {
	t.Parallel()

	asked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked = true
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	network, err := newTestClient(t, srv.URL, nil).AdmittedNetwork(context.Background(), "   ")
	if err != nil {
		t.Fatalf("AdmittedNetwork: %v", err)
	}
	if network != nil {
		t.Errorf("resolved %+v from a blank network id", network)
	}
	if asked {
		t.Error("the registry was searched for a network with no id, which matches any record missing one")
	}
}

// The shape the registry actually returns, written out rather than marshalled
// from our own struct, so a disagreement with the schema shows up here.
func TestTheRecordShapeTheRegistryReturnsIsRead(t *testing.T) {
	t.Parallel()

	const wire = `[{
	  "osid": "1-19087a97-f886-4fe4-bf14-3875437dc6f8",
	  "osCreatedAt": "2026-10-13T08:08:24.648Z",
	  "osUpdatedAt": "2026-10-13T08:08:24.648Z",
	  "participantId": "mahavistara.oan.local",
	  "name": "Maha Vistara",
	  "type": "node",
	  "status": "active",
	  "baseUrl": "https://mahavistara.oan.local/network",
	  "role": ["network"],
	  "projectionTtl": 3600,
	  "keys": [{"osid": "1-dd305f4d", "alg": "ed25519",
	            "key": "base64:XQaAtQx9ngpPzyjsHomjMqurr8TNighVe2MICMdhs8s=",
	            "status": "active", "validFrom": "2026-01-01T00:00:00Z"}],
	  "federationRole": {
	    "source": {
	      "osid": "1-7b2e91c4",
	      "schemaPacks": ["https://x/MandiPrice/v0.1/attributes.yaml"],
	      "descriptorRevision": 2,
	      "admittedAt": "2026-10-13T08:08:24Z",
	      "updatedAt": "2026-10-20T11:02:00Z"
	    }
	  }
	}]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, wire)
	}))
	defer srv.Close()

	networks, err := newTestClient(t, srv.URL, nil).AdmittedNetworks(context.Background())
	if err != nil {
		t.Fatalf("AdmittedNetworks: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("read %d networks from a record the registry would return, want 1", len(networks))
	}
	got := networks[0]
	if !got.IsSource() || got.IsRequester() {
		t.Errorf("agreed %v, want source alone", got.Directions())
	}
	if packs := got.SourcePacks(); len(packs) != 1 || packs[0] != "https://x/MandiPrice/v0.1/attributes.yaml" {
		t.Errorf("SourcePacks() = %v, want the one pack the agreement names", packs)
	}
	if got.ProjectionTtl != time.Hour {
		t.Errorf("ProjectionTtl = %v, want an hour", got.ProjectionTtl)
	}
}
