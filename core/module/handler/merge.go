package handler

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

// keptResponse is one target's accepted answer: the envelope body it sent,
// and the array read out of it at the configured field path (nil if it had
// none -- a network with no matches is a valid answer, not an error).
type keptResponse struct {
	body     []byte
	items    []json.RawMessage
	hasItems bool
}

// itemsOf pulls the array at fieldPath (a dot path from the envelope root,
// e.g. "message.catalogs" or "message.contract.commitments") out of one
// response body, without decoding the items themselves so a member this
// build does not know about is re-emitted byte for byte rather than dropped.
//
// A response carrying nothing at fieldPath contributes nothing rather than
// failing the merge: a network with no matches is a valid answer.
func itemsOf(body []byte, fieldPath string) ([]json.RawMessage, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("not a JSON object: %w", err)
	}
	return arrayAtPath(envelope, strings.Split(fieldPath, "."))
}

// mergeResponses folds several kept responses into one envelope, merging the
// array at fieldPath across all of them.
//
// The donor -- the first kept response in TARGET ORDER (not arrival order)
// that carries fieldPath -- supplies context and message verbatim; this code
// never synthesizes a context of its own. A 200 carrying a Beckn error
// envelope is kept (not a transport failure) but never donates: it has no
// fieldPath, so hasItems is false regardless of target order.
func mergeResponses(kept []keptResponse, fieldPath string, limit int, hasLimit bool) ([]byte, error) {
	donor := -1
	for i, k := range kept {
		if k.hasItems {
			donor = i
			break
		}
	}
	if donor < 0 {
		return nil, fmt.Errorf("no response carried %s: a rule with several targets is only meaningful for an action whose replies carry it", fieldPath)
	}

	var donorEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(kept[donor].body, &donorEnvelope); err != nil {
		return nil, fmt.Errorf("donor response is not a JSON object: %w", err)
	}

	// Rebuilt rather than edited in place: only context and message belong in
	// the answer. message is seeded from the donor here so setAtPath below
	// preserves every sibling along fieldPath instead of starting empty.
	envelope := map[string]json.RawMessage{}
	if raw, ok := donorEnvelope[contextKey]; ok {
		envelope[contextKey] = raw
	}
	if raw, ok := donorEnvelope[messageKey]; ok {
		envelope[messageKey] = raw
	}

	perNetwork := make([][]json.RawMessage, 0, len(kept))
	for _, k := range kept {
		perNetwork = append(perNetwork, k.items)
	}
	merged := dedupe(interleave(perNetwork))
	if hasLimit && len(merged) > limit {
		merged = merged[:limit]
	}
	if merged == nil {
		merged = []json.RawMessage{}
	}

	if err := setAtPath(envelope, strings.Split(fieldPath, "."), merged); err != nil {
		return nil, fmt.Errorf("writing merged %s: %w", fieldPath, err)
	}
	return json.Marshal(envelope)
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
// so a limit gives every network proportional presence instead of the first
// network in the routing config filling the whole page.
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

// dedupe drops repeats of an item id, keeping the first occurrence. An item
// with no readable id is kept rather than dropped: its absence means unknown,
// not duplicate.
func dedupe(items []json.RawMessage) []json.RawMessage {
	seen := make(map[string]bool, len(items))
	out := items[:0]
	for _, item := range items {
		id := itemID(item)
		if id == "" || !seen[id] {
			if id != "" {
				seen[id] = true
			}
			out = append(out, item)
		}
	}
	return out
}

// itemID reads an item's id without decoding the rest of it.
func itemID(item json.RawMessage) string {
	var fields struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(item, &fields); err != nil {
		return ""
	}
	return fields.ID
}
