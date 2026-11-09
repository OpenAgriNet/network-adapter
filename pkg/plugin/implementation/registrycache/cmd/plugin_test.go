package main

import (
	"context"
	"testing"
)

func TestNew(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]string
		ok   bool
	}{
		{"url and ttl without preload", map[string]string{"url": "http://registry:8081/api/v1", "ttl": "24h"}, false},
		{"no ttl", map[string]string{"url": "http://registry:8081/api/v1", "preload": "Participant"}, false},
		{"zero ttl", map[string]string{"url": "http://r", "ttl": "0s"}, false},
		{"missing url", map[string]string{"ttl": "24h", "preload": "Participant"}, false},
		{"bad ttl", map[string]string{"url": "http://r", "ttl": "a day"}, false},
		{"negative ttl", map[string]string{"url": "http://r", "ttl": "-1h"}, false},
		{"preload", map[string]string{"url": "http://r", "ttl": "24h", "preload": "Participant, ProviderSchema"}, true},
		{"preload with a field", map[string]string{"url": "http://r", "ttl": "24h", "preload": "Participant:participantId"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, _, err := Provider.New(context.Background(), nil, tc.cfg)
			if (err == nil) != tc.ok || (rc != nil) != tc.ok {
				t.Fatalf("got rc=%v err=%v, want ok=%v", rc, err, tc.ok)
			}
		})
	}
}
