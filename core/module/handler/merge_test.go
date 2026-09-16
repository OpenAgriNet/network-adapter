package handler

import (
	"encoding/json"
	"testing"
)

// mergeDiscover is mergeResponses fixed to "message.catalogs", bodies in and
// merged envelope out. onDiscover, mergedIDs and sameIDs are shared with
// fanout_test.go -- both files exercise the same envelope shape, one at the
// merge level and one through the executor.
func mergeDiscover(bodies [][]byte, limit int, hasLimit bool) ([]byte, error) {
	kept := make([]keptResponse, 0, len(bodies))
	for _, b := range bodies {
		items, present, err := itemsOf(b, "message.catalogs")
		if err != nil {
			return nil, err
		}
		kept = append(kept, keptResponse{body: b, items: items, hasItems: present})
	}
	return mergeResponses(kept, "message.catalogs", limit, hasLimit)
}

func TestResponsesSeveralNetworksInterleavedRoundRobin(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-1", "bharat-1", "bharat-2", "bharat-3"),
		onDiscover("m-1", "maha-1"),
		onDiscover("m-1", "third-1", "third-2"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}

	want := []string{"bharat-1", "maha-1", "third-1", "bharat-2", "third-2", "bharat-3"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("mergeResponses() = %v, want %v", got, want)
	}
}

func TestResponsesFirstResponseContextPreserved(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-2", "a"),
		onDiscover("m-2", "b"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	var env struct {
		Context struct {
			MessageID string `json:"messageId"`
			Action    string `json:"action"`
		} `json:"context"`
	}
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body has no readable context: %v", err)
	}
	if env.Context.MessageID != "m-2" || env.Context.Action != "on_discover" {
		t.Errorf("mergeResponses() context = %+v, want the first response's context kept whole", env.Context)
	}
}

func TestResponsesRepeatedIDKeepsFirstOccurrence(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-3", "shared", "bharat-only"),
		onDiscover("m-3", "shared", "maha-only"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	want := []string{"shared", "bharat-only", "maha-only"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("mergeResponses() = %v, want %v", got, want)
	}
}

func TestResponsesLimitPresentTruncatesMergedList(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-4", "a1", "a2", "a3"),
		onDiscover("m-4", "b1", "b2", "b3"),
	}, 3, true)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	// Interleaved first, so the cut keeps both networks represented.
	want := []string{"a1", "b1", "a2"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("mergeResponses() = %v, want %v", got, want)
	}
}

func TestResponsesLimitAbsentReturnsEverything(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-5", "a1", "a2"),
		onDiscover("m-5", "b1", "b2"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	if got := mergedIDs(t, merged); len(got) != 4 {
		t.Errorf("mergeResponses() returned %d catalogs, want all 4 kept when no limit was sent", len(got))
	}
}

func TestResponsesNetworkWithNoCatalogsContributesNothing(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-6", "only"),
		[]byte(`{"context":{"messageId":"m-6"},"message":{}}`),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	if got := mergedIDs(t, merged); !sameIDs(got, []string{"only"}) {
		t.Errorf("mergeResponses() = %v, want [only]", got)
	}
}

func TestResponsesUnknownCatalogMembersSurvive(t *testing.T) {
	body := []byte(`{"context":{},"message":{"catalogs":[{"id":"a","futureMember":42}]}}`)
	merged, err := mergeDiscover([][]byte{body}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	var env struct {
		Message struct {
			Catalogs []map[string]json.RawMessage `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if _, ok := env.Message.Catalogs[0]["futureMember"]; !ok {
		t.Error("mergeResponses() dropped a catalog member it does not know about")
	}
}

func TestResponsesNonObjectResponseReturnsError(t *testing.T) {
	if _, err := mergeDiscover([][]byte{[]byte(`["not an envelope"]`)}, 0, false); err == nil {
		t.Error("mergeResponses() with a non-object response = nil error, want an error")
	}
}

func TestResponsesErrorEnvelopeDoesNotDonateTheEnvelope(t *testing.T) {
	// 200 with a Beckn error envelope and no catalogs: kept, but it must not
	// be the envelope the caller receives.
	merged, err := mergeDiscover([][]byte{
		[]byte(`{"context":{"messageId":"m-e"},"error":{"code":"NET_SOMETHING"}}`),
		onDiscover("m-e", "real-1"),
	}, 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if _, ok := env["error"]; ok {
		t.Error("mergeResponses() carried one network's error member into the merged answer")
	}
	if got := mergedIDs(t, merged); !sameIDs(got, []string{"real-1"}) {
		t.Errorf("mergeResponses() = %v, want [real-1]", got)
	}
}

func TestFieldPathIsConfigurableNotHardcodedToCatalogs(t *testing.T) {
	order := func(id string) []byte {
		return []byte(`{"context":{},"message":{"orders":[{"id":"` + id + `"}]}}`)
	}
	kept := make([]keptResponse, 0, 2)
	for _, id := range []string{"o1", "o2"} {
		b := order(id)
		items, present, err := itemsOf(b, "message.orders")
		if err != nil {
			t.Fatalf("itemsOf() error = %v", err)
		}
		kept = append(kept, keptResponse{body: b, items: items, hasItems: present})
	}

	merged, err := mergeResponses(kept, "message.orders", 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}
	var env struct {
		Message struct {
			Orders []struct {
				ID string `json:"id"`
			} `json:"orders"`
		} `json:"message"`
	}
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if len(env.Message.Orders) != 2 {
		t.Errorf("mergeResponses() with fieldPath=%q merged %d, want 2: the field path must not be hardcoded", "message.orders", len(env.Message.Orders))
	}
}

// TestFieldPathReachesNestedArraysLikeSelectsCommitments is on_select's own
// shape: message.contract.commitments, two levels under message rather than
// discover's one. It also checks that contract's OTHER sibling keys survive
// the merge untouched, since setAtPath only decodes the segments on the path.
func TestFieldPathReachesNestedArraysLikeSelectsCommitments(t *testing.T) {
	onSelect := func(offerID string) []byte {
		return []byte(`{"context":{"action":"on_select"},"message":{"contract":{` +
			`"status":{"descriptor":{"code":"ACTIVE"}},` +
			`"commitments":[{"offer":{"id":"` + offerID + `"}}]}}}`)
	}

	kept := make([]keptResponse, 0, 2)
	for _, offerID := range []string{"offer:a", "offer:b"} {
		b := onSelect(offerID)
		items, present, err := itemsOf(b, "message.contract.commitments")
		if err != nil {
			t.Fatalf("itemsOf() error = %v", err)
		}
		kept = append(kept, keptResponse{body: b, items: items, hasItems: present})
	}

	merged, err := mergeResponses(kept, "message.contract.commitments", 0, false)
	if err != nil {
		t.Fatalf("mergeResponses() error = %v", err)
	}

	var env struct {
		Message struct {
			Contract struct {
				Status struct {
					Descriptor struct {
						Code string `json:"code"`
					} `json:"descriptor"`
				} `json:"status"`
				Commitments []struct {
					Offer struct {
						ID string `json:"id"`
					} `json:"offer"`
				} `json:"commitments"`
			} `json:"contract"`
		} `json:"message"`
	}
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if len(env.Message.Contract.Commitments) != 2 {
		t.Errorf("mergeResponses() with fieldPath=%q merged %d commitments, want 2", "message.contract.commitments", len(env.Message.Contract.Commitments))
	}
	if env.Message.Contract.Status.Descriptor.Code != "ACTIVE" {
		t.Error("mergeResponses() lost contract.status, a sibling of commitments at the same nesting level")
	}
}
