package common

import (
	"context"
	"net/http"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

const (
	us   = "karnataka.oan.dev"
	peer = "maharashtra.oan.dev"
)

// Built directly rather than through New: routeElsewhere is reached before
// anything a registry or mapper is needed for, and the point of these tests is
// that the decision rests on nothing but the body and our own id.
func routeStep(subscriberID string) *Step {
	return &Step{config: &Config{SubscriberID: subscriberID}}
}

func route(t *testing.T, body string) (*model.StepContext, bool, error) {
	t.Helper()
	ctx := &model.StepContext{Context: context.Background(), Body: []byte(body)}
	routed, err := routeStep(us).routeElsewhere(ctx)
	return ctx, routed, err
}

func badRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("routeElsewhere(): want a refusal, got nil")
	}
	coded, ok := err.(*model.CodedErr)
	if !ok {
		t.Fatalf("want *model.CodedErr, got %T", err)
	}
	if got := coded.HTTPStatus(); got != http.StatusBadRequest {
		t.Errorf("status: want %d, got %d", http.StatusBadRequest, got)
	}
}

// Without an identity the check cannot tell our own request from anyone else's.
// It refuses rather than assuming "ours" -- the handler supplies this from the
// module's subscriberId, so an empty one is a misconfigured module, not a
// request to serve.
func TestRefusesWithoutSubscriberID(t *testing.T) {
	ctx := &model.StepContext{
		Context: context.Background(),
		Body:    []byte(`{"context":{"receiverId":"` + us + `","action":"select"}}`),
	}
	if _, err := routeStep("").routeElsewhere(ctx); err == nil {
		t.Fatal("want an error when the module declared no subscriberId, got nil")
	}
}

// A request for us carries on into the binding check with NO route set, so the
// capability steps behave exactly as they did before this check was added.
func TestOursPassesThrough(t *testing.T) {
	for name, body := range map[string]string{
		"receiverId": `{"context":{"receiverId":"` + us + `","action":"select"}}`,
		"bppId":      `{"context":{"bppId":"` + us + `","action":"select"}}`,
		"bpp_id":     `{"context":{"bpp_id":"` + us + `","action":"select"}}`,
		"case":       `{"context":{"receiverId":"KARNATAKA.OAN.DEV","action":"select"}}`,
		// Ours even though a bppUri is present: the receiver decides, and a
		// local catalog's bppUri points back at this deployment, so routing by
		// it would send the request out and straight back in.
		"ownBppUri": `{"context":{"receiverId":"` + us + `","bppUri":"http://provider:9200","action":"select"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, routed, err := route(t, body)
			if err != nil {
				t.Fatalf("routeElsewhere(): want nil, got %v", err)
			}
			if routed {
				t.Fatal("want our own request carried on locally, got it routed away")
			}
			if ctx.Route != nil {
				t.Fatalf("Route: want nil for our own request, got %#v", ctx.Route)
			}
			if ctx.SubID != "" {
				t.Errorf("SubID: want it left alone for our own request, got %q", ctx.SubID)
			}
		})
	}
}

// The case this check exists for. Both networks front the same upstream, so the
// binding key matches here too and the handler's 404 never fires -- without
// this the peer's request is answered with our data, under their name.
func TestPeersRequestGoesToItsBppUri(t *testing.T) {
	for name, tc := range map[string]struct{ bppURI, want string }{
		"bare":        {"https://maharashtra.oan.dev", "https://maharashtra.oan.dev/select"},
		"withPath":    {"https://maharashtra.oan.dev/beckn", "https://maharashtra.oan.dev/beckn/select"},
		"trailing":    {"https://maharashtra.oan.dev/beckn/", "https://maharashtra.oan.dev/beckn/select"},
		"withPort":    {"http://peer-adapter:9200", "http://peer-adapter:9200/select"},
		"receiverUri": {"https://maharashtra.oan.dev", "https://maharashtra.oan.dev/select"},
	} {
		t.Run(name, func(t *testing.T) {
			field := "bppUri"
			if name == "receiverUri" {
				field = "receiverUri"
			}
			ctx, routed, err := route(t, `{"context":{"receiverId":"`+peer+`","`+field+`":"`+tc.bppURI+`","action":"select"}}`)
			if err != nil {
				t.Fatalf("routeElsewhere(): want nil, got %v", err)
			}
			if !routed {
				t.Fatal("want the peer's request routed away, got it served locally")
			}
			if ctx.Route == nil {
				t.Fatal("Route: want a route to the peer, got nil")
			}
			if ctx.Route.TargetType != "url" {
				t.Errorf("TargetType: want url, got %q", ctx.Route.TargetType)
			}
			if got := ctx.Route.URL.String(); got != tc.want {
				t.Errorf("URL: want %q, got %q", tc.want, got)
			}
			// The network signs, not the peer the request names.
			if ctx.SubID != us {
				t.Errorf("SubID: want %q so the sign step signs as us, got %q", us, ctx.SubID)
			}
		})
	}
}

// Every action, not just select. Hardcoding the endpoint would pass a
// select-only test and break init, confirm and status in the stack.
func TestActionComesFromTheRequest(t *testing.T) {
	for _, action := range []string{"select", "init", "confirm", "status"} {
		t.Run(action, func(t *testing.T) {
			ctx, _, err := route(t, `{"context":{"receiverId":"`+peer+
				`","bppUri":"https://peer.example","action":"`+action+`"}}`)
			if err != nil {
				t.Fatalf("routeElsewhere(): %v", err)
			}
			want := "https://peer.example/" + action
			if got := ctx.Route.URL.String(); got != want {
				t.Errorf("URL: want %q, got %q", want, got)
			}
		})
	}
}

