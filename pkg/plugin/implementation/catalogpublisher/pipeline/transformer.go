package pipeline

// transformer.go serves a pipeline's mappings and builds the JSONata
// mapper that reads them -- the two things every run needs before its first
// step.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/jsonmapper"
)

// ServeMappings publishes a directory of pipeline files on a loopback listener
// and returns the base URL to reference them by. Production fetches pipelines
// from their hosted URL; this is for tests and local runs, which serve the
// same files the same way.
//
// A SERVER RATHER THAN A FILE PATH, and not a workaround to route around.
// jsonmapper.verifyFetchable accepts only http and https, because a mapping
// reference reaches the adapter from the registry and a file scheme there
// would let a registry record make the adapter read local files. That check
// protects the adapter; a loopback server satisfies it honestly.
//
// Port 0: the OS picks a free port, so two runs on one machine cannot
// collide.
func ServeMappings(files fs.FS, dir string) (string, func(), error) {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		return "", nil, fmt.Errorf("pipeline: embedded mappings: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("pipeline: mapping server: %w", err)
	}

	server := &http.Server{Handler: http.FileServer(http.FS(sub))}
	go func() {
		// ErrServerClosed is the normal stop. Anything else means the
		// mappings stopped being served mid-run, and every later transform
		// will fail far from here -- so the cause is said where it is known.
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("publish pipeline: mapping server stopped", "error", err)
		}
	}()

	stop := func() { _ = server.Close() }
	return "http://" + listener.Addr().String(), stop, nil
}

// NewMapper builds the JSONata mapper.
//
// Cache settings are left at their defaults deliberately: references are
// loopback and each is fetched once per run, so nothing here is worth
// tuning.
func NewMapper(ctx context.Context) (*jsonmapper.Mapper, func() error, error) {
	return jsonmapper.New(ctx, &jsonmapper.Config{})
}
