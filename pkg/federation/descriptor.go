// Package federation implements the two documents a network publishes so other
// networks can find it, check it, and decide whether to deal with it.
//
// A network publishes exactly two files, at locations derived from its own
// domain:
//
//	https://{networkId}/.well-known/openagrinet   the signed Network Descriptor
//	https://{networkId}/.well-known/jwks.json     the keys that verify it
//
// These are publication points, not APIs. Serving a static file and generating
// the same JSON are equally valid, and nothing here assumes which.
//
// Fetching and verifying a descriptor yields a CANDIDATE, never trust. The
// signature proves only that whoever holds the key wrote it, and the key comes
// from the same domain -- so the cryptography alone is circular. What breaks the
// circle is an operator deciding to admit the network, which is a separate act
// recorded locally.
package federation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Descriptor is the signed declaration a network publishes about itself.
//
// Field for field the architecture contract. Everything here is public: it
// names the network, says what it is compatible with, and says where its
// federated discovery endpoint is. It does NOT describe internal services,
// providers, resources, the capability matrix, or who has been admitted.
type Descriptor struct {
	Version   string   `json:"version"`
	NetworkID string   `json:"networkId"`
	Name      string   `json:"name"`
	Operator  Operator `json:"operator"`

	// Revision only ever increases. A peer accepts a replacement descriptor
	// only when this is higher than the one it already holds, which is what
	// stops an old document being replayed as if it were current.
	Revision int `json:"revision"`

	// Active, Suspended or Retired.
	Status string `json:"status"`

	// When this revision was issued. Informational: it is not a key expiry and
	// not a cache expiry, and nothing decides anything from it.
	IssuedAt string `json:"issuedAt"`

	// The required OAN version and the Beckn version under it, as one ordered
	// stack. A peer that cannot satisfy the whole stack is incompatible.
	OperatingProfile []string `json:"operatingProfile"`

	// The complete published URLs of the schema packs this network supports.
	SchemaPacks []string `json:"schemaPacks"`

	Services Services `json:"services"`

	// Role-based contacts. Optional, and never personal.
	Contacts map[string]string `json:"contacts,omitempty"`

	// Omitted while the document is being built, present once signed. It is a
	// pointer so that Canonical can tell "not yet signed" from "signed with an
	// empty signature" -- the second would be a document to refuse, not one to
	// sign again.
	Signature *Signature `json:"signature,omitempty"`
}

// Operator names the organisation answering for the network.
type Operator struct {
	Name string `json:"name"`

	// Carried only where an ecosystem already has an accepted identifier
	// authority. OAN does not issue one, so this is usually absent.
	OperatorID string `json:"operatorId,omitempty"`
}

// Services is where a peer sends its federated discovery query.
//
// Discovery ONLY. Every later action goes to the provider endpoint that
// discovery returned, not here -- a peer routing a /select through this value
// would be bypassing the routing details it was just given.
type Services struct {
	Discovery string `json:"discovery"`
}

