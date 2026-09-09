// fanout.go implements this capability's own answer to a question
// internal/upstream deliberately does not have an opinion on: given a
// payload that names several facility types, how many upstream calls should
// that become, in what order or concurrency, and what is too many.
//
// Everything in this file is a fact about POCRA specifically, not about
// calling an upstream provider in general -- that is why it lives here and
// not in internal/upstream, which now serves several capabilities that never
// fan out at all. The actual bounded, ordered, fail-fast execution is
// internal/concurrent's Run -- a generic engine this file is a thin,
// POCRA-specific caller of, not a reimplementation.
//
// Separated from the package clause by a blank line on purpose: the package's
// own doc comment is in AgricultureFacility.go, and this is a note about one
// file.

package AgricultureFacility

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/concurrent"
)

// OneCall makes exactly one upstream call, with fanValue bound as _fan for
// the request half. It is what internal/upstream hands a fan-out hook, and
// all internal/upstream knows how to do: authenticate, retry within budget,
// decode the answer.
//
// Named here rather than there deliberately: internal/upstream declares this
// shape as an unnamed function type, because naming it would put a
// plugin-specific idea in the generic machinery. This package is the one
// that has the idea, so it is the one that gets to name it.
//
// An ALIAS (=), not a defined type, and it has to stay one. Gather below
// uses it in its own signature, and Gather must remain assignable to the
// unnamed parameter internal/upstream.NewWithFanOut declares. Go requires
// identical
// underlying types for that, and a defined OneCall would not be identical
// to the unnamed `func(context.Context, any) (any, error)` that appears in
// upstream's parameter -- so Gather would stop compiling at the call site,
// for a reason that reads as nonsense unless you know this rule.
type OneCall = func(ctx context.Context, fanValue any) (any, error)

// Gather decides what a fan-out MEANS for POCRA: how many upstream calls the
// selected values become, in what order, how many at once, and what is too
// many. internal/upstream calls it once per request, only when the mapping's
// fan-out half selected values, and hands the result to the response half.
//
// This is the plugin-specific half of fan-out, which is why the name lives
// here and not in internal/upstream -- see that package's NewWithFanOut
// for the
// unnamed shape this satisfies, and its README for the whole contract. The
// mechanical half (bounded, ordered, fail-fast) is
// internal/concurrent.Run, which gatherFacilities below is a thin caller of.
//
// A defined type rather than an alias, unlike OneCall: nothing needs Gather
// to be identical to anything, only assignable to upstream's unnamed
// parameter -- which a defined type with an identical underlying type is.
type Gather func(ctx context.Context, values []any, one OneCall) (any, error)

const (
	// MaxFanOut bounds how many upstream calls one inbound payload may become.
	//
	// Fan-out is amplification: one request in, N out, each with its own retry
	// budget. The values come from the PAYLOAD, so without a ceiling a caller
	// decides how much work this adapter and the provider do -- and the
	// provider is a government API that takes eight seconds to answer.
	//
	// Refused rather than clamped, unlike the registry's budgets. A clamped
	// timeout still answers the question asked; a clamped fan-out silently
	// drops facility types from the answer, which is the exact defect fan-out
	// was added to fix.
	MaxFanOut = 8

	// DefaultFanOutConcurrency is how many fan-out calls are in flight at once
	// when a deployment does not say.
	//
	// One -- sequential. POCRA's aggregator waits a fixed window for its BPPs
	// to reply and returns whatever arrived, so a slow BPP that misses the
	// window is reported as no results rather than as an error. Concurrency
	// makes that more likely, and its failure mode is silent data loss -- the
	// exact defect fan-out was added to fix.
	//
	// A provider that tolerates concurrency can say so per deployment, which is
	// a config change rather than a code one. The cost of the safe default is
	// latency: N calls take N times as long, and the operator can see that in
	// the log line each call writes.
	DefaultFanOutConcurrency = 1
)

// applyFanOutDefaults resolves the deployment's fan-out concurrency, clamped
// to MaxFanOut, defaulting to DefaultFanOutConcurrency when left unset or
// non-positive.
func applyFanOutDefaults(configured int) int {
	concurrency := configured
	if concurrency <= 0 {
		concurrency = DefaultFanOutConcurrency
	}
	if concurrency > MaxFanOut {
		concurrency = MaxFanOut
	}
	return concurrency
}

// gatherFacilities builds this capability's Gather: refuse a payload over
// the ceiling, then hand internal/concurrent.Run one call per fan-out value,
// bounded by concurrency.
//
// One failure fails the request. A partial answer is the defect this was
// built to fix wearing a different hat: the caller asked for four facility
// types, would receive three, and nothing in the payload would say that the
// fourth was asked for and lost. concurrent.Run's own cancel-on-first-error
// is what makes that true without this file managing any of the concurrency
// itself.
func gatherFacilities(concurrency int) Gather {
	return func(ctx context.Context, fan []any, one OneCall) (any, error) {
		if len(fan) > MaxFanOut {
			return nil, model.NewBadReqErr("", fmt.Errorf(
				"agriculture facility: this payload asks for %d upstream calls and the ceiling is %d; "+
					"split it across more than one request", len(fan), MaxFanOut))
		}

		return concurrent.Run(ctx, len(fan), concurrency, func(ctx context.Context, index int) (any, error) {
			value := fan[index]
			// Each call gets its own identity. POCRA keeps a message_id's
			// answers for ten minutes and returns the union of everything
			// asked for under it, so reusing one id across the fan-out
			// would make every call return every other call's facilities
			// too. Generated here rather than in the mapping because it
			// has to be a fresh UUID, which JSONata cannot produce, and
			// POCRA's schema refuses a message_id that is not one.
			answer, err := one(ctx, map[string]any{
				"value":  value,
				"callId": uuid.NewString(),
			})
			if err != nil {
				return nil, fmt.Errorf("agriculture facility: the call for fan-out value %v failed: %w", value, err)
			}
			return answer, nil
		})
	}
}
