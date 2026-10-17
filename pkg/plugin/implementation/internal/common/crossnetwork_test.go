package common_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

const (
	us   = "bharatvistar.oan.local"
	them = "mahavistara.oan.local"

	// Both networks front the same upstream, so they hold the SAME binding key.
	sharedBinding = "agmarknet.bharatvistar|openagrinet:MandiPrice"
)

// crossRequest names a receiver, an address and an action -- the three fields
// the decision rests on. The capability half stays constant.
func crossRequest(receiver, bppURI, action string) []byte {
	field := func(name, value string) string {
		if value == "" {
			return ""
		}
		return fmt.Sprintf(`, %q: %q`, name, value)
	}
	return []byte(fmt.Sprintf(`{
  "context": { "version": "2.0.0"%s%s%s },
  "message": { "contract": { "commitments": [ {
    "resources": [ { "resourceAttributes": { "@type": "openagrinet:MandiPrice" } } ],
    "offer": { "provider": { "id": "agmarknet.bharatvistar" } }
  } ] } }
}`, field("action", action), field("receiverId", receiver), field("bppUri", bppURI)))
}

// networkStub answers both lookups the step makes: the call plan, and the
// networks we admitted. One registry client serves both in the adapter too.
type networkStub struct {
	plan     *model.ProviderRecord
	networks []model.AdmittedNetwork
	err      error
}

func (s *networkStub) ProviderRecord(context.Context, string) (*model.ProviderRecord, error) {
	return s.plan, nil
}

func (s *networkStub) AdmittedNetworks(context.Context) ([]model.AdmittedNetwork, error) {
	return s.networks, s.err
}

// agreedPacks is what a direction covers. Any pack will do here; these tests
// are about the direction, not the subset.
func agreedPacks() *model.FederationDirection {
	return &model.FederationDirection{SchemaPacks: []string{"https://x/MandiPrice/v0.1/attributes.yaml"}}
}

// admittedAs is the step with one network on the books in the given direction.
func admittedAs(t *testing.T, agreed model.Agreement) definition.Step {
	t.Helper()
	return crossStepWith(t, "http://unused.invalid", &networkStub{
		plan:     &model.ProviderRecord{BindingKey: sharedBinding},
		networks: []model.AdmittedNetwork{{ID: them, Agreement: agreed}},
	})
}

func crossStep(t *testing.T, upstreamURL string) definition.Step {
	t.Helper()
	plan := &model.ProviderRecord{
		BindingKey: sharedBinding,
		BaseURL:    upstreamURL,
		Actions: map[string]model.ActionPlan{
			"select": {Method: http.MethodGet, Path: "/x", Mappings: dispatchMappingRef, RetryMax: 1},
		},
	}
	return crossStepWith(t, upstreamURL, &networkStub{
		plan: plan,
		networks: []model.AdmittedNetwork{
			{ID: them, Agreement: model.Agreement{Source: agreedPacks(), Requester: agreedPacks()}},
		},
	})
}

func crossStepWith(t *testing.T, upstreamURL string, registry definition.ProviderRecordLookup) definition.Step {
	t.Helper()
	step, closer, err := common.New(context.Background(),
		registry,
		fixedMapper{answer: `{"served":"locally"}`},
		nil,
		&common.Config{
			BindingKeys: []string{sharedBinding},
			AuthByProvider: map[string]*common.AuthProfile{
				"agmarknet.bharatvistar": {Scheme: util.AuthSchemeNone},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer() })
	return step
}

func TestRequestForUsIsServedHere(t *testing.T) {
	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		fmt.Fprint(w, `{"from":"upstream"}`)
	}))
	defer upstream.Close()

	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us, Body: crossRequest(us, "https://"+them+"/provider", "select")}
	if err := crossStep(t, upstream.URL).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("a request addressed to us did not reach our upstream")
	}
	if ctx.Route != nil {
		t.Errorf("Route = %#v, want nil -- our own request was sent away", ctx.Route)
	}
}

func TestRequestForAnotherNetworkIsSentThere(t *testing.T) {
	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer upstream.Close()

	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us, Body: crossRequest(them, "https://"+them+"/provider", "select")}
	if err := crossStep(t, upstream.URL).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("another network's request was answered with our data")
	}
	if ctx.Route == nil {
		t.Fatal("Route = nil, want the network's address")
	}
	if got := ctx.Route.URL.String(); got != "https://"+them+"/provider/select" {
		t.Errorf("Route.URL = %q, want the network's address with the action on it", got)
	}
	if ctx.SubID != us {
		t.Errorf("SubID = %q, want %q -- the forwarded call must be signed as us", ctx.SubID, us)
	}
}

