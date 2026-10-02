package opapolicychecker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// A peer may not pick a friendlier policy by naming another network in its own
// request. When the verified caller is an admitted PEER, that name is the only
// one used.
func TestPolicyForAVerifiedPeerIgnoresTheBody(t *testing.T) {
	ctx := &model.StepContext{
		Body:                      []byte(`{"context":{"networkId":"bharatvistar.oan.local"}}`),
		VerifiedCaller:            "mahavistara.oan.local",
		VerifiedCallerPeerNetwork: true,
	}

	if got := networkForPolicy(ctx, parseRequestContext(ctx.Body)); got != "mahavistara.oan.local" {
		t.Fatalf("policy chosen for %q, want the verified peer", got)
	}
}

// The case this gets wrong if it keys on the signature rather than on admission.
// A farmer's app signs too, and this deployment's own network-layer adapter even
// carries role "network" -- neither is a peer. Choosing a policy by their
// subscriber id would look for a peer policy that does not exist.
func TestPolicyForOurOwnCallersComesFromTheBody(t *testing.T) {
	for _, caller := range []string{
		"farmer-app.bharatvistar.oan.local",
		"network.bharatvistar.oan.local",
	} {
		ctx := &model.StepContext{
			Body:                      []byte(`{"context":{"networkId":"bharatvistar.oan.local"}}`),
			VerifiedCaller:            caller,
			VerifiedCallerPeerNetwork: false,
		}

		if got := networkForPolicy(ctx, parseRequestContext(ctx.Body)); got != "bharatvistar.oan.local" {
			t.Errorf("%s: policy chosen for %q, want this network's own", caller, got)
		}
	}
}

func TestPolicyForAnUnsignedCallComesFromTheBody(t *testing.T) {
	ctx := &model.StepContext{Body: []byte(`{"context":{"networkId":"bharatvistar.oan.local"}}`)}

	if got := networkForPolicy(ctx, parseRequestContext(ctx.Body)); got != "bharatvistar.oan.local" {
		t.Fatalf("policy chosen for %q, want the body's value for an unsigned call", got)
	}
}

// A policy file nothing loads is not a control. This is the test that would have
// caught a rule reading a field the evaluator never sets.
func TestPeerPolicyAllowsReadsAndRefusesCommits(t *testing.T) {
	evaluator, err := NewEvaluator(
		[]string{"testdata/federation/peer.rego"},
		"data.federation.peer.result",
		nil, false, time.Second, nil,
	)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}

	body := func(action string) []byte {
		return []byte(fmt.Sprintf(`{"context":{"action":%q}}`, action))
	}

	for _, action := range []string{"discover", "select"} {
		violations, err := evaluator.Evaluate(context.Background(), body(action))
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", action, err)
		}
		if len(violations) != 0 {
			t.Errorf("%s was refused across networks: %v", action, violations)
		}
	}

	for _, action := range []string{"init", "confirm", "cancel"} {
		violations, err := evaluator.Evaluate(context.Background(), body(action))
		if err != nil {
			t.Fatalf("Evaluate(%s): %v", action, err)
		}
		if len(violations) == 0 {
			t.Errorf("%s was permitted across networks", action)
			continue
		}
		// The refusal names the action, so an operator reading a log learns
		// which call was refused and not merely that one was.
		if !strings.Contains(violations[0].Message, action) {
			t.Errorf("refusal for %s did not name it: %v", action, violations[0].Message)
		}
	}
}
