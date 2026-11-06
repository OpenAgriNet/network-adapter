package MandiPrice

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// prerequisites is what a mandi capability needs that its payload does not
// carry, keyed by binding key.
//
// Nothing to resolve: a market price select names the market it wants, and the
// mapping reads Agmarknet's codes straight off the payload. The one entry only
// logs, because a mapping cannot: Agmarknet takes one commodity per call.
var prerequisites = common.Prerequisites{
	"agmarknet|openagrinet:MandiPrice": logExtraCommodities,
}

// logExtraCommodities warns when a select names several commodities. Only the
// first is sent to Agmarknet, which takes one code per call; the rest are
// dropped, not refused.
func logExtraCommodities(ctx context.Context, beckn any) (map[string]any, error) {
	if n := len(commodities(beckn)); n > 1 {
		log.Warnf(ctx, "MandiPrice select names %d commodities; Agmarknet takes one per call, so only supportedCommodities[0] is requested", n)
	}
	return nil, nil
}

// commodities reads message.contract.commitments[0].resources[0]
// .resourceAttributes.supportedCommodities, or nil when any step is missing.
func commodities(beckn any) []any {
	node := beckn
	for _, step := range []any{"message", "contract", "commitments", 0, "resources", 0, "resourceAttributes", "supportedCommodities"} {
		switch key := step.(type) {
		case string:
			m, ok := node.(map[string]any)
			if !ok {
				return nil
			}
			node = m[key]
		case int:
			list, ok := node.([]any)
			if !ok || len(list) <= key {
				return nil
			}
			node = list[key]
		}
	}
	list, _ := node.([]any)
	return list
}
