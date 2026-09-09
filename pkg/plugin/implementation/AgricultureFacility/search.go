// search.go is the whole of what a multi-type facility search means, and it
// lives here because every part of it is a fact about POCRA.
//
// POCRA's search takes exactly ONE category code: a comma-separated pair
// answers 200 with no providers at all, and a category array is refused
// outright, both verified against the live API. So a payload asking for three
// facility types cannot be served by one call, however the mapping is written.
//
// What this file does about that: read the types out of the payload, split one
// inbound payload into one single-type payload per type, run the ordinary
// one-payload-one-call step over each of them concurrently, and merge the
// answers back into one.
//
// Nothing below this file knows any of that. internal/upstream serves one
// payload with one call and has no notion of splitting; jsonmapper compiles
// the two halves every mapping has and no third thing; internal/concurrent
// runs N of anything, bounded and ordered, and has never heard of Beckn. The
// mapping this runs is written for a single-type payload, which is what it is
// always handed.
//
// Separated from the package clause by a blank line on purpose: the package's
// own doc comment is in AgricultureFacility.go, and this is a note about one
// file.

package AgricultureFacility

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/concurrent"
)

const (
	// MaxFacilityTypes bounds how many upstream calls one inbound payload may
	// become.
	//
	// Splitting is amplification: one request in, N out, each with its own
	// retry budget. The count comes from the PAYLOAD, so without a ceiling a
	// caller decides how much work this adapter and the provider do -- and the
	// provider is a government API that takes eight seconds to answer.
	//
	// Refused rather than clamped, unlike the registry's budgets. A clamped
	// timeout still answers the question asked; a clamped search silently
	// drops facility types from the answer, which is the exact defect this
	// facility exists to fix.
	MaxFacilityTypes = 8

	// DefaultSearchConcurrency is how many of a split search's calls are in
	// flight at once when a deployment does not say.
	//
	// One -- sequential. POCRA's aggregator waits a fixed window for its BPPs
	// to reply and returns whatever arrived, so a slow BPP that misses the
	// window is reported as no results rather than as an error. Concurrency
	// makes that more likely, and its failure mode is silent data loss -- the
	// exact defect this facility exists to fix.
	//
	// A provider that tolerates concurrency can say so per deployment, which
	// is a config change rather than a code one. The cost of the safe default
	// is latency: N calls take N times as long, and the operator can see that
	// in the log line each call writes.
	DefaultSearchConcurrency = 1
)

// Step serves the agriculture facility capability by splitting a multi-type
// search across the ordinary one-call step and merging what comes back.
//
// A step rather than a hook into internal/upstream: what a payload splits
// across, how many calls that is, what is too many and how the answers
// recombine are all facts about POCRA, so they belong to a thing that knows
// POCRA. internal/upstream stays a step that serves one payload with one
// call, which is what every other capability needs of it.
type Step struct {
	// inner serves one payload with one call: the ordinary upstream step,
	// built by this package's New and never handed a payload naming more than
	// one facility type.
	inner definition.Step

	// paths and bindingKeys answer "is this payload mine?", the same question
	// and the same way inner answers it. Asked here first because a payload
	// that is NOT this capability's must reach inner untouched -- it passes
	// through, as it does for every capability it does not serve -- while one
	// that IS must be split before inner sees it.
	paths       common.Paths
	bindingKeys []string

	// concurrency is how many of a split search's calls run at once.
	concurrency int
}

