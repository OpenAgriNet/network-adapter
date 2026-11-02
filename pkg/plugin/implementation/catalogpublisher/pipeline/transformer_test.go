package pipeline

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestServeMappingsServesEmbeddedFiles(t *testing.T) {
	base, stop, err := ServeMappings(fixtureFS, "testdata/mappings")
	if err != nil {
		t.Fatalf("ServeMappings: %v", err)
	}
	defer stop()

	resp, err := http.Get(base + "/catalog.yaml")
	if err != nil {
		t.Fatalf("GET %s/catalog.yaml: %v", base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "response:") {
		t.Errorf("body = %q, want the fixture's catalog mapping", body)
	}
}

func TestServeMappingsStopClosesTheListener(t *testing.T) {
	base, stop, err := ServeMappings(fixtureFS, "testdata/mappings")
	if err != nil {
		t.Fatalf("ServeMappings: %v", err)
	}
	stop()

	if _, err := http.Get(base + "/catalog.yaml"); err == nil {
		t.Error("GET after stop() succeeded, want a connection error")
	}
}

func TestNewMapperBuildsAWorkingMapper(t *testing.T) {
	mapper, closer, err := NewMapper(context.Background())
	if err != nil {
		t.Fatalf("NewMapper: %v", err)
	}
	if mapper == nil {
		t.Fatal("NewMapper returned a nil mapper")
	}
	if err := closer(); err != nil {
		t.Errorf("closer(): %v", err)
	}
}
