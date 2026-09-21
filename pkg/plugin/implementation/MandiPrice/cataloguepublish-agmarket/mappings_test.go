package agmarket

// mappings_test.go proves Files actually carries what a deployment needs: the
// pipeline definition PipelinePath names, and the four mapping files the
// pipeline's catalog block references by path. Nothing here checks their
// content -- these are copied verbatim from tools/publish/mandi_publish and
// already proven there; this only guards the embed directive itself against
// a typo or a file moved out from under it.

import (
	"testing"
)

// wantFiles is every file Files must serve, PipelinePath among them so the
// constant and the embed can never drift apart unnoticed.
var wantFiles = []string{
	PipelinePath,
	"mappings/master-states.yaml",
	"mappings/master-markets.yaml",
	"mappings/market-commodity.yaml",
	"mappings/catalog.yaml",
}

func TestFilesEmbedsThePipelineAndItsMappings(t *testing.T) {
	for _, path := range wantFiles {
		t.Run(path, func(t *testing.T) {
			data, err := Files.ReadFile(path)
			if err != nil {
				t.Fatalf("Files.ReadFile(%q): %v", path, err)
			}
			if len(data) == 0 {
				t.Errorf("Files.ReadFile(%q) returned no content", path)
			}
		})
	}
}