// Run serves one inbound payload.
//
// A payload naming one facility type is one call, which is the common case. A
// payload naming several becomes one call per type, run concurrently up to
// this step's limit, and their answers are merged into one. A payload that is
// not this capability's is handed to inner unchanged, which passes it through.
func (s *Step) Run(ctx *model.StepContext) error {
	if !s.mine(ctx.Body) {
		return s.inner.Run(ctx)
	}

	var beckn any
	if err := json.Unmarshal(ctx.Body, &beckn); err != nil {
		return model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: the payload is not JSON: %w", err))
	}

	types, err := facilityTypesFrom(beckn)
	if err != nil {
		return err
	}
	if len(types) > MaxFacilityTypes {
		return model.NewBadReqErr("", fmt.Errorf(
			"agriculture facility: this payload asks for %d facility types and the ceiling is %d; "+
				"split it across more than one request", len(types), MaxFacilityTypes))
	}

	// Every type gets its own payload, even when there is only one of them:
	// the fresh message id each part carries is what stops POCRA blending the
	// answers, and a search of one still wants its own id rather than the
	// caller's. See splitByType.
	parts, err := splitByType(ctx.Body, types)
	if err != nil {
		return err
	}

	log.Debugf(ctx, "agriculture facility: serving %d facility type(s) as %d call(s), %d at a time",
		len(types), len(parts), s.concurrency)

	// Bounded, ordered and fail-fast, which is internal/concurrent's whole
	// job. One failure fails the request: a partial answer is the defect this
	// was built to fix wearing a different hat -- the caller asked for four
	// facility types, would receive three, and nothing in the payload would
	// say that the fourth was asked for and lost.
	answers, err := concurrent.Map(ctx, parts, s.concurrency,
		func(callCtx context.Context, part []byte) ([]byte, error) {
			// Its own StepContext: inner reads Body and writes ResponseBody,
			// so the parts must not share either. callCtx rather than ctx so a
			// sibling's failure cancels this call too.
			partCtx := *ctx
			partCtx.Context = callCtx
			partCtx.Body = part
			partCtx.ResponseBody = nil

			if err := s.runPart(&partCtx); err != nil {
				return nil, err
			}
			return partCtx.ResponseBody, nil
		})
	if err != nil {
		return err
	}

	merged, err := mergeAnswers(answers, messageIDOf(beckn))
	if err != nil {
		return err
	}
	ctx.ResponseBody = merged
	return nil
}

// runPart runs the one-call step over one part and insists it answered.
//
// An empty ResponseBody means inner passed the payload through rather than
// serving it, which for a part built from a payload this step already
// recognised can only be a mismatch between this step's binding keys and
// inner's. Reported rather than merged, because merging nothing would answer
// a four-type search with three types and no error.
func (s *Step) runPart(partCtx *model.StepContext) error {
	if err := s.inner.Run(partCtx); err != nil {
		return err
	}
	if len(partCtx.ResponseBody) == 0 {
		return fmt.Errorf(
			"agriculture facility: the upstream step served no answer for one part of this search, " +
				"which means it does not serve this capability -- check that both are configured " +
				"with the same binding keys")
	}
	return nil
}

// mine reports whether this payload names one of the capabilities this step
// serves. A payload it cannot read is not refused here: inner refuses it, with
// the message it has always used.
func (s *Step) mine(body []byte) bool {
	binding, err := common.BindingFrom(s.paths, body)
	if err != nil {
		return false
	}
	return slices.Contains(s.bindingKeys, binding.Key())
}

// splitByType turns one payload into one payload per facility type.
//
// Each part names exactly one type, so the mapping's request half reads a
// single value rather than choosing among several -- choosing the first is
// what made a multi-type search answer with one type and silently drop the
// rest.
//
// Each part also carries a FRESH context.messageId. POCRA keeps a message_id's
// answers for ten minutes and returns the union of everything asked for under
// it, so two parts sharing an id would each come back carrying the other's
// facilities. The mapping copies this straight through to POCRA's message_id,
// whose schema refuses anything that is not a UUID.
//
// Re-decoded per part rather than deep-copied: the parts are mutated
// independently and a shared nested map would have them overwrite each other's
// type. A payload is a few kilobytes and this happens once per request.
func splitByType(body []byte, types []string) ([][]byte, error) {
	parts := make([][]byte, 0, len(types))
	for _, facilityType := range types {
		var part map[string]any
		if err := json.Unmarshal(body, &part); err != nil {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload is not a JSON object: %w", err))
		}

		attributes, ok := dig(part, "message", "contract", "commitments").([]any)
		if !ok || len(attributes) == 0 {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload carries no commitment to split"))
		}
		commitment, ok := attributes[0].(map[string]any)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload's first commitment is %T, not an object", attributes[0]))
		}
		resources, ok := commitment["resources"].([]any)
		if !ok || len(resources) == 0 {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload's commitment carries no resource to split"))
		}
		resource, ok := resources[0].(map[string]any)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload's first resource is %T, not an object", resources[0]))
		}
		resourceAttributes, ok := resource["resourceAttributes"].(map[string]any)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload's first resource carries no resourceAttributes"))
		}

		// A list of one, not a bare string: the required: checks and the
		// request half both read this through [] and a scalar would work, but
		// a part that does not look like the payload it came from is a trap
		// for whoever reads one in a log.
		resourceAttributes["supportedFacilityTypes"] = []any{facilityType}

		becknContext, ok := part["context"].(map[string]any)
		if !ok {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: the payload carries no context to stamp a message id on"))
		}
		becknContext["messageId"] = uuid.NewString()

		encoded, err := json.Marshal(part)
		if err != nil {
			return nil, fmt.Errorf("agriculture facility: could not rebuild the payload for %s: %w",
				facilityType, err)
		}
		parts = append(parts, encoded)
	}
	return parts, nil
}

