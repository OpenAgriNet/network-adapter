package pipeline

// classify.go turns a provider's `upstream.errors` block into a decision about
// one failed call.
//
// This used to be a constant. The engine recognised "this region has no rows"
// by matching one upstream's literal string -- `400` plus the text
// "No data available." -- which meant a second provider could not say the same
// thing in its own words without an engine change, and the file's own errors
// block sat there declaring rules nothing read.
//
// The distinction is worth getting from the file rather than from Go because
// it is not cosmetic: recording "no rows" as a failure once turned 27 of 36
// quiet regions into 27 reported outages, on a collection that was as complete
// as the upstream allowed.

import (
	"bytes"
	"fmt"
	"strings"
)

// The classifications a rule may name. They are the vocabulary the schema
// enforces and a step's `onError` dispatches on, so the three spellings live
// here once rather than as literals scattered through the engine.
const (
	classifyEmpty     = "emptyResult"
	classifyReauth    = "reauth"
	classifyTransport = "transportError"
)

// classify decides what a response means, by the rules the file declares.
//
// Rules are ordered and the FIRST match wins, so a file reads top to bottom: a
// specific rule above a general one behaves the way its reader expects.
//
// DECLARED RULES ARE CONSULTED FIRST, including for a 2xx. An upstream that
// answers "nothing here" with a 204, or with a 200 carrying an error object,
// is ordinary -- and a blanket "2xx is success" would make such a provider
// unable to say so, which is the hardcoding this file exists to remove.
//
// Only when no rule matches does status decide: a 2xx is success, and a
// failure is a transportError. Never an empty result -- treating an
// unrecognised failure as "nothing here" is how an outage becomes coverage.
func classify(rules []ErrorRule, status int, body []byte) string {
	for _, rule := range rules {
		if rule.When == nil {
			continue // the default is applied below, after every `when` has had its turn
		}
		if matches(*rule.When, status, body) {
			return rule.Classify
		}
	}

	if status >= 200 && status < 300 {
		return ""
	}

	// The declared catch-all, if the file states one.
	for _, rule := range rules {
		if rule.When == nil && strings.TrimSpace(rule.Default) != "" {
			return rule.Default
		}
	}

	return classifyTransport
}

// matches reports whether one rule's `when` describes this response.
//
// An empty `when` matches nothing rather than everything: a rule that acquired
// an empty match through an editing mistake would otherwise swallow every
// failure, which is the loudest possible way to go quiet.
func matches(when ErrorMatch, status int, body []byte) bool {
	statusStated := when.Status != nil
	bodyStated := strings.TrimSpace(when.BodyContains) != ""
	if !statusStated && !bodyStated {
		return false
	}

	if statusStated && !statusMatches(when.Status, status) {
		return false
	}
	if bodyStated && !bytes.Contains(body, []byte(when.BodyContains)) {
		return false
	}
	return true
}

// statusMatches accepts one status or several, because a file should not have
// to write a list for the single-status case.
func statusMatches(stated any, status int) bool {
	switch typed := stated.(type) {
	case int:
		return typed == status
	case []any:
		for _, item := range typed {
			if asInt, ok := toInt(item); ok && asInt == status {
				return true
			}
		}
		return false
	case []int:
		for _, item := range typed {
			if item == status {
				return true
			}
		}
		return false
	default:
		if asInt, ok := toInt(stated); ok {
			return asInt == status
		}
		return false
	}
}

// toInt reads the number types YAML can hand back for a status.
func toInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

// classifiedError builds the error a classification produces.
//
// An emptyResult wraps ErrNoUpstreamData, because that sentinel is what a
// step's onError dispatches on several frames up -- errors.Is is what tells a
// quiet region from a broken one, and a classification that did not produce it
// would be a rule the file states and nothing performs.
//
// THE BODY IS NEVER QUOTED, whatever it contained. This class of upstream
// echoes the request back in its error bodies, and the request carries the
// token in its query string; the error ends up in terminals and tickets.
func classifiedError(classification string, status int, call string) error {
	// Defence in depth: a caller may hand over a path that still carries a
	// query, and for this class of upstream the query is where the token
	// rides. Stripping it here means no call site can leak one by forgetting.
	if cut, _, found := strings.Cut(call, "?"); found {
		call = cut
	}

	switch classification {
	case "":
		return nil
	case classifyEmpty:
		return fmt.Errorf("%s: %w", call, ErrNoUpstreamData)
	case classifyReauth:
		return fmt.Errorf("%s: the upstream rejected the token (status %d)", call, status)
	default:
		return fmt.Errorf("%s returned status %d", call, status)
	}
}
