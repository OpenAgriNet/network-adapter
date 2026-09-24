package agmarket

// mappings_test.go gives this package's tests the pipeline they exercise, and
// proves the embed actually carries what a deployment needs: the pipeline
// definition and the four mapping files its steps and catalog block reference.
//
// There is no Go in this package any more. The pipeline is embedded centrally,
// by folder convention (pkg/plugin/implementation/publishpipelines.go), and
// the crawler finds it through the registry's publish action. The names below
// are what these tests used to import from pipeline_files.go, resolved the
// same way the crawler resolves them -- so the tests run the file production
// runs, reached the way production reaches it.

import (
	"path"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// RegistryPipelinePath is the repo-relative path the registry's publish action
// names for this pipeline.
const RegistryPipelinePath = "pkg/plugin/implementation/MandiPrice/" +
	"cataloguepublish-agmarket/mandi-price-agmarket.yaml"

// Capability is the code the registry knows this pipeline by, as declared in
// the YAML's metadata.capability (spec_conformance_test checks they agree).
const Capability = "openagrinet:MandiPrice"

// pipelineFiles is the pipeline as the crawler resolves it.
var pipelineFiles = func() pipeline.Files {
	files, err := implementation.PublishPipeline(RegistryPipelinePath)
	if err != nil {
		panic(err)
	}
	return files
}()

// Files, PipelinePath and mappingsDir address the pipeline inside the central
// embed, where it sits under its own folder rather than at the root.
var (
	Files        = pipelineFiles.FS
	PipelinePath = pipelineFiles.Path
	mappingsDir  = path.Join(path.Dir(PipelinePath), "mappings")
)

// Pipeline is what a runner needs to execute this capability.
func Pipeline() pipeline.Files { return pipelineFiles }

// wantFiles is every file the embed must serve for this pipeline.
var wantFiles = []string{
	PipelinePath,
	path.Join(mappingsDir, "master-states.yaml"),
	path.Join(mappingsDir, "master-markets.yaml"),
	path.Join(mappingsDir, "market-commodity.yaml"),
	path.Join(mappingsDir, "catalog.yaml"),
}

func TestFilesEmbedsThePipelineAndItsMappings(t *testing.T) {
	for _, file := range wantFiles {
		t.Run(file, func(t *testing.T) {
			data, err := Files.ReadFile(file)
			if err != nil {
				t.Fatalf("Files.ReadFile(%q): %v", file, err)
			}
			if len(data) == 0 {
				t.Errorf("Files.ReadFile(%q) returned no content", file)
			}
		})
	}
}
