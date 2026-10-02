// Command oanfed is the operator's tool for federation.
//
// Admitting a peer is a DECISION, not a request, so it is a command an operator
// runs and not an endpoint the adapter exposes. An adapter endpoint would mean
// anything able to reach the adapter could widen who this network deals with,
// which is exactly the thing admission exists to control.
//
//	oanfed publish   generate and sign this network's two published documents
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	commands := map[string]func([]string) error{
		"publish": publish,
		"verify":  verify,
		"admit":   admit,
		"suspend": suspend,
	}
	run, known := commands[os.Args[1]]
	if !known {
		fmt.Fprintf(os.Stderr, "oanfed: unknown command %q\n", os.Args[1])
		usage()
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "oanfed %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: oanfed <command> [flags]

  publish   generate and sign this network's .well-known documents
  verify    fetch another network's documents and check them
  admit     record the decision to deal with a peer; also the refresh job
  suspend   stop dealing with a peer

Run a command with -h for its flags.
`)
	os.Exit(2)
}
