package federation

import (
	"errors"
	"fmt"
	"strings"
)

// RoleNetwork is the registry role that makes a participant a peer network
// rather than one of this network's own consumers or providers.
const RoleNetwork = "network"

// Participant is the registry record for an admitted peer network.
//
// It maps onto the registry's existing Participant schema: NetworkID is
// participantId, DiscoveryURL is baseUrl, Status is status. Revision and
// AdmittedKeyID are the two fields federation adds.
//
// Keys is a plain slice and not the KeySet that was fetched. What a peer
// PUBLISHES and what we ADMIT are different things -- the governance key is in
// one and must not be in the other -- and giving them one type invites assigning
// the first to the second without noticing.
type Participant struct {
	NetworkID     string
	Name          string
	DiscoveryURL  string
	Keys          []JWK
	Status        string
	Revision      int
	AdmittedKeyID string
}

var (
	// ErrStaleRevision reports a descriptor that does not advance the revision.
	ErrStaleRevision = errors.New("descriptor revision did not increase")
	// ErrKeyChanged reports a descriptor signed by a different key than the one
	// this peer was admitted under.
	ErrKeyChanged = errors.New("descriptor is signed by a different key than the admitted one")
	// ErrWrongNetwork reports a descriptor for a different network than the
	// record being refreshed.
	ErrWrongNetwork = errors.New("descriptor is for a different network")
)

// StatusActive and StatusInactive are the two the registry uses.
const (
	StatusActive   = "active"
	StatusInactive = "inactive"
)

// Admit turns a verified descriptor into the registry record to write.
//
// current is nil on first admission; on every later call it is what the registry
// already holds, which is what makes the revision and key checks possible.
//
// Three rules, and each exists because of a specific way a peer could otherwise
// take something back or take something over:
//
//   - The revision must increase, so a superseded descriptor cannot be replayed.
//   - The signing key may not change, so possession of one signing key cannot be
//     used to hand the network's identity to another key.
//   - Status is carried over and never read from the descriptor, so a peer an
//     operator suspended cannot reinstate itself by publishing a new file. What
//     the peer says about itself is a claim; what we recorded is a decision.
func Admit(current *Participant, fresh Descriptor, keys KeySet) (Participant, error) {
	if fresh.NetworkID == "" {
		return Participant{}, ErrMissingNetwork
	}
	if err := Verify(fresh, keys); err != nil {
		return Participant{}, fmt.Errorf("admit %s: %w", fresh.NetworkID, err)
	}

	status := StatusActive
	if current != nil {
		if err := refreshable(*current, fresh); err != nil {
			return Participant{}, err
		}
		status = current.Status
	}

	return Participant{
		NetworkID:     fresh.NetworkID,
		Name:          fresh.Name,
		DiscoveryURL:  fresh.Services.Discovery,
		Keys:          operationalKeys(keys, fresh.Signature.KeyID),
		Status:        status,
		Revision:      fresh.Revision,
		AdmittedKeyID: fresh.Signature.KeyID,
	}, nil
}

// refreshable reports whether a fresh descriptor may replace a stored record.
func refreshable(current Participant, fresh Descriptor) error {
	if !strings.EqualFold(fresh.NetworkID, current.NetworkID) {
		return fmt.Errorf("%w: descriptor is for %q, stored peer is %q",
			ErrWrongNetwork, fresh.NetworkID, current.NetworkID)
	}
	if fresh.Revision <= current.Revision {
		return fmt.Errorf("%w: hold %d, offered %d",
			ErrStaleRevision, current.Revision, fresh.Revision)
	}
	if fresh.Signature.KeyID != current.AdmittedKeyID {
		return fmt.Errorf("%w: admitted under %q, now signed by %q",
			ErrKeyChanged, current.AdmittedKeyID, fresh.Signature.KeyID)
	}
	return nil
}

// operationalKeys returns the peer's keys WITHOUT the one that signed the
// descriptor.
//
// The registry record is what the signature validator reads to verify ordinary
// traffic. Writing the governance key there would let a peer sign a discover
// with the key that is only supposed to sign its descriptor -- and the
// validator, which checks that a key is published and active and nothing more,
// would accept it. The contract forbids a governance key from signing normal
// traffic; this is the line that enforces it, because nothing downstream can
// tell the two apart.
func operationalKeys(all KeySet, governanceKeyID string) []JWK {
	operational := make([]JWK, 0, len(all.Keys))
	for _, key := range all.Keys {
		if key.Kid == governanceKeyID {
			continue
		}
		operational = append(operational, key)
	}
	return operational
}

// Suspend returns the record with this peer marked inactive.
//
// Nothing is fetched. Suspension is a decision about a network we have stopped
// trusting, and asking that network's own server to take part in it would be
// absurd. It is separate from Admit for the same reason Admit never sets status:
// the two must not be reachable from one another.
func Suspend(current Participant) Participant {
	current.Status = StatusInactive
	return current
}
