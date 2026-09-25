package implementation

// publishpipelines_test.go proves the embed carries what the registry names,
// and holds every embedded pipeline to its contract, so a folder added without
// Go is still checked here.

import (
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

const mandiRegistryPath = "pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml"

// Every embedded pipeline loads against the contract and drops no key. This is
// the check a folder-only capability gets instead of a package of its own.
func TestEveryEmbeddedPipelineLoads(t *testing.T) {
	paths := PublishPipelines()
	for _, want := range []string{mandiRegistryPath} {
		found := false
		for _, got := range paths {
			found = found || got == want
		}
		if !found {
			t.Errorf("PublishPipelines() = %v, missing %s", paths, want)
		}
	}

	for _, registryPath := range paths {
		t.Run(registryPath, func(t *testing.T) {
			files, err := PublishPipeline(registryPath)
			if err != nil {
				t.Fatalf("PublishPipeline: %v", err)
			}
			spec, err := pipeline.LoadSpec(files.FS, files.Path)
			if err != nil {
				t.Fatalf("LoadSpec: %v", err)
			}
			if spec.Metadata.Capability == "" {
				t.Error("metadata.capability is empty")
			}
			missing, err := pipeline.UnmappedKeys(files.FS, files.Path)
			if err != nil {
				t.Fatalf("UnmappedKeys: %v", err)
			}
			if len(missing) != 0 {
				t.Errorf("keys the file declares that nothing reads: %v", missing)
			}
		})
	}
}

// A path the binary does not embed is refused by name, never substituted.
func TestPublishPipelineRefusesWhatIsNotEmbedded(t *testing.T) {
	for _, bad := range []string{
		"pkg/plugin/implementation/Nope/cataloguepublish-x/pipeline.yaml",
		"somewhere/else/pipeline.yaml",
		"",
	} {
		if _, err := PublishPipeline(bad); err == nil {
			t.Errorf("PublishPipeline(%q) succeeded; want a refusal", bad)
		}
	}
}