func TestEveryActionCrossesTheSameWay(t *testing.T) {
	for _, action := range []string{"select", "init", "confirm", "status"} {
		t.Run(action, func(t *testing.T) {
			ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us, Body: crossRequest(them, "https://"+them+"/provider", action)}
			if err := crossStep(t, "http://unused.invalid").Run(ctx); err != nil {
				t.Fatal(err)
			}
			if ctx.Route == nil {
				t.Fatal("Route = nil, want the network's address")
			}
			if got := ctx.Route.URL.String(); got != "https://"+them+"/provider/"+action {
				t.Errorf("Route.URL = %q, want the action appended", got)
			}
		})
	}
}

func TestRequestNamingNoReceiverIsRefused(t *testing.T) {
	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us, Body: crossRequest("", "", "select")}
	err := crossStep(t, "http://unused.invalid").Run(ctx)
	if err == nil {
		t.Fatal("a request naming no receiver was accepted; it must not be assumed ours")
	}
	if !strings.Contains(err.Error(), "receiverId") {
		t.Errorf("error = %v, want it to name the field to set", err)
	}
}

func TestRequestForAnotherNetworkWithNoAddressIsRefused(t *testing.T) {
	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us, Body: crossRequest(them, "", "select")}
	err := crossStep(t, "http://unused.invalid").Run(ctx)
	if err == nil {
		t.Fatal("a request for another network with no bppUri was accepted")
	}
	if !strings.Contains(err.Error(), "bppUri") {
		t.Errorf("error = %v, want it to name the missing address", err)
	}
}

func TestStepWithoutAnIdentityRefusesToDecide(t *testing.T) {
	ctx := &model.StepContext{Context: t.Context(), Body: crossRequest(them, "https://"+them+"/provider", "select")}
	err := crossStep(t, "http://unused.invalid").Run(ctx)
	if err == nil {
		t.Fatal("a step with no subscriberId decided whose request this was")
	}
	if !strings.Contains(err.Error(), "subscriberId") {
		t.Errorf("error = %v, want it to name the missing identity", err)
	}
}

func TestASecondStepLeavesAnAlreadyRoutedRequestAlone(t *testing.T) {
	var called bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer upstream.Close()

	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us, Body: crossRequest(us, "https://"+them+"/provider", "select")}
	ctx.Route = &model.Route{TargetType: "url"}
	if err := crossStep(t, upstream.URL).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a step called its upstream for a request already routed away")
	}
}

func TestForwardingNeedsTheReceiverAdmittedAsASource(t *testing.T) {
	for _, tc := range []struct {
		role      string
		agreed    model.Agreement
		forwarded bool
	}{
		{"source", model.Agreement{Source: agreedPacks()}, true},
		{"both", model.Agreement{Source: agreedPacks(), Requester: agreedPacks()}, true},
		{"requester", model.Agreement{Requester: agreedPacks()}, false},
	} {
		t.Run(tc.role, func(t *testing.T) {
			ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us,
				Body: crossRequest(them, "https://"+them+"/provider", "select")}
			err := admittedAs(t, tc.agreed).Run(ctx)
			if tc.forwarded {
				if err != nil {
					t.Fatalf("a %s network was not forwarded to: %v", tc.role, err)
				}
				if ctx.Route == nil {
					t.Error("Route = nil, want the network's address")
				}
				return
			}
			if err == nil {
				t.Fatalf("a %s-only network was forwarded to; we never agreed to read from them", tc.role)
			}
			if ctx.Route != nil {
				t.Errorf("Route = %#v, want nil", ctx.Route)
			}
		})
	}
}

func TestForwardingRefusesANetworkWeNeverAdmitted(t *testing.T) {
	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us,
		Body: crossRequest("stranger.oan.local", "https://stranger.oan.local/provider", "select")}
	err := crossStepWith(t, "http://unused.invalid", &networkStub{
		plan: &model.ProviderRecord{BindingKey: sharedBinding}}).Run(ctx)
	if err == nil {
		t.Fatal("a request for a network we never admitted was forwarded")
	}
	if !strings.Contains(err.Error(), "stranger.oan.local") {
		t.Errorf("error = %v, want it to name the network", err)
	}
}

func TestForwardingRefusesWhenTheRegistryCannotBeAsked(t *testing.T) {
	ctx := &model.StepContext{Context: t.Context(), OwnSubscriberID: us,
		Body: crossRequest(them, "https://"+them+"/provider", "select")}
	err := crossStepWith(t, "http://unused.invalid", &networkStub{
		plan: &model.ProviderRecord{BindingKey: sharedBinding},
		err:  fmt.Errorf("registry unreachable")}).Run(ctx)
	if err == nil {
		t.Fatal("a network was forwarded to without checking we admitted them")
	}
}
