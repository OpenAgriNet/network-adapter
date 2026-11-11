package MandiPrice

import (
	"encoding/json"
	"strings"
	"testing"
)

// The warnings count what the mapping reads, commitments[0].resources[0], not
// every commodity in the payload.
func TestDroppedSelectionsCountsWhatTheMappingReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "one of each",
			payload: `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{"supportedCommodities":[{"code":"1"}]}}]}]}}}`,
		},
		{
			// One commodity each in two commitments: the second commitment is
			// dropped whole, and no commodity is lost from the first.
			name:    "two commitments, one commodity each",
			payload: `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{"supportedCommodities":[{"code":"1"}]}}]},{"resources":[{"resourceAttributes":{"supportedCommodities":[{"code":"2"}]}}]}]}}}`,
			want:    []string{"2 commitments"},
		},
		{
			name:    "two resources",
			payload: `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{"supportedCommodities":[{"code":"1"}]}},{"resourceAttributes":{"supportedCommodities":[{"code":"2"}]}}]}]}}}`,
			want:    []string{"2 resources"},
		},
		{
			name:    "two commodities in the first resource",
			payload: `{"message":{"contract":{"commitments":[{"resources":[{"resourceAttributes":{"supportedCommodities":[{"code":"1"},{"code":"2"}]}}]}]}}}`,
			want:    []string{"2 commodities"},
		},
		{
			name:    "no commitments",
			payload: `{"message":{"contract":{}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var beckn any
			if err := json.Unmarshal([]byte(tc.payload), &beckn); err != nil {
				t.Fatalf("the fixture is not JSON: %v", err)
			}
			got := droppedSelections(beckn)
			if len(got) != len(tc.want) {
				t.Fatalf("warnings = %q, want %d matching %q", got, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("warning %d = %q, want it to mention %q", i, got[i], want)
				}
			}
		})
	}
}
