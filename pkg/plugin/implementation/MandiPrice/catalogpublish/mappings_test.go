package agmarknet

// mappings_test.go gives this package's tests the pipeline they exercise, and
// proves the embed actually carries what a deployment needs: the pipeline
// definition and the four mapping files its steps and catalog block reference.
//
// There is no Go in this package any more. The pipeline is embedded centrally,
// by folder convention (pkg/plugin/implementation/embedded.go), and
// the crawler finds it through the registry's publish action. The names below
// are what these tests used to import from pipeline_files.go, resolved the
// same way the crawler resolves them -- so the tests run the file production
// runs, reached the way production reaches it.

import (
	"path"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// RegistryPipelinePath is the repo-relative path the registry's publish action
// names for this pipeline.
const RegistryPipelinePath = "pkg/plugin/implementation/MandiPrice/" +
	"catalogpublish-agmarknet/agmarknet.yaml"

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

// The catalog mapping RENDERS; it does not decide. Which markets publish and
// in what order is the pipeline file's catalog block, applied by the engine
// before the mapping runs, and the ids are the engine's too. A second copy of
// either rule here drifts from the first -- and drops markets silently, where
// the file's exclude names every drop.
func TestCatalogMappingNeitherFiltersSortsNorBuildsIds(t *testing.T) {
	raw, err := Files.ReadFile(path.Join(mappingsDir, "catalog.yaml"))
	if err != nil {
		t.Fatalf("reading the mapping: %v", err)
	}
	mapping := string(raw)
	for _, banned := range []string{
		"$isPublishable", "publishWithoutGeometry", // a second exclude rule
		"$sortedMarkets", "$a.marketId", // a second order rule
		`"catalog:mandi-price:" &`, `"resource:mandi-price:market:" &`, // ids built by hand
	} {
		if strings.Contains(mapping, banned) {
			t.Errorf("mappings/catalog.yaml still contains %q", banned)
		}
	}
	for _, want := range []string{"_local.catalogId", "$row.resourceId"} {
		if !strings.Contains(mapping, want) {
			t.Errorf("mappings/catalog.yaml does not use the engine-supplied %s", want)
		}
	}
}

// The Agmarknet -> ISO 3166-2 table is data, reviewed here as data.
// Agmarknet's codes are its own -- measured in real output, KK is Karnataka
// and MG is Meghalaya -- so "IN-" + code publishes wrong areas, which is worse
// than none: consumers trust the codeScheme. Codes not listed get no ISO area.
func TestCatalogMappingAreaCodeTable(t *testing.T) {
	raw, err := Files.ReadFile(path.Join(mappingsDir, "catalog.yaml"))
	if err != nil {
		t.Fatalf("reading the mapping: %v", err)
	}
	mapping := string(raw)
	for code, iso := range map[string]string{
		"CG": "IN-CG", "KK": "IN-KA", "KA": "IN-KA", "MG": "IN-ML",
		"MH": "IN-MH", "MP": "IN-MP", "TN": "IN-TN", "UP": "IN-UP",
	} {
		if !strings.Contains(mapping, `"`+code+`": "`+iso+`"`) {
			t.Errorf("the table does not map %s -> %s", code, iso)
		}
	}
	if strings.Contains(mapping, `"IN-" & $uppercase`) {
		t.Error("the mapping still derives ISO codes from Agmarknet codes")
	}
}
