package main

import (
	"context"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

func TestServeMappingsIsFetchableByTheMapper(t *testing.T) {
	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatalf("base = %q, want a loopback http URL", base)
	}

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	// The response half must return an ARRAY even for one row: JSONata
	// collapses a one-element sequence to a bare value, so the mapping wraps
	// its $map in [...]. A regression here is silent -- the caller's JSON
	// decode into a slice simply fails on an object.
	out, err := mapper.Transform(ctx, base+"/master-states.yaml", definition.DirectionResponse,
		map[string]any{"response": []any{
			map[string]any{"state_id": 20, "state_name": "Maharashtra", "agm_state_code": "MH"},
		}})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if got, want := string(out), `[{"code":"MH","name":"Maharashtra"}]`; got != want {
		t.Errorf("response half = %s, want %s", got, want)
	}
}

func TestMasterStatesRequestHalfBuildsTheQuery(t *testing.T) {
	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	out, err := mapper.Transform(ctx, base+"/master-states.yaml", definition.DirectionRequest,
		map[string]any{"_local": map[string]any{"token": "t-123"}})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if got, want := string(out), `{"option":"4","token":"t-123"}`; got != want {
		t.Errorf("request half = %s, want %s", got, want)
	}
}
