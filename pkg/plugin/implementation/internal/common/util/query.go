// Rendering a mapped request as query parameters, for methods with no body.
package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

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
		// 'g' with -1 precision round-trips without inventing trailing zeros, so
		// 19.9975 stays 19.9975 rather than becoming 19.997500.
		return strconv.FormatFloat(typed, 'g', -1, 64), true
	default:
		return "", false
	}
}
