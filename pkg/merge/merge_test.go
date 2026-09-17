package merge

import (
	"encoding/json"
	"strings"
	"testing"
)

// onDiscover builds an on_discover envelope carrying catalogs with the given
// ids. A local copy of core/module/handler/fanout_test.go's helper of the
// same name: that one exercises the executor against real servers, this one
// exercises the merge in isolation, and neither package should import the
// other's test-only code just to share a JSON fixture builder.
func onDiscover(messageID string, ids ...string) []byte {
	catalogs := make([]string, 0, len(ids))
	for _, id := range ids {
		catalogs = append(catalogs, `{"id":"`+id+`"}`)
	}
	return []byte(`{"context":{"messageId":"` + messageID + `","action":"on_discover"},` +
		`"message":{"catalogs":[` + strings.Join(catalogs, ",") + `]}}`)
}

func mergedIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var env struct {
		Message struct {
			Catalogs []struct {
				ID string `json:"id"`
			} `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("merged body is not a readable on_discover envelope: %v", err)
	}
	ids := make([]string, 0, len(env.Message.Catalogs))
	for _, c := range env.Message.Catalogs {
		ids = append(ids, c.ID)
	}
	return ids
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// mergeDiscover is Responses fixed to "message.catalogs", bodies in and
// merged envelope out.
func mergeDiscover(bodies [][]byte) ([]byte, error) {
	kept := make([]KeptResponse, 0, len(bodies))
	for _, b := range bodies {
		items, present, err := ItemsOf(b, "message.catalogs")
		if err != nil {
			return nil, err
		}
		kept = append(kept, KeptResponse{Body: b, Items: items, HasItems: present})
	}
	return Responses(kept, "message.catalogs")
}

func TestResponsesSeveralNetworksInterleavedRoundRobin(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-1", "bharat-1", "bharat-2", "bharat-3"),
		onDiscover("m-1", "maha-1"),
		onDiscover("m-1", "third-1", "third-2"),
	})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
	}

	want := []string{"bharat-1", "maha-1", "third-1", "bharat-2", "third-2", "bharat-3"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("Responses() = %v, want %v", got, want)
	}
}

func TestResponsesFirstResponseContextPreserved(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-2", "a"),
		onDiscover("m-2", "b"),
	})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
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
		t.Errorf("Responses() context = %+v, want the first response's context kept whole", env.Context)
	}
}

// TestResponsesRepeatedIDIsReturnedFromEveryNetwork locks in that merging no
// longer dedupes: the same catalog id published by two networks is the same
// resource served by two different providers -- each occurrence is returned,
// not collapsed to one.
func TestResponsesRepeatedIDIsReturnedFromEveryNetwork(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-3", "shared", "bharat-only"),
		onDiscover("m-3", "shared", "maha-only"),
	})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
	}
	want := []string{"shared", "shared", "bharat-only", "maha-only"}
	if got := mergedIDs(t, merged); !sameIDs(got, want) {
		t.Errorf("Responses() = %v, want %v (no dedupe)", got, want)
	}
}

func TestResponsesReturnsEverythingNoTruncation(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-5", "a1", "a2"),
		onDiscover("m-5", "b1", "b2"),
	})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
	}
	if got := mergedIDs(t, merged); len(got) != 4 {
		t.Errorf("Responses() returned %d catalogs, want all 4 kept", len(got))
	}
}

func TestResponsesNetworkWithNoCatalogsContributesNothing(t *testing.T) {
	merged, err := mergeDiscover([][]byte{
		onDiscover("m-6", "only"),
		[]byte(`{"context":{"messageId":"m-6"},"message":{}}`),
	})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
	}
	if got := mergedIDs(t, merged); !sameIDs(got, []string{"only"}) {
		t.Errorf("Responses() = %v, want [only]", got)
	}
}

func TestResponsesUnknownCatalogMembersSurvive(t *testing.T) {
	body := []byte(`{"context":{},"message":{"catalogs":[{"id":"a","futureMember":42}]}}`)
	merged, err := mergeDiscover([][]byte{body})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
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
		t.Error("Responses() dropped a catalog member it does not know about")
	}
}

func TestResponsesNonObjectResponseReturnsError(t *testing.T) {
	if _, err := mergeDiscover([][]byte{[]byte(`["not an envelope"]`)}); err == nil {
		t.Error("Responses() with a non-object response = nil error, want an error")
	}
}

func TestResponsesErrorEnvelopeDoesNotDonateTheEnvelope(t *testing.T) {
	// 200 with a Beckn error envelope and no catalogs: kept, but it must not
	// be the envelope the caller receives.
	merged, err := mergeDiscover([][]byte{
		[]byte(`{"context":{"messageId":"m-e"},"error":{"code":"NET_SOMETHING"}}`),
		onDiscover("m-e", "real-1"),
	})
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(merged, &env); err != nil {
		t.Fatalf("merged body unreadable: %v", err)
	}
	if _, ok := env["error"]; ok {
		t.Error("Responses() carried one network's error member into the merged answer")
	}
	if got := mergedIDs(t, merged); !sameIDs(got, []string{"real-1"}) {
		t.Errorf("Responses() = %v, want [real-1]", got)
	}
}

func TestFieldPathIsConfigurableNotHardcodedToCatalogs(t *testing.T) {
	order := func(id string) []byte {
		return []byte(`{"context":{},"message":{"orders":[{"id":"` + id + `"}]}}`)
	}
	kept := make([]KeptResponse, 0, 2)
	for _, id := range []string{"o1", "o2"} {
		b := order(id)
		items, present, err := ItemsOf(b, "message.orders")
		if err != nil {
			t.Fatalf("ItemsOf() error = %v", err)
		}
		kept = append(kept, KeptResponse{Body: b, Items: items, HasItems: present})
	}

	merged, err := Responses(kept, "message.orders")
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
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
		t.Errorf("Responses() with fieldPath=%q merged %d, want 2: the field path must not be hardcoded", "message.orders", len(env.Message.Orders))
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

	kept := make([]KeptResponse, 0, 2)
	for _, offerID := range []string{"offer:a", "offer:b"} {
		b := onSelect(offerID)
		items, present, err := ItemsOf(b, "message.contract.commitments")
		if err != nil {
			t.Fatalf("ItemsOf() error = %v", err)
		}
		kept = append(kept, KeptResponse{Body: b, Items: items, HasItems: present})
	}

	merged, err := Responses(kept, "message.contract.commitments")
	if err != nil {
		t.Fatalf("Responses() error = %v", err)
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
		t.Errorf("Responses() with fieldPath=%q merged %d commitments, want 2", "message.contract.commitments", len(env.Message.Contract.Commitments))
	}
	if env.Message.Contract.Status.Descriptor.Code != "ACTIVE" {
		t.Error("Responses() lost contract.status, a sibling of commitments at the same nesting level")
	}
}