// mergeAnswers combines one answer per facility type into the single answer the
// caller gets.
//
// The first answer is the shape of the result -- context, status and offer are
// the same in all of them, because all the parts came from one payload. What
// differs is the resources, which are concatenated in the order the payload
// named the types, deduplicated by id, and pointed at by the offer.
//
// The caller's own messageId is restored: each part carried a generated one so
// POCRA would not blend them, and the answer has to correlate with the request
// the caller actually sent.
//
// ORDERING, worth knowing: within one facility type the mapping ranks by
// POCRA's distance, and that ranking survives here. ACROSS types the result is
// type-blocked -- every KrishiVigyanKendra, then every Warehouse -- rather
// than globally nearest-first, because distance is not an intrinsic facility
// attribute and the mapping drops it before this sees the answers. A globally
// ranked multi-type search would need the mapping to publish a distance this
// could sort on, which the schema pack says it must not.
func mergeAnswers(answers [][]byte, callerMessageID string) ([]byte, error) {
	if len(answers) == 0 {
		return nil, fmt.Errorf("agriculture facility: nothing to merge; this search made no calls")
	}

	var merged map[string]any
	if err := json.Unmarshal(answers[0], &merged); err != nil {
		return nil, fmt.Errorf("agriculture facility: the answer for the first facility type is not JSON: %w", err)
	}
	commitment, err := commitmentOf(merged)
	if err != nil {
		return nil, err
	}

	resources := make([]any, 0)
	seen := map[string]bool{}
	for index, answer := range answers {
		var document map[string]any
		if err := json.Unmarshal(answer, &document); err != nil {
			return nil, fmt.Errorf(
				"agriculture facility: the answer for facility type %d is not JSON: %w", index, err)
		}
		part, err := commitmentOf(document)
		if err != nil {
			return nil, err
		}
		found, _ := part["resources"].([]any)
		for _, entry := range found {
			resource, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			id, _ := resource["id"].(string)
			// Deduplicated by id because a repeated id in the answer is a
			// broken reference the moment anything resolves it. The mapping
			// already collapses POCRA's own repeats within one call; this
			// catches a facility that answered for two types.
			if id != "" && seen[id] {
				continue
			}
			seen[id] = true
			resources = append(resources, resource)
		}
	}
	commitment["resources"] = resources

	// The offer points at what the answer actually carries. Leaving the first
	// part's resourceIds would name only its own type's facilities.
	if offer, ok := commitment["offer"].(map[string]any); ok {
		ids := make([]any, 0, len(resources))
		for _, entry := range resources {
			if resource, ok := entry.(map[string]any); ok {
				ids = append(ids, resource["id"])
			}
		}
		offer["resourceIds"] = ids
	}

	if becknContext, ok := merged["context"].(map[string]any); ok && callerMessageID != "" {
		becknContext["messageId"] = callerMessageID
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("agriculture facility: could not encode the merged answer: %w", err)
	}
	return encoded, nil
}

// commitmentOf reaches the one commitment an answer carries.
func commitmentOf(document map[string]any) (map[string]any, error) {
	commitments, _ := dig(document, "message", "contract", "commitments").([]any)
	if len(commitments) == 0 {
		return nil, fmt.Errorf("agriculture facility: an answer carries no commitment to merge")
	}
	commitment, ok := commitments[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("agriculture facility: an answer's commitment is %T, not an object", commitments[0])
	}
	return commitment, nil
}

// messageIDOf reads the caller's own message id, for the merged answer to
// correlate with. Absent is not an error: the mapping copies whatever the
// payload carried, so an absent one stays absent, as it did before splitting
// existed.
func messageIDOf(beckn any) string {
	document, ok := beckn.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := dig(document, "context", "messageId").(string)
	return id
}

// searchConcurrency resolves the deployment's setting, clamped to
// MaxFacilityTypes and defaulting to DefaultSearchConcurrency when left unset
// or non-positive.
//
// The arithmetic is internal/concurrent's, because every caller of it needs
// the same clamp and a zero limit there means UNBOUNDED. The three numbers are
// this package's, because each one is a fact about POCRA.
func searchConcurrency(configured int) int {
	return concurrent.Bound(configured, DefaultSearchConcurrency, MaxFacilityTypes)
}
