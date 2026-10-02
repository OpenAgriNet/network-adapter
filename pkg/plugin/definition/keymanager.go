package definition

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// KeyManager defines the interface for key management operations/methods.
type KeyManager interface {
	GenerateKeyset() (*model.Keyset, error)
	InsertKeyset(ctx context.Context, keyID string, keyset *model.Keyset) error
	Keyset(ctx context.Context, keyID string) (*model.Keyset, error)
	LookupNPKeys(ctx context.Context, subscriberID, uniqueKeyID string) (signingPublicKey string, encrPublicKey string, err error)
	DeleteKeyset(ctx context.Context, keyID string) error
}

// KeyManagerProvider initializes a new signer instance.
type KeyManagerProvider interface {
	New(context.Context, RegistryLookup, map[string]string) (KeyManager, func() error, error)
}

// PeerNetworkLookup is an OPTIONAL extension of KeyManager: it reports whether a
// subscriber is a peer network this deployment has admitted.
//
// Separate from KeyManager because not every key manager can answer it -- one
// backed by a static key file knows keys and nothing else -- and because adding
// it to KeyManager would break every existing implementation. Obtain it by
// type-asserting a KeyManager, the same way RegistryMetadataLookup is obtained
// from a RegistryLookup; an implementation that does not offer it yields nil,
// and the caller decides what "cannot tell" means.
//
// The question is deliberately "did we admit this as a peer", not "is its role
// network". A deployment's own network-layer adapter has that role, and
// answering on role would make this network a foreign peer to itself.
type PeerNetworkLookup interface {
	// IsAdmittedPeer reports whether the subscriber is an admitted peer
	// network. A false with a nil error means "no"; an error means the question
	// could not be answered, which a caller must not read as either.
	IsAdmittedPeer(ctx context.Context, subscriberID, uniqueKeyID string) (bool, error)
}
