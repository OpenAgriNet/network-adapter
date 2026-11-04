package common

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// rowEncryptionHook stands in for a domain package's builder, recording what it
// was asked for.
type rowEncryptionHook struct {
	envelope Envelope
	hosted   map[string]string
	err      error

	calls      int
	bindingKey string
	encryption map[string]any
	secrets    Secrets
}

func (h *rowEncryptionHook) build(bindingKey string, encryption map[string]any, secrets Secrets) (Envelope, map[string]string, error) {
	h.calls++
	h.bindingKey, h.encryption, h.secrets = bindingKey, encryption, secrets
	return h.envelope, h.hosted, h.err
}

func encryptedPlan(baseURL string, encryption map[string]any) *model.ProviderRecord {
	plan := planWithAction(baseURL, "select")
	plan.Encryption = encryption
	return plan
}

func TestRunSealsWithTheRowsEnvelope(t *testing.T) {
	var sent string
	url := recordingUpstream(t, string(reverse([]byte(`{"answer":42}`))), &sent)
	encryption := map[string]any{"type": "aesGcm"}
	hook := &rowEncryptionHook{envelope: &reversingEnvelope{}}
	mapper := &stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"ok":true}`)}
	step := newStep(t, &stubRegistry{plan: encryptedPlan(url, encryption)}, mapper,
		func(c *Config) { c.EncryptionFromRow = hook.build })

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if sent != string(reverse([]byte(`{"ask":1}`))) {
		t.Errorf("upstream received %q, want the request sealed by the row's envelope", sent)
	}
	if hook.bindingKey != testBindingKey || !reflect.DeepEqual(hook.encryption, encryption) {
		t.Errorf("hook was given %q %v, want %q %v", hook.bindingKey, hook.encryption, testBindingKey, encryption)
	}
}

func TestRunSendsAnActionTheRowHostsElsewhereToThatHost(t *testing.T) {
	var onElsewhere, onParticipant string
	elsewhere := recordingUpstream(t, `{"ok":true}`, &onElsewhere)
	participant := recordingUpstream(t, `{"ok":true}`, &onParticipant)
	registry := keyedRegistry{
		testBindingKey:  encryptedPlan(participant, map[string]any{"type": "aesCbcKeyInBand"}),
		otherBindingKey: {BindingKey: otherBindingKey, BaseURL: elsewhere},
	}
	hook := &rowEncryptionHook{hosted: map[string]string{"select": otherBindingKey}}
	mapper := &stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"a":1}`)}
	step := newStep(t, registry, mapper, func(c *Config) { c.EncryptionFromRow = hook.build })

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if onElsewhere == "" || onParticipant != "" {
		t.Errorf("the named host got %q, the participant got %q; want the named host called", onElsewhere, onParticipant)
	}
}

