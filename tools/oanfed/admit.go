package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/federation"
)

// registryFlags are the connection details both admit and suspend need.
type registryFlags struct {
	registryURL    string
	keycloakURL    string
	keycloakIssuer string
	realm          string
	clientID       string
	username       string
	password       string
	timeout        time.Duration
}

func (r *registryFlags) bind(set *flag.FlagSet) {
	set.StringVar(&r.registryURL, "registry-url", "", "this network's own registry, e.g. http://localhost:10081")
	set.StringVar(&r.keycloakURL, "keycloak-url", "", "the registry's Keycloak, e.g. http://localhost:10080")
	set.StringVar(&r.keycloakIssuer, "keycloak-issuer", "sunbird-registry-keycloak:8080",
		"the container-internal address the registry validates a token's issuer against")
	set.StringVar(&r.realm, "realm", "sunbird-rc", "Keycloak realm")
	set.StringVar(&r.clientID, "client-id", "registry-frontend", "Keycloak client id")
	set.StringVar(&r.username, "username", "", "registry admin user")
	set.StringVar(&r.password, "password", "", "registry admin password")
	// Named apart from peerAccess's -timeout: one bounds calls to OUR OWN
	// registry, the other calls to a peer that has not been admitted yet. They
	// are different risks and a single knob would tie them together.
	set.DurationVar(&r.timeout, "registry-timeout", 30*time.Second, "per-request timeout for our own registry")
}

func (r registryFlags) client() (*registryClient, error) {
	return newRegistryClient(registryOptions{
		registryURL: r.registryURL, keycloakURL: r.keycloakURL,
		keycloakIssuer: r.keycloakIssuer, realm: r.realm, clientID: r.clientID,
		username: r.username, password: r.password, timeout: r.timeout,
	})
}

// admit fetches a candidate network's documents and records the decision to deal
// with it.
//
// Run it again and it is also the refresh job: Admit already refuses anything
// that does not advance the revision, so a repeat against an unchanged peer is a
// no-op, and a repeat against a replayed descriptor is a refusal.
//
// A fetch that fails leaves the stored record untouched. A network being briefly
// unreachable is not a reason to stop trusting it -- suspension is a decision,
// not a timeout.
func admit(args []string) error {
	var (
		registry  registryFlags
		access    peerAccess
		networkID string
	)
	set := flag.NewFlagSet("admit", flag.ExitOnError)
	registry.bind(set)
	access.bind(set)
	set.StringVar(&networkID, "network-id", "", "the peer to admit, which is its domain")
	if err := set.Parse(args); err != nil {
		return err
	}
	if networkID == "" {
		return fmt.Errorf("-network-id is required")
	}

	client, err := registry.client()
	if err != nil {
		return err
	}
	ctx := context.Background()

	fetcher, err := access.fetcher(networkID)
	if err != nil {
		return err
	}
	descriptor, keys, err := fetcher.Fetch(ctx, networkID)
	if err != nil {
		return err
	}

	current, err := storedPeer(ctx, client, networkID)
	if err != nil {
		return err
	}

	admitted, err := federation.Admit(current, descriptor, keys)
	switch {
	case errors.Is(err, federation.ErrStaleRevision):
		// Not a failure. A refresh that finds nothing new is the normal case
		// when this runs on a schedule.
		fmt.Printf("%s is unchanged at revision %d; nothing written\n", networkID, current.Revision)
		return nil
	case err != nil:
		return err
	}

	record, err := recordFor(admitted)
	if err != nil {
		return err
	}
	if err := client.save(ctx, record); err != nil {
		return err
	}

	verb := "admitted"
	if current != nil {
		verb = "refreshed"
	}
	fmt.Printf("%s %s at revision %d, status %s\n",
		verb, admitted.NetworkID, admitted.Revision, admitted.Status)
	fmt.Printf("  discovery  %s\n", admitted.DiscoveryURL)
	fmt.Printf("  keys       %d operational (the governance key is deliberately not stored)\n", len(admitted.Keys))
	return nil
}

// storedPeer reads what the registry already holds for a network, as the shape
// Admit compares against.
func storedPeer(ctx context.Context, client *registryClient, networkID string) (*federation.Participant, error) {
	record, err := client.find(ctx, networkID)
	if err != nil || record == nil {
		return nil, err
	}
	return &federation.Participant{
		NetworkID:     record.ParticipantID,
		Name:          record.Name,
		DiscoveryURL:  record.BaseURL,
		Status:        record.Status,
		Revision:      record.Revision,
	}, nil
}

// reinstate undoes a suspension.
//
// A separate command and an explicit act, for the same reason Admit never sets
// status: lifting a suspension is a decision, and the one thing that must not be
// able to make it is the suspended peer. Re-running admit will not do it --
// Admit carries status over on purpose.
func reinstate(args []string) error {
	return setStatus(args, "reinstate", federation.StatusActive,
		"reinstated %s; it will be crawled again and its calls will verify\n")
}

// suspend stops dealing with a peer.
//
// It fetches nothing, and that is the point: this is a decision about a network
// we have stopped trusting, and asking that network's own server to take part in
// it would be absurd.
//
// A separate command rather than a flag on admit, because Admit deliberately
// carries status over and never sets it -- letting `admit --suspend` reach in
// would put the one decision Admit refuses to make back inside it.
func suspend(args []string) error {
	return setStatus(args, "suspend", federation.StatusInactive,
		"suspended %s; it will not be crawled, and its calls will not verify\n")
}

// setStatus is the whole of both commands: read the record, change one field,
// write it back. They are two names rather than one with a flag because they are
// two decisions, and a single command taking a status would read as though the
// status were data rather than a choice.
func setStatus(args []string, name, status, message string) error {
	var (
		registry  registryFlags
		networkID string
	)
	set := flag.NewFlagSet(name, flag.ExitOnError)
	registry.bind(set)
	set.StringVar(&networkID, "network-id", "", "the peer to suspend")
	if err := set.Parse(args); err != nil {
		return err
	}
	if networkID == "" {
		return fmt.Errorf("-network-id is required")
	}

	client, err := registry.client()
	if err != nil {
		return err
	}
	ctx := context.Background()

	current, err := storedPeer(ctx, client, networkID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("%s is not an admitted peer", networkID)
	}

	// The stored keys are read back and rewritten unchanged. Suspension is
	// about status; dropping the keys would make it unrecoverable without a
	// fresh fetch from a network we have just decided to stop talking to.
	record, err := client.find(ctx, networkID)
	if err != nil {
		return err
	}
	record.Status = status

	if err := client.save(ctx, *record); err != nil {
		return err
	}
	fmt.Printf(message, networkID)
	return nil
}
