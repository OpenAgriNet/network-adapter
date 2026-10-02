package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/federation"
)

// governanceKeyID is the kid the descriptor's signing key is published under.
//
// Fixed rather than configurable because a network has exactly one governance
// key at a time and admission pins the kid a peer was admitted under -- a value
// an operator could vary per run would be a value that silently breaks every
// peer's refresh.
const governanceKeyID = "gov-1"

type publishFlags struct {
	networkID    string
	name         string
	operator     string
	revision     int
	discoveryURL string
	schemaPacks  string
	profile      string
	status       string
	out          string
	govKey       string
}

func publish(args []string) error {
	var f publishFlags
	set := flag.NewFlagSet("publish", flag.ExitOnError)
	set.StringVar(&f.networkID, "network-id", "", "this network's DNS domain, which is its identity")
	set.StringVar(&f.name, "name", "", "human-readable network name")
	set.StringVar(&f.operator, "operator", "", "organisation answering for this network")
	set.IntVar(&f.revision, "revision", 0, "descriptor revision; must increase on every republish")
	set.StringVar(&f.discoveryURL, "discovery-url", "", "complete federated discovery endpoint, used verbatim")
	set.StringVar(&f.schemaPacks, "schema-packs", "", "comma-separated published schema pack URLs")
	set.StringVar(&f.profile, "operating-profile", "OAN/v0.1,Beckn/v2", "comma-separated version stack")
	set.StringVar(&f.status, "status", "Active", "Active, Suspended or Retired")
	set.StringVar(&f.out, "out", "", "directory to write .well-known/ into")
	set.StringVar(&f.govKey, "gov-key", "", "governance private key file; generated if absent")
	if err := set.Parse(args); err != nil {
		return err
	}
	if err := f.validate(); err != nil {
		return err
	}

	private, err := loadOrCreateKey(f.govKey)
	if err != nil {
		return err
	}

	descriptor, err := federation.Sign(f.descriptor(), governanceKeyID, private)
	if err != nil {
		return err
	}
	keys := federation.KeySet{Keys: []federation.JWK{{
		Kty: "OKP", Crv: "Ed25519", Kid: governanceKeyID,
		X: federation.EncodeKey(private.Public().(ed25519.PublicKey)),
	}}}

	// Verified before it is written, not after. A descriptor that does not
	// verify against its own key is a bug here, and publishing it would move
	// the symptom to a peer that cannot do anything about it.
	if err := federation.Verify(descriptor, keys); err != nil {
		return fmt.Errorf("the descriptor just signed does not verify: %w", err)
	}

	return writeWellKnown(f.out, descriptor, keys)
}

func (f publishFlags) validate() error {
	required := map[string]string{
		"-network-id":    f.networkID,
		"-name":          f.name,
		"-operator":      f.operator,
		"-discovery-url": f.discoveryURL,
		"-out":           f.out,
	}
	for flagName, value := range required {
		if value == "" {
			return fmt.Errorf("%s is required", flagName)
		}
	}
	// Revisions start at 1. Zero is what an unset flag looks like, and a
	// descriptor published at revision 0 can never be superseded downwards.
	if f.revision < 1 {
		return fmt.Errorf("-revision must be 1 or more, got %d", f.revision)
	}
	return nil
}

func (f publishFlags) descriptor() federation.Descriptor {
	return federation.Descriptor{
		Version:   "1.0",
		NetworkID: f.networkID,
		Name:      f.name,
		Operator:  federation.Operator{Name: f.operator},
		Revision:  f.revision,
		Status:    f.status,
		IssuedAt:  time.Now().UTC().Format(time.RFC3339),

		OperatingProfile: splitList(f.profile),
		SchemaPacks:      splitList(f.schemaPacks),

		// Used verbatim. The shape of this URL is the publishing network's to
		// decide, so nothing is appended to it here.
		Services: federation.Services{Discovery: f.discoveryURL},
	}
}

// splitList turns a comma-separated flag into a slice, dropping empties so a
// trailing comma does not become a blank entry in a published document.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// loadOrCreateKey reads the governance private key, generating one the first
// time.
//
// Generated rather than demanded so a network can be stood up in one command,
// and written 0600 because it is the key that speaks for the whole network.
func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, fmt.Errorf("-gov-key is required")
	}
	switch encoded, err := os.ReadFile(path); {
	case err == nil:
		raw, decodeErr := base64.RawURLEncoding.DecodeString(string(encoded))
		if decodeErr != nil {
			return nil, fmt.Errorf("reading %s: %w", path, decodeErr)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%s holds %d bytes, want an Ed25519 private key of %d",
				path, len(raw), ed25519.PrivateKeySize)
		}
		return ed25519.PrivateKey(raw), nil
	case os.IsNotExist(err):
		return createKey(path)
	default:
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
}

func createKey(path string) (ed25519.PrivateKey, error) {
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("generating governance key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(private)
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "oanfed: generated a governance key at %s -- it is not recoverable, and a peer that admitted this network pinned its kid\n", path)
	return private, nil
}

// writeWellKnown writes the two documents a peer fetches.
//
// The descriptor has no file extension because the contract fixes the path as
// /.well-known/openagrinet, not openagrinet.json.
func writeWellKnown(dir string, descriptor federation.Descriptor, keys federation.KeySet) error {
	wellKnown := filepath.Join(dir, ".well-known")
	if err := os.MkdirAll(wellKnown, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", wellKnown, err)
	}
	for name, document := range map[string]any{
		"openagrinet": descriptor,
		"jwks.json":   keys,
	} {
		encoded, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding %s: %w", name, err)
		}
		path := filepath.Join(wellKnown, name)
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}
