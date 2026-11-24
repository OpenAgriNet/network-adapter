// Splitting a mapped request between the query string and the body, and
// rendering the query half.
package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// QueryField is the reserved key a mapping puts its query parameters under.
//
// Reserved in the same sense as _error and _local: it is not a field any
// provider is sent, it is an instruction to this binary. A mapping writes
//
//	request: | { "_query": { "grievanceNo": beckn.message.contract.id } }
//
// when the upstream reads that value from the URL rather than the body --
// KHC's check-status is a POST that answers 400 "grievanceNo is required" to
// the body form and reads the grievance from the query form. Where a value
// goes is the provider's own quirk, and the mapping is the file that already
// describes those; a registry row names the method and the path, which is
// everything except this.
//
// Fields outside _query are unaffected, so a mapping can send both at once.
// A mapping that does not use the key produces exactly the bytes it did
// before.
const QueryField = "_query"

// Split divides a mapped request into the part that travels in the query
// string and the part that travels in the body.
//
// The reserved _query object always becomes query parameters. Everything else
// becomes the body when the method has one, and becomes query parameters too
// when it does not -- a GET has nowhere else to put it.
//
// A request that never names the key comes back as the bytes it already was.
// Re-marshalling re-orders keys, and a body is not always a JSON object -- a
// sealed envelope is a string, and an upstream expecting a list gets one -- so
// a mapping with nothing to do with this produces exactly the request it did
// before.
func Split(mapped []byte, hasBody bool) (string, []byte, error) {
	if len(bytes.TrimSpace(mapped)) == 0 {
		return "", nil, nil
	}
	// Checked as text, and nothing is parsed without it.
	if !bytes.Contains(mapped, []byte(`"`+QueryField+`"`)) {
		return split(mapped, hasBody)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mapped, &fields); err != nil {
		return "", nil, fmt.Errorf(
			"mapped request is not an object, so it cannot become a query: %w", err)
	}
	reserved, named := fields[QueryField]
	if !named {
		// The text sits deeper -- inside a value, or a provider field genuinely
		// called _query. Not ours to lift out.
		return split(mapped, hasBody)
	}

	query, err := asQuery(reserved)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", QueryField, err)
	}
	delete(fields, QueryField)
	if len(fields) == 0 {
		return query, nil, nil
	}
	rest, err := json.Marshal(fields)
	if err != nil {
		return "", nil, err
	}
	if hasBody {
		return query, rest, nil
	}
	// A bodyless method that named some of its fields anyway: named and
	// unnamed go to the same place, so this is redundant rather than wrong --
	// and a mapping moved from POST to GET must not silently drop the half it
	// named.
	others, err := asQuery(rest)
	if err != nil {
		return "", nil, err
	}
	if query == "" || others == "" {
		return query + others, nil, nil
	}
	return query + "&" + others, nil, nil
}

// split places a request that named nothing: the body for a method that has
// one, the query string for a method that does not.
func split(mapped []byte, hasBody bool) (string, []byte, error) {
	if hasBody {
		return "", mapped, nil
	}
	query, err := asQuery(mapped)
	return query, nil, err
}

// asQuery renders a mapped request as query parameters, for a method with no
// body. Scalars only -- see asQueryValue.
func asQuery(mapped []byte) (string, error) {
	if len(bytes.TrimSpace(mapped)) == 0 {
		return "", nil
	}
	var fields map[string]any
	if err := json.Unmarshal(mapped, &fields); err != nil {
		return "", fmt.Errorf("mapped request is not an object, so it cannot become a query: %w", err)
	}

	values := url.Values{}
	for name, value := range fields {
		rendered, ok := asQueryValue(value)
		if !ok {
			return "", fmt.Errorf("mapped field %q is not a scalar and cannot become a query parameter", name)
		}
		values.Set(name, rendered)
	}
	return values.Encode(), nil
}

// asQueryValue renders one mapped field as a query parameter value, reporting
// false when the field cannot be one.
//
// The three JSON scalars and nothing else. An object or array has no single
// obvious encoding -- repeated keys, comma-joined, indexed are all in use
// somewhere -- so choosing here would put a convention in Go that belongs in
// the mapping. The caller turns false into an error naming the field.
//
// null is refused rather than sent as empty: present-but-empty and absent mean
// different things to some upstreams, and a mapping says which by omitting the
// field or setting "". An empty string IS carried, being a scalar.
func asQueryValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		// 'f' never switches to an exponent, so a 7-digit identifier stays
		// 1234567 rather than becoming 1.234567e+06, which no upstream parses.
		// -1 precision still round-trips, so 19.9975 does not gain zeros.
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	default:
		return "", false
	}
}