// Refused, not assumed. Guessing "ours" here is exactly the failure this check
// exists to prevent, and guessing a destination is worse.
func TestRefusedWhenItCannotDecideOrSend(t *testing.T) {
	for name, body := range map[string]string{
		"noReceiver":  `{"context":{"action":"select"}}`,
		"blankField":  `{"context":{"receiverId":"   ","action":"select"}}`,
		"noContext":   `{"message":{}}`,
		"notJSON":     `not json`,
		"noBppUri":    `{"context":{"receiverId":"` + peer + `","action":"select"}}`,
		"brokenUri":   `{"context":{"receiverId":"` + peer + `","bppUri":"not a url","action":"select"}}`,
		"relativeUri": `{"context":{"receiverId":"` + peer + `","bppUri":"/beckn","action":"select"}}`,
		"noAction":    `{"context":{"receiverId":"` + peer + `","bppUri":"https://peer.example"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := route(t, body)
			badRequest(t, err)
		})
	}
}

// The trap, pinned. reqpreprocessor resolves the module's own id FROM THE BODY
// (it reads bppId/receiverId), so a rewrite that compares the receiver against
// ctx.SubID would compare the field with itself: always equal, nothing ever
// routed away. SubID is set to the PEER here, so that rewrite fails this test
// instead of silently answering every peer's request with our data.
func TestDoesNotCompareAgainstContextSubID(t *testing.T) {
	ctx := &model.StepContext{
		Context: context.Background(),
		Body:    []byte(`{"context":{"receiverId":"` + peer + `","bppUri":"https://peer.example","action":"select"}}`),
		SubID:   peer,
	}
	if _, err := routeStep(us).routeElsewhere(ctx); err != nil {
		t.Fatalf("routeElsewhere(): %v", err)
	}
	if ctx.Route == nil {
		t.Fatal("a peer's request was served locally; the check is reading its own id from the body")
	}
	if ctx.SubID != us {
		t.Fatalf("SubID: want %q, got %q -- the routed request would be signed as the peer, "+
			"and the keyset lookup would fail", us, ctx.SubID)
	}
}

// An earlier step having already decided wins over everything here: Run returns
// before the check, so a request someone else routed is not re-decided.
func TestRunStopsAtAnExistingRoute(t *testing.T) {
	ctx := &model.StepContext{
		Context: context.Background(),
		Body:    []byte(`{"context":{"receiverId":"` + peer + `","action":"select"}}`),
		Route:   &model.Route{TargetType: "url"},
	}
	// No bppUri and not ours, so without the guard this refuses.
	if err := routeStep(us).Run(ctx); err != nil {
		t.Fatalf("Run(): want nil once a route is already set, got %v", err)
	}
}