func TestRunRefusesARowAskingForEncryptionTheStepCannotApply(t *testing.T) {
	var sent string
	url := recordingUpstream(t, `{"ok":true}`, &sent)
	step := newStep(t, &stubRegistry{plan: encryptedPlan(url, map[string]any{"type": "aesGcm"})},
		&stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"a":1}`)})

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("Run() served a row asking for encryption on a step that has none")
	}
	if !strings.Contains(err.Error(), testBindingKey) {
		t.Errorf("error %q does not name the binding", err)
	}
	if sent != "" {
		t.Errorf("the provider was sent %q unencrypted", sent)
	}
}

func TestRunRefusesARowWhoseEncryptionIsBrokenOnEveryRequest(t *testing.T) {
	var sent string
	url := recordingUpstream(t, `{"ok":true}`, &sent)
	hook := &rowEncryptionHook{err: errors.New("aesGcm requires aesKeyRef")}
	step := newStep(t, &stubRegistry{plan: encryptedPlan(url, map[string]any{"type": "aesGcm"})},
		&stubMapper{requestResult: []byte(`{"ask":1}`), responseResult: []byte(`{"a":1}`)},
		func(c *Config) { c.EncryptionFromRow = hook.build })

	for range 2 {
		_, err := runStep(t, step, selectBody)
		if err == nil {
			t.Fatal("Run() served a row whose encryption is broken")
		}
		if !strings.Contains(err.Error(), testBindingKey) || !strings.Contains(err.Error(), "aesKeyRef") {
			t.Errorf("error %q does not name the binding and the reason", err)
		}
	}
	if hook.calls != 2 {
		t.Errorf("hook called %d times, want 2: a broken row must be retried once fixed, not remembered", hook.calls)
	}
	if sent != "" {
		t.Errorf("the provider was sent %q", sent)
	}
}

func TestEnvelopeForBuildsOnceUntilTheRowChanges(t *testing.T) {
	t.Parallel()

	hook := &rowEncryptionHook{envelope: &reversingEnvelope{}}
	step := newStep(t, &stubRegistry{}, &stubMapper{}, func(c *Config) { c.EncryptionFromRow = hook.build })

	for range 2 {
		if _, _, err := step.envelopeFor(context.Background(), encryptedPlan("http://unused", map[string]any{"type": "aesGcm"})); err != nil {
			t.Fatalf("envelopeFor() = %v", err)
		}
	}
	if hook.calls != 1 {
		t.Errorf("the same row was built %d times, want once", hook.calls)
	}

	if _, _, err := step.envelopeFor(context.Background(), encryptedPlan("http://unused", map[string]any{"type": "aesCbcKeyInBand"})); err != nil {
		t.Fatalf("envelopeFor() = %v", err)
	}
	if hook.calls != 2 {
		t.Errorf("a changed row was built %d times in total, want 2", hook.calls)
	}
}

func TestEnvelopeForUsesTheConfigWhenTheRowHasNone(t *testing.T) {
	t.Parallel()

	configured := &reversingEnvelope{}
	hook := &rowEncryptionHook{envelope: &reversingEnvelope{}}
	step := newStep(t, &stubRegistry{}, &stubMapper{}, func(c *Config) {
		c.Envelopes = map[string]Envelope{testProvider: configured}
		c.EncryptionFromRow = hook.build
	})

	got, _, err := step.envelopeFor(context.Background(), planWithAction("http://unused", "select"))
	if err != nil {
		t.Fatalf("envelopeFor() = %v", err)
	}
	if got != configured {
		t.Error("a row without encryption did not use the config's envelope")
	}
	if hook.calls != 0 {
		t.Errorf("hook called %d times for a row without encryption", hook.calls)
	}
}

func TestRowEnvelopeIsGivenTheProvidersStoreEntry(t *testing.T) {
	t.Parallel()

	hook := &rowEncryptionHook{envelope: &reversingEnvelope{}}
	store := &fakeSecretStore{entries: map[string]map[string]string{testProvider: {"PMKISAN_OTP_TOKEN": "from-store"}}}
	step := newStep(t, &stubRegistry{}, &stubMapper{}, func(c *Config) {
		c.EncryptionFromRow = hook.build
		c.SecretStore = store
	})

	if _, _, err := step.envelopeFor(context.Background(), encryptedPlan("http://unused", map[string]any{"type": "aesGcm"})); err != nil {
		t.Fatalf("envelopeFor() = %v", err)
	}
	if got := hook.secrets.Value(context.Background(), "PMKISAN_OTP_TOKEN"); got != "from-store" {
		t.Errorf("the builder's secrets read %q, want the provider's store entry", got)
	}
}

func TestRowEnvelopeReadsTheEnvironmentWithoutAStore(t *testing.T) {
	t.Setenv("PMKISAN_OTP_TOKEN", "from-env")
	hook := &rowEncryptionHook{envelope: &reversingEnvelope{}}
	step := newStep(t, &stubRegistry{}, &stubMapper{}, func(c *Config) { c.EncryptionFromRow = hook.build })

	if _, _, err := step.envelopeFor(context.Background(), encryptedPlan("http://unused", map[string]any{"type": "aesGcm"})); err != nil {
		t.Fatalf("envelopeFor() = %v", err)
	}
	if got := hook.secrets.Value(context.Background(), "PMKISAN_OTP_TOKEN"); got != "from-env" {
		t.Errorf("the builder's secrets read %q, want the environment's", got)
	}
}
