package federatedrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

const (
	ourID     = "provider.bharatvistar.oan.local"
	peerID    = "provider.mahavistara.oan.local"
	forwardTo = "http://localhost:9200/federation/out"
)

func newStep(t *testing.T) *Step {
	t.Helper()
	step, err := New(map[string]string{"subscriberId": ourID, "forwardUrl": forwardTo})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return step
}

// selectFor builds a select naming who should answer it, the way an experience
// layer sends one.
func selectFor(receiverKey, receiver string) []byte {
	reqContext := map[string]any{
		"action":  "select",
		"domain":  "agriculture",
		"version": "2.0.0",
	}
	if receiver != "" {
		reqContext[receiverKey] = receiver
	}
	body, err := json.Marshal(map[string]any{
		"context": reqContext,
		"message": map[string]any{"contract": map[string]any{}},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func stepContext(body []byte) *model.StepContext {
	return &model.StepContext{
		Context: context.Background(),
		Request: httptest.NewRequest(http.MethodPost, "/select", strings.NewReader(string(body))),
		Body:    body,
		// Deliberately set to the PEER. reqpreprocessor resolves this from the
		// body, so a step that trusted it would compare the receiver with
		// itself. Every test below passes only because the step ignores it.
		SubID: peerID,
	}
}

// A request naming another participant leaves this module, and leaves it by
// setting a route -- the proxy makes the call, not this step.
func TestARequestForAnotherParticipantIsRoutedOnward(t *testing.T) {
	step := newStep(t)
	ctx := stepContext(selectFor("receiverId", peerID))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Route == nil {
		t.Fatal("no route was set, so the request would be answered locally")
	}
	// The ACTION is on the end. The proxy replaces the request URL outright, so
	// without it the outbound module would be reached at its bare mount point
	// with no endpoint left for its router to match.
	if want := forwardTo + "/select"; ctx.Route.URL.String() != want {
		t.Fatalf("routed to %s, want %s", ctx.Route.URL, want)
	}
	if ctx.Route.TargetType != "url" {
		t.Fatalf("targetType = %q, want url", ctx.Route.TargetType)
	}
}

// A request for us is left alone. Same file as the test above on purpose: a fix
// aimed at either is a regression in the other.
func TestARequestForUsIsLeftToTheLocalSteps(t *testing.T) {
	step := newStep(t)
	ctx := stepContext(selectFor("receiverId", ourID))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Route != nil {
		t.Fatalf("route was set to %v; a request for us must reach the local steps", ctx.Route)
	}
}

// The decision must come from CONFIG, never from the resolved subscriber id on
// the context -- that one is read out of the body, so trusting it would compare
// the receiver with itself and make every request look local.
//
// stepContext sets SubID to the peer, so this fails loudly if that ever changes.
func TestTheDecisionIgnoresTheSubscriberIdResolvedFromTheBody(t *testing.T) {
	step := newStep(t)
	ctx := stepContext(selectFor("receiverId", peerID))
	if ctx.SubID != peerID {
		t.Fatalf("precondition: SubID = %q, want it to differ from config", ctx.SubID)
	}

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Route == nil {
		t.Fatal("the step trusted the body-resolved id and treated a peer's request as ours")
	}
}

// Every spelling of the field means the same thing.
func TestEverySpellingOfTheReceiverIsUnderstood(t *testing.T) {
	for _, key := range []string{"bpp_id", "bppId", "receiverId"} {
		t.Run(key, func(t *testing.T) {
			step := newStep(t)
			ctx := stepContext(selectFor(key, ourID))

			if err := step.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctx.Route != nil {
				t.Fatalf("%s naming us still routed away", key)
			}
		})
	}
}

// A participant id is a domain, and case is not part of its identity. Reading a
// differently-cased form as foreign would send our own traffic out.
func TestOurIdIsMatchedCaseInsensitively(t *testing.T) {
	step := newStep(t)
	ctx := stepContext(selectFor("receiverId", strings.ToUpper(ourID)))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Route != nil {
		t.Fatal("a differently-cased form of our own id was treated as foreign")
	}
}

// No receiver at all is REFUSED, not guessed.
//
// Guessing either way is worse than saying so: "ours" hides a cross-network
// request as a local 404, and "theirs" sends a local request out of the network.
func TestARequestWithNoReceiverIsRefused(t *testing.T) {
	step := newStep(t)
	ctx := stepContext(selectFor("receiverId", ""))

	err := step.Run(ctx)
	if err == nil {
		t.Fatal("a request naming no receiver was accepted")
	}
	if !strings.Contains(err.Error(), "receiverId") {
		t.Fatalf("err = %v, want it to name the missing field", err)
	}
	if !strings.Contains(err.Error(), ourID) {
		t.Fatalf("err = %v, want it to say which participant this module is", err)
	}
	if ctx.Route != nil {
		t.Fatal("a refused request was also routed")
	}
}

// A blank receiver is the same as none. Whitespace is not an identity.
func TestABlankReceiverIsRefused(t *testing.T) {
	step := newStep(t)
	ctx := stepContext(selectFor("receiverId", "   "))

	if err := step.Run(ctx); err == nil {
		t.Fatal("a whitespace receiver was accepted")
	}
}

// An unreadable body cannot name a receiver, so it is refused the same way
// rather than being silently forwarded somewhere.
func TestAnUnreadableBodyIsRefused(t *testing.T) {
	step := newStep(t)
	ctx := stepContext([]byte("{not json"))

	if err := step.Run(ctx); err == nil {
		t.Fatal("an unreadable body was accepted")
	}
}

// Without our own id every request looks foreign, so the step refuses to be
// built rather than start forwarding everything.
func TestTheStepRefusesToBuildWithoutOurSubscriberId(t *testing.T) {
	if _, err := New(map[string]string{"forwardUrl": forwardTo}); err == nil {
		t.Fatal("the step was built with no subscriberId")
	}
}

// And without somewhere to forward to, since deciding "foreign" would then have
// no action behind it.
func TestTheStepRefusesToBuildWithoutAForwardUrl(t *testing.T) {
	if _, err := New(map[string]string{"subscriberId": ourID}); err == nil {
		t.Fatal("the step was built with no forwardUrl")
	}
}

func TestTheStepRefusesAForwardUrlThatIsNotAUrl(t *testing.T) {
	if _, err := New(map[string]string{"subscriberId": ourID, "forwardUrl": "://nope"}); err == nil {
		t.Fatal("an unparseable forwardUrl was accepted")
	}
}

// A foreign request with no action has no endpoint to be forwarded to, so it is
// refused rather than sent to the outbound module's bare mount point.
func TestAForeignRequestWithNoActionIsRefused(t *testing.T) {
	step := newStep(t)
	body, err := json.Marshal(map[string]any{
		"context": map[string]any{"receiverId": peerID, "version": "2.0.0"},
		"message": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := stepContext(body)

	runErr := step.Run(ctx)
	if runErr == nil {
		t.Fatal("a foreign request with no action was accepted")
	}
	if !strings.Contains(runErr.Error(), "action") {
		t.Fatalf("err = %v, want it to name the missing action", runErr)
	}
	if ctx.Route != nil {
		t.Fatal("a refused request was also routed")
	}
}

// A trailing slash on the configured forward URL must not produce a double
// slash in the path the outbound module is reached at.
func TestATrailingSlashOnTheForwardUrlIsNormalised(t *testing.T) {
	step, err := New(map[string]string{"subscriberId": ourID, "forwardUrl": forwardTo + "/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := stepContext(selectFor("receiverId", peerID))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := forwardTo + "/select"; ctx.Route.URL.String() != want {
		t.Fatalf("routed to %s, want %s", ctx.Route.URL, want)
	}
}

// The action is READ FROM THE BODY, not assumed to be select.
//
// Every other test here sends a select, so each of them would still pass if the
// path were hardcoded. This is the one that fails if it ever is -- a federated
// init, confirm or status has to reach the peer at its own endpoint, and
// sending all four to /select would be a silent misroute.
func TestTheForwardedActionIsTheOneTheRequestCarries(t *testing.T) {
	for _, action := range []string{"select", "init", "confirm", "status"} {
		t.Run(action, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"context": map[string]any{
					"action":     action,
					"version":    "2.0.0",
					"receiverId": peerID,
				},
				"message": map[string]any{},
			})
			if err != nil {
				t.Fatal(err)
			}

			step := newStep(t)
			ctx := stepContext(body)
			if err := step.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctx.Route == nil {
				t.Fatal("no route was set")
			}
			if want := forwardTo + "/" + action; ctx.Route.URL.String() != want {
				t.Fatalf("routed to %s, want %s", ctx.Route.URL, want)
			}
		})
	}
}
