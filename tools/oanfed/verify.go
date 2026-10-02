package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// verify fetches a network's two published documents and checks them.
//
// It changes nothing. A descriptor that verifies is a CANDIDATE -- both files
// came from the same host, so this proves self-consistency and not that the
// network should be dealt with. Admission is the separate decision.
func verify(args []string) error {
	var (
		access    peerAccess
		networkID string
		show      bool
	)
	set := flag.NewFlagSet("verify", flag.ExitOnError)
	access.bind(set)
	set.StringVar(&networkID, "network-id", "", "the network to fetch, which is its domain")
	set.BoolVar(&show, "show", false, "print the descriptor that verified")
	if err := set.Parse(args); err != nil {
		return err
	}
	if networkID == "" {
		return fmt.Errorf("-network-id is required")
	}

	fetcher, err := access.fetcher(networkID)
	if err != nil {
		return err
	}
	descriptor, keys, err := fetcher.Fetch(context.Background(), networkID)
	if err != nil {
		return err
	}

	fmt.Printf("%s verifies\n", descriptor.NetworkID)
	fmt.Printf("  name       %s (%s)\n", descriptor.Name, descriptor.Operator.Name)
	fmt.Printf("  status     %s at revision %d\n", descriptor.Status, descriptor.Revision)
	fmt.Printf("  signed by  %s, %s\n", descriptor.Signature.KeyID, descriptor.Signature.Algorithm)
	fmt.Printf("  discovery  %s\n", descriptor.Services.Discovery)
	fmt.Printf("  profile    %v\n", descriptor.OperatingProfile)
	fmt.Printf("  keys       %d published\n", len(keys.Keys))

	if show {
		encoded, err := json.MarshalIndent(descriptor, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "\n%s\n", encoded)
	}

	fmt.Println("\nThis is a candidate, not a peer: the signature and the key came from the")
	fmt.Println("same host, so it proves only that the holder of a published key wrote it.")
	return nil
}
