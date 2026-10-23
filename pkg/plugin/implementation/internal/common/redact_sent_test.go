package common

import (
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// A provider refusing a call commonly quotes the request back. What we sent it
// carries the farmer's mobile, enrolment id and complaint, which the packs
// mark no-log -- so none of it may survive into the log line.
func TestRedactSent_MasksWhatTheRequestCarried(t *testing.T) {
	sent := []byte(`{"requestorMobileNo":"9876543210",` +
		`"GrievenceSupportTicketNo":"100626000099001",` +
		`"ApplicationNo":"040108251010160770605",` +
		`"GrievenceDescription":"Claim not received for Kharif"}`)

	text := "No record for 9876543210 against ticket 100626000099001 " +
		"(enrolment 040108251010160770605): Claim not received for Kharif"

	got := redactSent(text, sent)
	for _, secret := range []string{"9876543210", "100626000099001",
		"040108251010160770605", "Claim not received for Kharif"} {
		if strings.Contains(got, secret) {
			t.Errorf("redactSent() kept %q; the request's own data must not reach the log:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, util.RedactedMarker) {
		t.Errorf("redactSent() = %q, want the values replaced by the marker", got)
	}
	if !strings.Contains(got, "No record for") {
		t.Errorf("redactSent() = %q, want the provider's own wording kept", got)
	}
}

// Masking every short code would shred the sentence it was meant to protect.
func TestRedactSent_KeepsTextThatIsNotOurs(t *testing.T) {
	sent := []byte(`{"season":"1","category":"2","requestorMobileNo":"9876543210"}`)
	got := redactSent("Season 1 category 2 is closed", sent)
	if got != "Season 1 category 2 is closed" {
		t.Errorf("redactSent() = %q, want the sentence unchanged", got)
	}
}

// A number mapped as a number echoes as digits, and digits identify a farmer.
func TestRedactSent_MasksNumbersAsDigits(t *testing.T) {
	got := redactSent("ticket 109301 not found", []byte(`{"GrievenceSupportTicketID":109301}`))
	if strings.Contains(got, "109301") {
		t.Errorf("redactSent() = %q, want the sent id masked", got)
	}
}

func TestRedactSent_SurvivesNonJSONAndEmpty(t *testing.T) {
	for name, sent := range map[string][]byte{
		"not json": []byte("<html>no</html>"),
		"empty":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := redactSent("upstream said no", sent); got != "upstream said no" {
				t.Errorf("redactSent() = %q, want the text unchanged", got)
			}
		})
	}
}
