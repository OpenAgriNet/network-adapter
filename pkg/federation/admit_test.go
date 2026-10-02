package federation

import (
	"crypto/ed25519"
	"errors"
	"testing"
)

// network is one peer's published material, kept together so a test can issue a
// LATER descriptor from the same identity -- which is what a refresh is, and
// what every rule below is really about.
type network struct {
	keys    KeySet
	private ed25519.PrivateKey
}

// newNetwork publishes a governance key and an operational key, so the split
// between what signs the descriptor and what signs traffic can be asserted.
func newNetwork(t *testing.T) *network {
	t.Helper()
	govPublic, govPrivate, _ := ed25519.GenerateKey(nil)
	opPublic, _, _ := ed25519.GenerateKey(nil)

	return &network{
		private: govPrivate,
		keys: KeySet{Keys: []JWK{
			{Kty: "OKP", Crv: "Ed25519", Kid: "gov-1", X: EncodeKey(govPublic)},
			{Kty: "OKP", Crv: "Ed25519", Kid: "op-1", X: EncodeKey(opPublic)},
		}},
	}
}

// issues signs a descriptor at a revision, as this network would publish it.
func (n *network) issues(t *testing.T, revision int) Descriptor {
	t.Helper()
	signed, err := Sign(descriptorAt(revision), "gov-1", n.private)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return signed
}

func admittable(t *testing.T, revision int) (Descriptor, KeySet) {
	t.Helper()
	n := newNetwork(t)
	return n.issues(t, revision), n.keys
}

func TestAdmitBuildsTheRecord(t *testing.T) {
	descriptor, keys := admittable(t, 1)

	admitted, err := Admit(nil, descriptor, keys)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	if admitted.NetworkID != descriptor.NetworkID {
		t.Errorf("networkId = %q, want %q", admitted.NetworkID, descriptor.NetworkID)
	}
	if admitted.DiscoveryURL != descriptor.Services.Discovery {
		t.Errorf("discovery = %q, want the descriptor's", admitted.DiscoveryURL)
	}
	if admitted.Status != StatusActive {
		t.Errorf("status = %q, want %q on first admission", admitted.Status, StatusActive)
	}
	if admitted.Revision != 1 || admitted.AdmittedKeyID != "gov-1" {
		t.Errorf("revision/key = %d/%q, want 1/gov-1", admitted.Revision, admitted.AdmittedKeyID)
	}
}

// The governance key signs the descriptor and nothing else. If it reached the
// registry, the signature validator -- which checks only that a key is published
// and active -- would accept it on an ordinary call.
func TestAdmitKeepsTheGovernanceKeyOutOfTheRegistry(t *testing.T) {
	descriptor, keys := admittable(t, 1)

	admitted, err := Admit(nil, descriptor, keys)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	for _, key := range admitted.Keys {
		if key.Kid == "gov-1" {
			t.Fatal("the governance key was written to the registry, where it could sign ordinary traffic")
		}
	}
	if len(admitted.Keys) != 1 || admitted.Keys[0].Kid != "op-1" {
		t.Fatalf("keys = %+v, want the operational key only", admitted.Keys)
	}
}

// A peer publishing only a governance key is admitted with nothing to verify its
// calls, so every call it makes fails. That is correct: it has not published an
// operational key, so there is no way to talk to it yet.
func TestAdmitLeavesAGovernanceOnlyPeerWithNoUsableKey(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	signed, _ := Sign(descriptorAt(1), "gov-1", private)

	admitted, err := Admit(nil, signed, keysetOf(t, "gov-1", public))
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(admitted.Keys) != 0 {
		t.Fatalf("keys = %+v, want none — the only published key signs the descriptor", admitted.Keys)
	}
}

func TestAdmitRefusesAnUnverifiableDescriptor(t *testing.T) {
	descriptor, _ := admittable(t, 1)
	otherPublic, _, _ := ed25519.GenerateKey(nil)

	if _, err := Admit(nil, descriptor, keysetOf(t, "gov-1", otherPublic)); err == nil {
		t.Fatal("a descriptor that does not verify was admitted")
	}
}

// A refresh must advance the revision. Without this, a descriptor captured
// before a suspension or a key change could be replayed as current.
func TestRefreshRefusesAStaleRevision(t *testing.T) {
	peer := newNetwork(t)
	current, err := Admit(nil, peer.issues(t, 5), peer.keys)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}

	// Properly signed, genuinely from this peer, and still refused: the only
	// thing wrong with them is that they do not move forward.
	for _, revision := range []int{4, 5} {
		_, err := Admit(&current, peer.issues(t, revision), peer.keys)
		if !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("revision %d: err = %v, want ErrStaleRevision", revision, err)
		}
	}
}

// A peer may rotate the keys it publishes, but may not change which key speaks
// for the network. Otherwise anyone who obtained one signing key could hand the
// identity to another.
func TestRefreshRefusesADifferentSigningKey(t *testing.T) {
	peer := newNetwork(t)
	current, _ := Admit(nil, peer.issues(t, 1), peer.keys)

	newPublic, newPrivate, _ := ed25519.GenerateKey(nil)
	resigned, _ := Sign(descriptorAt(2), "gov-2", newPrivate)

	_, err := Admit(&current, resigned, keysetOf(t, "gov-2", newPublic))
	if !errors.Is(err, ErrKeyChanged) {
		t.Fatalf("err = %v, want ErrKeyChanged", err)
	}
}

// A descriptor for another network must not overwrite this peer's record.
func TestRefreshRefusesADescriptorForAnotherNetwork(t *testing.T) {
	peer := newNetwork(t)
	current, _ := Admit(nil, peer.issues(t, 1), peer.keys)

	// Correctly signed by the same key, for a different network.
	other := descriptorAt(2)
	other.NetworkID = "someone-else.oan.local"
	signed, err := Sign(other, "gov-1", peer.private)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, err := Admit(&current, signed, peer.keys); !errors.Is(err, ErrWrongNetwork) {
		t.Fatalf("err = %v, want ErrWrongNetwork", err)
	}
}

// The rule that makes suspension mean something: a suspended peer must not
// reinstate itself by publishing a new descriptor.
func TestRefreshCannotLiftASuspension(t *testing.T) {
	peer := newNetwork(t)
	current, _ := Admit(nil, peer.issues(t, 1), peer.keys)
	current = Suspend(current)

	// A fresh, valid, higher-revision descriptor from the admitted key.
	refreshed, err := Admit(&current, peer.issues(t, 2), peer.keys)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if refreshed.Status != StatusInactive {
		t.Fatal("a suspended peer lifted its own suspension by publishing a new descriptor")
	}
}

func TestSuspendMarksThePeerInactive(t *testing.T) {
	descriptor, keys := admittable(t, 1)
	admitted, _ := Admit(nil, descriptor, keys)

	if got := Suspend(admitted); got.Status != StatusInactive {
		t.Fatalf("status = %q, want %q", got.Status, StatusInactive)
	}
}

func TestAdmitRefusesAnEmptyNetworkID(t *testing.T) {
	descriptor, keys := admittable(t, 1)
	descriptor.NetworkID = ""

	if _, err := Admit(nil, descriptor, keys); err != ErrMissingNetwork {
		t.Fatalf("err = %v, want ErrMissingNetwork", err)
	}
}
