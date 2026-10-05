package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

const ourNetwork = "bharat-vistar"

// selectFor builds a select body the way an Experience layer sends one: the
// network and the provider endpoint both in the context, which is where Beckn
// v2 puts them.
func selectFor(networkID, bppURI string) []byte {
	reqContext := map[string]any{
		"action":  "select",
		"domain":  "agriculture",
		"version": "2.0.0",
	}
	if networkID != "" {
		reqContext["networkId"] = networkID
	}
	if bppURI != "" {
		reqContext["bppUri"] = bppURI
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

func stepContextFor(t *testing.T, body []byte) *model.StepContext {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/bpp/receiver/select", strings.NewReader(string(body)))
	return &model.StepContext{Context: context.Background(), Request: request, Body: body}
}

// peerServing stands in for the other network's adapter, and records what
// actually reached it.
type peerServing struct {
	*httptest.Server
	calls int
	path  string
	body  []byte
	auth  string
}

func newPeerServing(t *testing.T, status int, answer string) *peerServing {
	t.Helper()
	peer := &peerServing{}
	peer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.calls++
		peer.path = r.URL.Path
		peer.auth = r.Header.Get(model.AuthHeaderSubscriber)
		buf := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(buf); err != nil && r.ContentLength > 0 {
			// io.EOF on a full read is normal; the bytes are what matter.
			_ = err
		}
		peer.body = buf
		w.WriteHeader(status)
		fmt.Fprint(w, answer)
	}))
	t.Cleanup(peer.Close)
	return peer
}

func newStepFor(t *testing.T) *federatedStep {
	t.Helper()
	step, err := newFederatedStep(ourNetwork, time.Second)
	if err != nil {
		t.Fatalf("newFederatedStep: %v", err)
	}
	return step
}

// Step 1 -- a select for another network is not resolved locally. It leaves
// this adapter, carrying the body unchanged, and the peer's answer comes back
// in place of the ACK.
func TestASelectForAnotherNetworkGoesToThatNetwork(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{"ok":"from the peer"}`)
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("maha-vistar", peer.URL))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.calls != 1 {
		t.Fatalf("peer was called %d times, want exactly 1", peer.calls)
	}
	if peer.path != "/select" {
		t.Fatalf("peer was called at %q, want the action from the context", peer.path)
	}
	if string(ctx.ResponseBody) != `{"ok":"from the peer"}` {
		t.Fatalf("response = %q, want the peer's answer verbatim", ctx.ResponseBody)
	}
}

// Step 2 -- a select for US is untouched, and lives in the same file as Step 1
// deliberately: a fix aimed at either is a regression in the other.
func TestASelectForUsIsLeftToTheLocalSteps(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{"ok":"should never be reached"}`)
	step := newStepFor(t)
	// bppUri IS present and points at the peer. A request for our own network
	// must still not be sent there -- networkId decides, not the URI.
	ctx := stepContextFor(t, selectFor(ourNetwork, peer.URL))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.calls != 0 {
		t.Fatal("a select for our own network was sent to a peer")
	}
	if ctx.ResponseBody != nil {
		t.Fatalf("ResponseBody = %q, want nil so the normal steps answer", ctx.ResponseBody)
	}
}

// Step 3 -- networkId ours but the binding is unknown. The step must not treat
// "we cannot serve it" as "it must be foreign": it hands the request on, and
// the existing path produces its 404.
func TestOurNetworkWithAnUnknownBindingIsNotSentAnywhere(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{}`)
	step := newStepFor(t)
	body := selectFor(ourNetwork, peer.URL)
	ctx := stepContextFor(t, body)

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.calls != 0 {
		t.Fatal("an unservable local binding caused an outbound call")
	}
}

// Step 4 -- networkId absent is treated as ours. Missing information must not
// widen what we do.
func TestAnAbsentNetworkIdIsTreatedAsOurs(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{}`)
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("", peer.URL))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.calls != 0 {
		t.Fatal("a request with no networkId was sent to another network")
	}
}

