package federation

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// RoleNetwork is the registry role that makes a participant a peer network
// rather than one of this network's own consumers or providers.
const RoleNetwork = "network"

// Participant is the registry record for an admitted peer network.
//
// It maps onto the registry's existing Participant schema: NetworkID is
// participantId, DiscoveryURL is baseUrl, Status is status. Revision is the
// field federation adds.
//
// Keys is a plain slice and not the KeySet that was fetched. What a peer
// PUBLISHES and what we ADMIT are different things -- the governance key is in
// one and must not be in the other -- and giving them one type invites assigning
// the first to the second without noticing.
type Participant struct {
	NetworkID    string
	Name         string
	DiscoveryURL string
	Keys         []JWK
	Status       string
	Revision     int

	// ProjectionTtl is how long we may keep what we crawl from this peer.
	// Always positive on an admitted record: zero is refused at admission.
	ProjectionTtl time.Duration
}

// DefaultProjectionTtl is what a peer that declares none is held to.
//
// Finite, and deliberately short. A descriptor silent about how long its data
// may be kept has not granted an unlimited licence, and the safe reading of
// silence is "not for long" rather than "for ever".
const DefaultProjectionTtl = time.Hour

// projectionTtl resolves what a descriptor permits.
//
// The three cases are genuinely different and none may be folded into another:
// absent is consent to decide for ourselves, zero is a refusal to be cached,
// and a positive value is a bounded licence.
func projectionTtl(d Descriptor) (time.Duration, error) {
	if d.ProjectionTtl == nil {
		return DefaultProjectionTtl, nil
	}
	switch seconds := *d.ProjectionTtl; {
	case seconds < 0:
		return 0, fmt.Errorf("projectionTtl is negative: %d", seconds)
	case seconds == 0:
		return 0, ErrLiveQueryOnly
	default:
		return time.Duration(seconds) * time.Second, nil
	}
}

var (
	// ErrStaleRevision reports a descriptor that does not advance the revision.
	ErrStaleRevision = errors.New("descriptor revision did not increase")
	// ErrWrongNetwork reports a descriptor for a different network than the
	// record being refreshed.
	ErrWrongNetwork = errors.New("descriptor is for a different network")
	// ErrLiveQueryOnly reports a peer that forbids caching. Honouring it means
	// querying it on every discover, which this build does not do.
	ErrLiveQueryOnly = errors.New("peer declares projectionTtl 0 (live query only), which is not implemented")
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
// Two rules, and each exists because of a specific way a peer could otherwise
// take something back or take something over:
//
//   - The revision must increase, so a superseded descriptor cannot be replayed.
//     This, and the fact that the documents are served from the network's own
//     domain, is what the contract rests identity on -- a peer's right to speak
//     for itself comes from controlling that domain, not from holding one
//     particular key, which is also what leaves key rotation open to it.
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

	ttl, err := projectionTtl(fresh)
	if err != nil {
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
		ProjectionTtl: ttl,
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
