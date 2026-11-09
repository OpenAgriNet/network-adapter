package MandiPrice

import (
	"context"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// prerequisites is what a mandi capability needs that its payload does not
// carry, keyed by binding key.
//
// Nothing to resolve: a market price select names the market it wants, and the
// mapping reads Agmarknet's codes straight off the payload. The one entry only
// logs, because a mapping cannot: it reads one commitment, one resource and one
// commodity per call.
var prerequisites = common.Prerequisites{
	"agmarknet|openagrinet:MandiPrice": logDroppedSelections,
}

// logDroppedSelections warns about what the mapping leaves out of the call;
// see droppedSelections.
func logDroppedSelections(ctx context.Context, beckn any) (map[string]any, error) {
	for _, warning := range droppedSelections(beckn) {
		log.Warn(ctx, warning)
	}
	return nil, nil
}

// droppedSelections names what the mapping drops. It reads
// commitments[0].resources[0] and its supportedCommodities[0] only; the rest
// are dropped, not refused.
func droppedSelections(beckn any) []string {
	var warnings []string
	commitments := listAt(beckn, "message", "contract", "commitments")
	if n := len(commitments); n > 1 {
		warnings = append(warnings, fmt.Sprintf("MandiPrice select has %d commitments; only commitments[0] is requested", n))
	}
	if len(commitments) == 0 {
		return warnings
	}
	resources := listAt(commitments[0], "resources")
	if n := len(resources); n > 1 {
		warnings = append(warnings, fmt.Sprintf("MandiPrice select has %d resources in commitments[0]; only resources[0] is requested", n))
	}
	if len(resources) == 0 {
		return warnings
	}
	if n := len(listAt(resources[0], "resourceAttributes", "supportedCommodities")); n > 1 {
		warnings = append(warnings, fmt.Sprintf("MandiPrice select names %d commodities; Agmarknet takes one per call, so only supportedCommodities[0] is requested", n))
	}
	return warnings
}

// listAt follows plain object keys and returns the array at the end, or nil.
func listAt(node any, keys ...string) []any {
	for _, key := range keys {
		fields, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = fields[key]
	}
	list, _ := node.([]any)
	return list
}
