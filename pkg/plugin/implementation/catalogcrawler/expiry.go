package catalogcrawler

// expiry.go — how long a crawled copy is served for.

import (
	"encoding/json"
	"fmt"
	"time"
)

// withExpiry stamps when our copy stops being served. One the owner stops
// sharing is never refreshed again, so it lapses on its own.
func withExpiry(document json.RawMessage, until time.Time) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, fmt.Errorf("catalogcrawler: a crawled catalog is not an object: %w", err)
	}

	// Only the calendar end moves. startDate is the owner saying when it goes
	// live, and the daily window is their opening hours.
	validity := map[string]json.RawMessage{}
	if raw, ok := doc["validity"]; ok {
		if err := json.Unmarshal(raw, &validity); err != nil {
			return nil, fmt.Errorf("catalogcrawler: a crawled catalog's validity is not an object: %w", err)
		}
	}
	endDate, err := json.Marshal(until.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	validity["endDate"] = endDate

	stamped, err := json.Marshal(validity)
	if err != nil {
		return nil, err
	}
	doc["validity"] = stamped
	return json.Marshal(doc)
}
