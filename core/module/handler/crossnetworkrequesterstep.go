package handler

import (
	"context"
	"fmt"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// crossNetworkRequester is the half of the registry this step needs: judging a
// caller against what we agreed when we admitted them. Narrowed from the
// registry so a plugin that only distributes keys is refused at startup rather
// than passing every request.
type crossNetworkRequester interface {
	CheckCrossNetworkRequester(ctx context.Context, caller, action string, capabilities []string) error
}

// crossNetworkRequesterStep refuses a caller we did not agree to answer.
//
// It reads the caller from the body rather than from a verified signature, so a
// mount that also runs validateSign should list it AFTER that step -- there the
// caller has been proved. Listed alone it still enforces the agreement, against
// a senderId the caller wrote.
type crossNetworkRequesterStep struct {
	registry crossNetworkRequester
}

func newCrossNetworkRequesterStep(registry definition.RegistryLookup) (definition.Step, error) {
	admission, ok := registry.(crossNetworkRequester)
	if !ok {
		return nil, fmt.Errorf("invalid config: the Registry plugin cannot judge admission, " +
			"so checkCrossNetworkRequester would pass every caller")
	}
	return &crossNetworkRequesterStep{registry: admission}, nil
}

func (s *crossNetworkRequesterStep) Run(ctx *model.StepContext) error {
	_, becknContext, becknErr := model.ExtractContext(ctx.Body)
	if becknErr != nil {
		return model.WrapExtractContextErr("could not read the request context", becknErr)
	}

	// senderId names the caller on every mount another network reaches. Refused
	// rather than skipped: a request naming nobody cannot be judged, and
	// treating that as "allow" is the one outcome worth never reaching by
	// accident.
	caller := model.ResolveCallerID(becknContext, model.RoleBPP)
	if caller == "" {
		return model.NewBadReqErr("", fmt.Errorf(
			"the request names no sender, so there is nobody to judge: set context.senderId"))
	}

	action, _ := becknContext["action"].(string)
	return s.registry.CheckCrossNetworkRequester(ctx, caller, action, schemaContextOf(becknContext))
}

// schemaContextOf reads the capabilities a discover names. Absent is an empty
// list, which the agreement check refuses for a discover and ignores otherwise.
func schemaContextOf(becknContext map[string]any) []string {
	listed, _ := becknContext["schemaContext"].([]any)
	capabilities := make([]string, 0, len(listed))
	for _, entry := range listed {
		if name, ok := entry.(string); ok && name != "" {
			capabilities = append(capabilities, name)
		}
	}
	return capabilities
}
