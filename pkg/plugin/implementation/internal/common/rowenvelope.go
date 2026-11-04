// Envelopes built from a provider's registry row instead of the step config, so
// a new provider needs only its row.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// EncryptionFromRow builds a binding's envelope from its row's encryption block,
// reading keys from secrets, with the actions it sends to another binding's host.
type EncryptionFromRow func(bindingKey string, encryption map[string]any, secrets Secrets) (Envelope, map[string]string, error)

// rowEnvelopes holds one envelope per binding whose row carries encryption,
// rebuilt only when that block changes.
type rowEnvelopes struct {
	mu        sync.Mutex
	byBinding map[string]rowEnvelope
}

type rowEnvelope struct {
	fingerprint string
	envelope    Envelope
	hosted      map[string]string
}

// envelopeFor is plan's envelope and the actions it sends elsewhere: from its
// row when it carries encryption, else from the step config.
func (s *Step) envelopeFor(ctx context.Context, plan *model.ProviderRecord) (Envelope, map[string]string, error) {
	if plan.Encryption == nil {
		return forBinding(s.config.Envelopes, plan.BindingKey), forBinding(s.config.HostedElsewhere, plan.BindingKey), nil
	}
	if s.config.EncryptionFromRow == nil {
		// Refused rather than sent plain: the row says this provider expects encryption.
		err := fmt.Errorf("%s: its registry row asks for encryption, which this step cannot apply", plan.BindingKey)
		log.Errorf(ctx, err, "Refusing %s", plan.BindingKey)
		return nil, nil, err
	}
	return s.rowEnvelopes.forRow(ctx, plan, s.config.EncryptionFromRow, s.config.SecretStore)
}

func (r *rowEnvelopes) forRow(ctx context.Context, plan *model.ProviderRecord, build EncryptionFromRow,
	store SecretStore) (Envelope, map[string]string, error) {
	fingerprint, err := json.Marshal(plan.Encryption)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: its registry row's encryption block cannot be read: %w", plan.BindingKey, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	held, seen := r.byBinding[plan.BindingKey]
	if seen && held.fingerprint == string(fingerprint) {
		return held.envelope, held.hosted, nil
	}

	envelope, hosted, err := build(plan.BindingKey, plan.Encryption, rowSecrets(ctx, plan, store))
	if err != nil {
		log.Errorf(ctx, err, "Refusing %s: the encryption block in its registry row is not usable", plan.BindingKey)
		return nil, nil, fmt.Errorf("%s: the encryption block in its registry row is not usable: %w", plan.BindingKey, err)
	}
	if r.byBinding == nil {
		r.byBinding = map[string]rowEnvelope{}
	}
	r.byBinding[plan.BindingKey] = rowEnvelope{fingerprint: string(fingerprint), envelope: envelope, hosted: hosted}

	if seen {
		log.Infof(ctx, "%s: encryption changed in its registry row, envelope rebuilt", plan.BindingKey)
	} else {
		log.Infof(ctx, "%s: encryption taken from its registry row", plan.BindingKey)
	}
	return envelope, hosted, nil
}