// Step 5 -- a foreign networkId with no bppUri is refused, and the message says
// which field is missing. Falling through would 404 about a binding that was
// never going to be here, which tells the caller nothing.
func TestAForeignNetworkWithNoEndpointIsRefused(t *testing.T) {
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("maha-vistar", ""))

	err := step.Run(ctx)
	if err == nil {
		t.Fatal("a foreign request with no bppUri was accepted")
	}
	if !strings.Contains(err.Error(), "bppUri") {
		t.Fatalf("err = %v, want it to name the missing context.bppUri", err)
	}
	if !strings.Contains(err.Error(), "maha-vistar") {
		t.Fatalf("err = %v, want it to name the network asked for", err)
	}
}

// The network id comparison is case-insensitive. A peer writing "Bharat-Vistar"
// is naming us, and reading that as a foreign network would send our own
// traffic out of the network.
func TestOurNetworkIdIsMatchedCaseInsensitively(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{}`)
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor(strings.ToUpper(ourNetwork), peer.URL))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.calls != 0 {
		t.Fatal("a differently-cased form of our own network id was treated as foreign")
	}
}

// The caller's Authorization header travels with the request. We are a hop:
// the peer has to see who actually asked, not us.
func TestTheCallersAuthorizationIsCarriedToThePeer(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{}`)
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("maha-vistar", peer.URL))
	ctx.Request.Header.Set(model.AuthHeaderSubscriber, `Signature keyId="farmer-app"`)

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.auth != `Signature keyId="farmer-app"` {
		t.Fatalf("peer saw Authorization %q, want the caller's own", peer.auth)
	}
}

// A peer that errors is reported as a failed federated call, not passed off as
// an answer. The caller asked US.
func TestAPeerErrorIsReportedNotRelayed(t *testing.T) {
	peer := newPeerServing(t, http.StatusInternalServerError, `{"error":"peer is broken"}`)
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("maha-vistar", peer.URL))

	err := step.Run(ctx)
	if err == nil {
		t.Fatal("a 500 from the peer was accepted as an answer")
	}
	if ctx.ResponseBody != nil {
		t.Fatalf("ResponseBody = %q, want nothing on a failed call", ctx.ResponseBody)
	}
}

// An unreachable peer fails the call rather than hanging or answering empty.
func TestAnUnreachablePeerFailsTheCall(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{}`)
	unreachable := peer.URL
	peer.Close()

	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("maha-vistar", unreachable))

	if err := step.Run(ctx); err == nil {
		t.Fatal("an unreachable peer produced no error")
	}
}

// A trailing slash on the peer's URI must not produce a double slash in the
// path it is called at. Registry and descriptor values are written by hand.
func TestATrailingSlashOnThePeerUriIsNormalised(t *testing.T) {
	peer := newPeerServing(t, http.StatusOK, `{}`)
	step := newStepFor(t)
	ctx := stepContextFor(t, selectFor("maha-vistar", peer.URL+"/"))

	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peer.path != "/select" {
		t.Fatalf("peer was called at %q, want /select", peer.path)
	}
}

// Without our own network id every request looks foreign, so the step refuses
// to be built at all rather than start forwarding everything.
func TestTheStepRefusesToBuildWithoutOurNetworkId(t *testing.T) {
	if _, err := newFederatedStep("", time.Second); err == nil {
		t.Fatal("the federated step was built with no networkId")
	}
}

// Zero timeout means "use the default", not "wait for ever".
func TestAZeroTimeoutTakesTheDefaultRatherThanNone(t *testing.T) {
	step, err := newFederatedStep(ourNetwork, 0)
	if err != nil {
		t.Fatalf("newFederatedStep: %v", err)
	}
	if step.client.Timeout != defaultFederatedTimeout {
		t.Fatalf("timeout = %v, want the default %v", step.client.Timeout, defaultFederatedTimeout)
	}
}
