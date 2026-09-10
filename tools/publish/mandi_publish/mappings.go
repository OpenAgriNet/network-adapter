package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// mappingFiles carries this tool's mappings into the binary, so a run needs no
// working directory and a released tool cannot be pointed at an edited mapping
// by accident.
//
//go:embed mappings
var mappingFiles embed.FS

// serveMappings publishes the embedded mappings on a loopback listener and
// returns the base URL to reference them by.
//
// A SERVER RATHER THAN A FILE PATH, and not a workaround to route around.
// jsonmapper.verifyFetchable accepts only http and https, because a mapping
// reference reaches the adapter from the registry and a file scheme there would
// let a registry record make the adapter read local files. That check protects
// the adapter, this tool has no business weakening it, and a loopback server
// satisfies it honestly.
//
// Port 0: the OS picks a free port, so two runs on one machine cannot collide.
func serveMappings() (string, func(), error) {
	sub, err := fs.Sub(mappingFiles, "mappings")
	if err != nil {
		return "", nil, fmt.Errorf("mandi_publish: embedded mappings: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("mandi_publish: mapping server: %w", err)
	}

	server := &http.Server{Handler: http.FileServer(http.FS(sub))}
	go func() { _ = server.Serve(listener) }()

	stop := func() { _ = server.Close() }
	return "http://" + listener.Addr().String(), stop, nil
}

// newMapper builds the JSONata mapper.
//
// Cache settings are left at their defaults deliberately: the references are
// loopback and each is fetched once per run, so nothing here is worth tuning.
func newMapper(ctx context.Context) (*jsonmapper.Mapper, func() error, error) {
	return jsonmapper.New(ctx, &jsonmapper.Config{})
}
