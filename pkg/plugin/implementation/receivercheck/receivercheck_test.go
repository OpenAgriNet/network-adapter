package receivercheck

import (
	"net/http"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

const us = "karnataka.oan.dev"

func step(t *testing.T) *Step {
	t.Helper()
	s, err := New(map[string]string{"subscriberId": us})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return s
}

func run(t *testing.T, body string) error {
	t.Helper()
	return step(t).Run(&model.StepContext{Body: []byte(body)})
}

func TestNewRequiresSubscriberID(t *testing.T) {
	for _, raw := range []map[string]string{{}, {"subscriberId": "   "}} {
		if _, err := New(raw); err == nil {
			t.Fatalf("New(%v): want an error, got nil", raw)
		}
	}
}

// A request for us is passed through untouched, so the capability steps behave
// exactly as they did before this step was added.
func TestOursPassesThrough(t *testing.T) {
	for name, body := range map[string]string{
		"receiverId": `{"context":{"receiverId":"` + us + `"}}`,
		"bppId":      `{"context":{"bppId":"` + us + `"}}`,
		"bpp_id":     `{"context":{"bpp_id":"` + us + `"}}`,
		"case":       `{"context":{"receiverId":"KARNATAKA.OAN.DEV"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(t, body); err != nil {
				t.Fatalf("Run(): want nil, got %v", err)
			}
		})
	}
}

// The case this step exists for. Both networks front the same upstream, so the
// binding key matches here too and the handler's 404 never fires -- without
// this check the peer's request is answered with our data, under their name.
func TestPeersRequestIsRefused(t *testing.T) {
	err := run(t, `{"context":{"receiverId":"maharashtra.oan.dev","action":"select"}}`)
	if err == nil {
		t.Fatal("Run(): want a refusal for another network's receiver, got nil")
	}

	coded, ok := err.(*model.CodedErr)
	if !ok {
		t.Fatalf("Run(): want *model.CodedErr, got %T", err)
	}
	if got := coded.HTTPStatus(); got != http.StatusNotFound {
		t.Errorf("status: want %d, got %d", http.StatusNotFound, got)
	}
	// Both ends named, because the fix is the caller's: its bppUri and its
	// receiverId disagree.
	for _, want := range []string{"maharashtra.oan.dev", us} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not name %q: %s", want, err)
		}
	}
}

// Refused, not assumed. Guessing "ours" here is exactly the failure the step
// exists to prevent.
func TestMissingReceiverIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"context":{"action":"select"}}`,
		"blank":  `{"context":{"receiverId":"   "}}`,
		"noCtx":  `{"message":{}}`,
		"broken": `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t, body)
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
		})
	}
}

// The trap, pinned. reqpreprocessor resolves the module's own id FROM THE BODY
// (it reads bppId/receiverId), so a rewrite that compares the receiver against
// ctx.SubID would compare the field with itself: always equal, nothing ever
// refused. SubID is set to the PEER here, so that rewrite fails this test
// instead of silently passing everything.
func TestDoesNotCompareAgainstContextSubID(t *testing.T) {
	err := step(t).Run(&model.StepContext{
		Body:  []byte(`{"context":{"receiverId":"maharashtra.oan.dev"}}`),
		SubID: "maharashtra.oan.dev",
	})
	if err == nil {
		t.Fatal("Run(): a peer's request was accepted; the check is reading its own id from the body")
	}
}