// Signature is the detached signature over the descriptor's canonical bytes.
type Signature struct {
	// KeyID matches the kid of one key in the network's published JWKS.
	KeyID     string `json:"keyId"`
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// AlgorithmEd25519 is the only algorithm this implementation signs or verifies
// with. The contract leaves the choice open; narrowing it here means an
// unexpected value is refused rather than quietly treated as this one.
const AlgorithmEd25519 = "Ed25519"

// KeySet is the JWKS published beside the descriptor.
type KeySet struct {
	Keys []JWK `json:"keys"`
}

// JWK is one published public key, in the subset of the JWK form an Ed25519
// verification key needs.
type JWK struct {
	Kty string `json:"kty"` // "OKP"
	Crv string `json:"crv"` // "Ed25519"
	Kid string `json:"kid"`
	X   string `json:"x"` // base64url, unpadded
}

// Errors a caller is expected to tell apart.
var (
	ErrNotSigned      = errors.New("descriptor carries no signature")
	ErrUnknownKey     = errors.New("no published key matches the signature's keyId")
	ErrBadSignature   = errors.New("signature does not verify")
	ErrBadAlgorithm   = errors.New("unsupported signature algorithm")
	ErrMissingNetwork = errors.New("descriptor carries no networkId")
)

// Canonical returns the exact bytes that get signed.
//
// The signature is excluded, because it cannot cover itself. Everything else is
// included: narrowing the covered set would let an unsigned field be changed in
// flight, and the fields most worth tampering with -- status, revision, the
// discovery URL -- are exactly the ones a smaller set would be tempted to omit.
//
// LIMITATION, and it must be settled before another team builds a network:
// this is Go's encoder, which is deterministic for us but is not an agreed
// canonical form. A second implementation would order or escape something
// differently, rebuild different bytes, and fail every signature check on a
// document nobody tampered with. The fix is a published canonicalisation (JCS,
// RFC 8785, is the obvious candidate) chosen before the first signature anyone
// outside this project has to verify, because changing it later invalidates
// everything already published.
func Canonical(d Descriptor) ([]byte, error) {
	d.Signature = nil
	encoded, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("federation: canonicalising descriptor: %w", err)
	}
	return encoded, nil
}

// EncodeKey renders a public key the way the JWKS carries it.
func EncodeKey(key ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

// RegistryKey renders a JWKS key in the form the registry stores.
//
// A JWKS carries base64url WITHOUT padding, because RFC 7517 says so. The
// registry's PublicKey schema pins ^[A-Za-z0-9+/]{43}=$, which is standard
// base64 WITH padding. Both are correct for their own format, so something has
// to convert -- and it is one function rather than an inline re-encode at each
// call site, because getting it wrong produces a key that is silently the wrong
// bytes rather than an error.
func RegistryKey(jwkX string) (string, error) {
	raw, err := DecodeKey(jwkX)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeKey reads a public key back out of its JWKS form.
func DecodeKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("federation: decoding key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("federation: key is %d bytes, want %d",
			len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// Sign returns a copy of the descriptor carrying its signature.
//
// The governance key signs this and nothing else. The contract forbids it from
// signing ordinary traffic, which is why admission deliberately keeps it out of
// the registry record a peer's calls are verified against.
func Sign(d Descriptor, keyID string, private ed25519.PrivateKey) (Descriptor, error) {
	if d.NetworkID == "" {
		return Descriptor{}, ErrMissingNetwork
	}
	message, err := Canonical(d)
	if err != nil {
		return Descriptor{}, err
	}
	d.Signature = &Signature{
		KeyID:     keyID,
		Algorithm: AlgorithmEd25519,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, message)),
	}
	return d, nil
}

// Verify checks a descriptor against the keys its network published.
//
// It answers one question -- did the holder of a published key write this
// document -- and deliberately not whether the network should be trusted. The
// keys came from the same domain as the document, so a network can always
// verify its own lies. Admission is what decides trust.
func Verify(d Descriptor, keys KeySet) error {
	if d.Signature == nil {
		return ErrNotSigned
	}
	if d.Signature.Algorithm != AlgorithmEd25519 {
		return fmt.Errorf("%w: %q", ErrBadAlgorithm, d.Signature.Algorithm)
	}

	jwk, found := keys.find(d.Signature.KeyID)
	if !found {
		return fmt.Errorf("%w: %q", ErrUnknownKey, d.Signature.KeyID)
	}
	public, err := DecodeKey(jwk.X)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(d.Signature.Value)
	if err != nil {
		return fmt.Errorf("federation: decoding signature: %w", err)
	}

	message, err := Canonical(d)
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, message, signature) {
		return ErrBadSignature
	}
	return nil
}

// find returns the published key with this kid.
func (k KeySet) find(kid string) (JWK, bool) {
	for _, key := range k.Keys {
		if key.Kid == kid {
			return key, true
		}
	}
	return JWK{}, false
}
