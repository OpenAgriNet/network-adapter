package common

import (
	"net/url"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// A step list runs every step, so a capability step sees a request that an
// earlier step has already routed to another network. Serving it would call the
// upstream for an answer the proxy is about to discard, and answer in the
// peer's name while doing it.
func TestRoutedRequestIsNotServed(t *testing.T) {
	target, err := url.Parse("https://maharashtra.oan.dev/select")
	if err != nil {
		t.Fatal(err)
	}
	// No paths, no registry, no auth: if the guard does not fire, the body is
	// read and the nil dependencies below panic -- which is the failure.
	step := &Step{}
	ctx := &model.StepContext{
		Body:  []byte(`{"context":{"action":"select"},"message":{}}`),
		Route: &model.Route{TargetType: "url", URL: target},
	}
	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run(): want nil for an already-routed request, got %v", err)
	}
}
