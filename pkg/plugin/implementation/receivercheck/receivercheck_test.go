package receivercheck

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

func step(t *testing.T) *Step {
	t.Helper()
	s, err := New(map[string]string{"subscriberId": us})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return s
}

func run(t *testing.T, body string) (*model.StepContext, error) {
	t.Helper()
	ctx := &model.StepContext{Context: context.Background(), Body: []byte(body)}
	return ctx, step(t).Run(ctx)
}

func badRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Run(): want a refusal, got nil")
	}
	coded, ok := err.(*model.CodedErr)
	if !ok {
		t.Fatalf("want *model.CodedErr, got %T", err)
	}
	if got := coded.HTTPStatus(); got != http.StatusBadRequest {
		t.Errorf("status: want %d, got %d", http.StatusBadRequest, got)
	}
}

func TestNewRequiresSubscriberID(t *testing.T) {
	for _, raw := range []map[string]string{{}, {"subscriberId": "   "}} {
		if _, err := New(raw); err == nil {
			t.Fatalf("New(%v): want an error, got nil", raw)
		}
	}
}

// A request for us is passed through untouched and NO route is set, so the
// capability steps behave exactly as they did before this step was added.
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
			ctx, err := run(t, body)
			if err != nil {
				t.Fatalf("Run(): want nil, got %v", err)
			}
			if ctx.Route != nil {
				t.Fatalf("Route: want nil for our own request, got %#v", ctx.Route)
			}
		})
	}
}

// The case this step exists for. Both networks front the same upstream, so the
// binding key matches here too and the handler's 404 never fires -- without
// this step the peer's request is answered with our data, under their name.
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
			ctx, err := run(t, `{"context":{"receiverId":"`+peer+`","`+field+`":"`+tc.bppURI+`","action":"select"}}`)
			if err != nil {
				t.Fatalf("Run(): want nil, got %v", err)
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
		})
	}
}

// Every action, not just select. Hardcoding the endpoint would pass a
// select-only test and break init, confirm and status in the stack.
func TestActionComesFromTheRequest(t *testing.T) {
	for _, action := range []string{"select", "init", "confirm", "status"} {
		t.Run(action, func(t *testing.T) {
			ctx, err := run(t, `{"context":{"receiverId":"`+peer+
				`","bppUri":"https://peer.example","action":"`+action+`"}}`)
			if err != nil {
				t.Fatalf("Run(): %v", err)
			}
			want := "https://peer.example/" + action
			if got := ctx.Route.URL.String(); got != want {
				t.Errorf("URL: want %q, got %q", want, got)
			}
		})
	}
}

// Refused, not assumed. Guessing "ours" here is exactly the failure the step
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
			_, err := run(t, body)
			badRequest(t, err)
		})
	}
}

// The trap, pinned. reqpreprocessor resolves the module's own id FROM THE BODY
// (it reads bppId/receiverId), so a rewrite that compares the receiver against
// ctx.SubID would compare the field with itself: always equal, nothing ever
// forwarded. SubID is set to the PEER here, so that rewrite fails this test
// instead of silently answering every peer's request with our data.
func TestDoesNotCompareAgainstContextSubID(t *testing.T) {
	ctx := &model.StepContext{
		Context: context.Background(),
		Body:    []byte(`{"context":{"receiverId":"` + peer + `","bppUri":"https://peer.example","action":"select"}}`),
		SubID:   peer,
	}
	if err := step(t).Run(ctx); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if ctx.Route == nil {
		t.Fatal("a peer's request was served locally; the check is reading its own id from the body")
	}
}
