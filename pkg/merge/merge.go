// Package merge folds several targets' Beckn v2 envelope responses into one,
// over a caller-configured field path. It has no knowledge of HTTP, routing,
// or any specific action -- catalogs, commitments or anything else nested
// under message is the caller's business, not this package's.
//
// The field path is the only configurable part of the merge policy.
// Ordering (round-robin interleave, in interleave) and donor selection
// (first target in caller order that carries the path, in Responses) are
// fixed here, deliberately: no deployment has needed either to vary, and
// adding knobs ahead of an actual need is speculative complexity this
// package would then have to carry and test regardless.
package merge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// contextKey and messageKey name the two members every Beckn v2 envelope
// carries. The merged answer keeps exactly these two and nothing else -- a
// v2 action is additionalProperties:false, so anything a donor carried beyond
// them (an "error" member, say) would fail the caller's own schema.
const (
	contextKey = "context"
	messageKey = "message"
)

// KeptResponse is one target's accepted answer: the envelope body it sent,
// and the array read out of it at the configured field path (nil if it had
// none -- a network with no matches is a valid answer, not an error).
type KeptResponse struct {
	Body     []byte
	Items    []json.RawMessage
	HasItems bool
}

// ItemsOf pulls the array at fieldPath (a dot path from the envelope root,
// e.g. "message.catalogs" or "message.contract.commitments") out of one
// response body, without decoding the items themselves so a member this
// build does not know about is re-emitted byte for byte rather than dropped.
//
// A response carrying nothing at fieldPath contributes nothing rather than
// failing the merge: a network with no matches is a valid answer.
func ItemsOf(body []byte, fieldPath string) ([]json.RawMessage, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("not a JSON object: %w", err)
	}
	return arrayAtPath(envelope, strings.Split(fieldPath, "."))
}

// Responses folds several kept responses into one envelope, merging the
// array at fieldPath across all of them.
//
// The donor -- the first kept response in TARGET ORDER (not arrival order)
// that carries fieldPath -- supplies message verbatim; this code never
// synthesizes one of its own. A 200 carrying a Beckn error envelope is kept
// (not a transport failure) but never donates: it has no fieldPath, so
// HasItems is false regardless of target order.
//
// context comes from requestBody instead, action replaced with action --
// never from a donor. A target is the caller's proxy for one catalog, not
// this adapter's identity; echoing a donor's context would have the merged
// reply claim to be whichever target happened to answer first, and that
// claim would change depending on who's up.
func Responses(kept []KeptResponse, fieldPath string, requestBody []byte, action string) ([]byte, error) {
	donor := -1
	for i, k := range kept {
		if k.HasItems {
			donor = i
			break
		}
	}
	if donor < 0 {
		return nil, fmt.Errorf("no response carried %s: a rule with several targets is only meaningful for an action whose replies carry it", fieldPath)
	}

	var donorEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(kept[donor].Body, &donorEnvelope); err != nil {
		return nil, fmt.Errorf("donor response is not a JSON object: %w", err)
	}

	replyContext, err := contextWithAction(requestBody, action)
	if err != nil {
		return nil, err
	}

	// Rebuilt rather than edited in place: only context and message belong in
	// the answer. message is seeded from the donor here so setAtPath below
	// preserves every sibling along fieldPath instead of starting empty.
	envelope := map[string]json.RawMessage{contextKey: replyContext}
	if raw, ok := donorEnvelope[messageKey]; ok {
		envelope[messageKey] = raw
	}

	perNetwork := make([][]json.RawMessage, 0, len(kept))
	for _, k := range kept {
		perNetwork = append(perNetwork, k.Items)
	}
	merged := interleave(perNetwork)
	if merged == nil {
		merged = []json.RawMessage{}
	}

	if err := setAtPath(envelope, strings.Split(fieldPath, "."), merged); err != nil {
		return nil, fmt.Errorf("writing merged %s: %w", fieldPath, err)
	}
	return json.Marshal(envelope)
}

// contextWithAction reads context out of requestBody -- the caller's own
// inbound envelope -- and returns it with its action member replaced by
// action (e.g. "on_discover"), everything else (bapId/bapUri, transactionId,
// messageId, version...) carried through unchanged: they are the caller's
// own identity, genuinely correct to echo back, not this package's to
// invent or verify.
func contextWithAction(requestBody []byte, action string) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &envelope); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	rawContext, ok := envelope[contextKey]
	if !ok {
		return nil, fmt.Errorf("request carries no %s", contextKey)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rawContext, &fields); err != nil {
		return nil, fmt.Errorf("request context is not a JSON object: %w", err)
	}
	encodedAction, err := json.Marshal(action)
	if err != nil {
		return nil, fmt.Errorf("encoding action %q: %w", action, err)
	}
	fields["action"] = encodedAction
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("re-encoding request context: %w", err)
	}
	return encoded, nil
}

// arrayAtPath walks a dot path of object keys from the given node ("message",
// "catalogs", or "message", "contract", "commitments" for select) and reads
// the array at the end of it, without decoding the array's own elements. Any
// segment absent along the way means the response simply does not carry this
// path -- a valid answer, not an error.
func arrayAtPath(node map[string]json.RawMessage, segments []string) ([]json.RawMessage, bool, error) {
	raw, ok := node[segments[0]]
	if !ok {
		return nil, false, nil
	}
	if len(segments) == 1 {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, true, fmt.Errorf("non-array %q: %w", segments[0], err)
		}
		return items, true, nil
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(raw, &child); err != nil {
		return nil, false, fmt.Errorf("non-object %q: %w", segments[0], err)
	}
	return arrayAtPath(child, segments[1:])
}

// setAtPath is arrayAtPath's write side: it stores items at the path,
// preserving every sibling key at every level by decoding only the segments
// on the path itself and re-encoding them outside-in.
func setAtPath(node map[string]json.RawMessage, segments []string, items []json.RawMessage) error {
	seg := segments[0]
	if len(segments) == 1 {
		encoded, err := json.Marshal(items)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", seg, err)
		}
		node[seg] = encoded
		return nil
	}
	child := map[string]json.RawMessage{}
	if raw, ok := node[seg]; ok {
		if err := json.Unmarshal(raw, &child); err != nil {
			return fmt.Errorf("non-object %q: %w", seg, err)
		}
	}
	if err := setAtPath(child, segments[1:], items); err != nil {
		return err
	}
	encoded, err := json.Marshal(child)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", seg, err)
	}
	node[seg] = encoded
	return nil
}

// interleave takes one item from each network in turn until all are drained,
// so every network gets proportional presence in the result instead of the
// first network in the routing config filling the front of it.
func interleave(perNetwork [][]json.RawMessage) []json.RawMessage {
	total, longest := 0, 0
	for _, items := range perNetwork {
		total += len(items)
		if len(items) > longest {
			longest = len(items)
		}
	}
	out := make([]json.RawMessage, 0, total)
	for i := 0; i < longest; i++ {
		for _, items := range perNetwork {
			if i < len(items) {
				out = append(out, items[i])
			}
		}
	}
	return out
}
