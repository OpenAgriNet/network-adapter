package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// askedRequester records what the step passed on, so a test can assert the
// step read the body rather than that the registry refused.
type askedRequester struct {
	caller       string
	action       string
	capabilities []string
	refuse       error
}

func (r *askedRequester) CheckCrossNetworkRequester(_ context.Context, caller, action string, capabilities []string) error {
	r.caller, r.action, r.capabilities = caller, action, capabilities
	return r.refuse
}

func (r *askedRequester) Lookup(context.Context, *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

// keysOnlyRegistry resolves keys and knows nothing about admission.
type keysOnlyRegistry struct{}

func (keysOnlyRegistry) Lookup(context.Context, *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

func requestFor(action, sender string, contexts ...string) *model.StepContext {
	body := `{"context":{"action":"` + action + `","senderId":"` + sender + `"`
	if len(contexts) > 0 {
		body += `,"schemaContext":["` + strings.Join(contexts, `","`) + `"]`
	}
	body += `}}`
	return &model.StepContext{Context: context.Background(), Body: []byte(body)}
}

func TestAStepIsRefusedARegistryThatCannotJudgeAdmission(t *testing.T) {
	t.Parallel()

	if _, err := newCrossNetworkRequesterStep(keysOnlyRegistry{}); err == nil {
		t.Fatal("a registry that cannot list admitted networks must be refused at startup")
	}
}

func TestTheCallerAndWhatTheyAskedForComeFromTheBody(t *testing.T) {
	t.Parallel()

	registry := &askedRequester{}
	step, err := newCrossNetworkRequesterStep(registry)
	if err != nil {
		t.Fatalf("building the step: %v", err)
	}

	if err := step.Run(requestFor("discover", "other.example", "https://x/Mandi/context.jsonld")); err != nil {
		t.Fatalf("a caller we agreed to answer must pass: %v", err)
	}
	if registry.caller != "other.example" {
		t.Errorf("caller: got %q, want the body's senderId", registry.caller)
	}
	if registry.action != "discover" {
		t.Errorf("action: got %q, want discover", registry.action)
	}
	if len(registry.capabilities) != 1 || registry.capabilities[0] != "https://x/Mandi/context.jsonld" {
		t.Errorf("capabilities: got %v, want the body's schemaContext", registry.capabilities)
	}
}

func TestARefusalFromTheRegistryStopsTheRequest(t *testing.T) {
	t.Parallel()

	refused := errors.New("other.example is not a network we agreed to give to")
	step, err := newCrossNetworkRequesterStep(&askedRequester{refuse: refused})
	if err != nil {
		t.Fatalf("building the step: %v", err)
	}

	err = step.Run(requestFor("discover", "other.example"))
	if err == nil {
		t.Fatal("a refused caller must not reach the rest of the pipeline")
	}
	if !strings.Contains(err.Error(), "not a network we agreed to give to") {
		t.Errorf("the registry's reason must survive: %v", err)
	}
}

func TestARequestNamingNoSenderIsRefused(t *testing.T) {
	t.Parallel()

	registry := &askedRequester{}
	step, err := newCrossNetworkRequesterStep(registry)
	if err != nil {
		t.Fatalf("building the step: %v", err)
	}

	if err := step.Run(requestFor("discover", "")); err == nil {
		t.Fatal("a request naming no sender names nobody to judge, so it must be refused")
	}
}

func TestABodyThatIsNotJSONIsRefused(t *testing.T) {
	t.Parallel()

	step, err := newCrossNetworkRequesterStep(&askedRequester{})
	if err != nil {
		t.Fatalf("building the step: %v", err)
	}

	err = step.Run(&model.StepContext{Context: context.Background(), Body: []byte("not json")})
	if err == nil {
		t.Fatal("a body that cannot be read names no caller, so it must be refused")
	}
}
