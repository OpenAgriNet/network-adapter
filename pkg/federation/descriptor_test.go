package federation

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func keysetOf(t *testing.T, kid string, public ed25519.PublicKey) KeySet {
	t.Helper()
	return KeySet{Keys: []JWK{{Kty: "OKP", Crv: "Ed25519", Kid: kid, X: EncodeKey(public)}}}
}

// descriptorAt builds a descriptor at a revision, with every field a real one
// carries, so Canonical is exercised over the whole shape rather than a stub.
func descriptorAt(revision int) Descriptor {
	return Descriptor{
		Version:          "1.0",
		NetworkID:        "bharatvistar.oan.local",
		Name:             "Bharat Vistar",
		Operator:         Operator{Name: "Bharat Vistar Operator"},
		Revision:         revision,
		Status:           "Active",
		IssuedAt:         "2026-10-02T00:00:00Z",
		OperatingProfile: []string{"OAN/v0.1", "Beckn/v2"},
		SchemaPacks:      []string{"https://example.org/schema/MandiPrice/v0.1"},
		Services:         Services{Discovery: "https://bharatvistar.oan.local/federation/discovery"},
	}
}

// The signature cannot cover itself, so the bytes signed must be the same before
// and after it is attached. If they differ, nothing ever verifies.
func TestCanonicalIgnoresTheSignature(t *testing.T) {
	unsigned := descriptorAt(1)
	signed := unsigned
	signed.Signature = &Signature{KeyID: "gov-1", Algorithm: AlgorithmEd25519, Value: "zzz"}

	before, err := Canonical(unsigned)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	after, err := Canonical(signed)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	if string(before) != string(after) {
		t.Fatalf("attaching a signature changed the bytes that get signed:\n%s\n%s", before, after)
	}
}

// Everything a peer decides from must be covered. These are the fields worth
// tampering with -- lifting a suspension, replaying an old revision, pointing
// discovery somewhere else -- so each is checked individually rather than
// trusting that "the whole struct" is enough.
func TestCanonicalCoversTheFieldsWorthTampering(t *testing.T) {
	base := descriptorAt(1)
	original, err := Canonical(base)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	for name, mutate := range map[string]func(*Descriptor){
		"status":    func(d *Descriptor) { d.Status = "Suspended" },
		"revision":  func(d *Descriptor) { d.Revision = 99 },
		"discovery": func(d *Descriptor) { d.Services.Discovery = "https://elsewhere.example/x" },
		"networkId": func(d *Descriptor) { d.NetworkID = "someone-else.oan.local" },
		"profile":   func(d *Descriptor) { d.OperatingProfile = []string{"OAN/v9"} },
		"packs":     func(d *Descriptor) { d.SchemaPacks = nil },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := base
			mutate(&tampered)
			got, err := Canonical(tampered)
			if err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if string(got) == string(original) {
				t.Fatalf("%s is not covered by the signature", name)
			}
		})
	}
}

func TestSignedDescriptorVerifies(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)

	signed, err := Sign(descriptorAt(1), "gov-1", private)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(signed, keysetOf(t, "gov-1", public)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// The whole point of signing it. A document changed after signing must not pass.
func TestTamperedDescriptorIsRefused(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)

	signed, _ := Sign(descriptorAt(1), "gov-1", private)
	signed.Status = "Active" // was already Active; now change something that matters
	signed.Services.Discovery = "https://attacker.example/discovery"

	if err := Verify(signed, keysetOf(t, "gov-1", public)); err == nil {
		t.Fatal("a descriptor whose discovery URL was changed after signing verified")
	}
}

// A signature from a key the network never published is not a signature. This is
// the check that stops anyone with any key signing on a network's behalf.
func TestSignatureFromAnUnpublishedKeyIsRefused(t *testing.T) {
	_, attackerPrivate, _ := ed25519.GenerateKey(nil)
	networkPublic, _, _ := ed25519.GenerateKey(nil)

	signed, _ := Sign(descriptorAt(1), "gov-1", attackerPrivate)

	err := Verify(signed, keysetOf(t, "gov-1", networkPublic))
	if err == nil {
		t.Fatal("a descriptor signed by an unpublished key verified")
	}
	if err != ErrBadSignature {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// A keyId naming a key that is not in the JWKS is a different fault from a
// signature that does not verify, and a peer reporting it should say which.
func TestUnknownKeyIDIsReportedAsSuch(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(nil)
	otherPublic, _, _ := ed25519.GenerateKey(nil)

	signed, _ := Sign(descriptorAt(1), "gov-1", private)

	err := Verify(signed, keysetOf(t, "some-other-kid", otherPublic))
	if err == nil || !strings.Contains(err.Error(), "gov-1") {
		t.Fatalf("err = %v, want it to name the missing keyId", err)
	}
}

func TestUnsignedDescriptorIsRefused(t *testing.T) {
	if err := Verify(descriptorAt(1), KeySet{}); err != ErrNotSigned {
		t.Fatalf("err = %v, want ErrNotSigned", err)
	}
}

// Narrowed deliberately: the contract leaves the algorithm open, and anything
// we do not implement must be refused rather than assumed to be Ed25519.
func TestUnsupportedAlgorithmIsRefused(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)

	signed, _ := Sign(descriptorAt(1), "gov-1", private)
	signed.Signature.Algorithm = "RS256"

	err := Verify(signed, keysetOf(t, "gov-1", public))
	if err == nil || !strings.Contains(err.Error(), "RS256") {
		t.Fatalf("err = %v, want it to name the unsupported algorithm", err)
	}
}

func TestSigningWithoutANetworkIDIsRefused(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(nil)

	d := descriptorAt(1)
	d.NetworkID = ""

	if _, err := Sign(d, "gov-1", private); err != ErrMissingNetwork {
		t.Fatalf("err = %v, want ErrMissingNetwork", err)
	}
}

func TestKeyRoundTrips(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(nil)

	decoded, err := DecodeKey(EncodeKey(public))
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	if !decoded.Equal(public) {
		t.Fatal("a key did not survive the round trip through its published form")
	}
}

// Padded base64 is the likely mistake, and it must fail loudly rather than
// yield a key of the wrong length that fails every verification later.
func TestDecodeKeyRefusesTheWrongLength(t *testing.T) {
	if _, err := DecodeKey("c2hvcnQ"); err == nil {
		t.Fatal("a key of the wrong length decoded")
	}
}
