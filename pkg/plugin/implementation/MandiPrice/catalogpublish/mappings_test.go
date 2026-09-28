package catalogpublish

// mappings_test.go gives this package's tests the pipeline they exercise.
//
// There is no Go in this folder that production runs. The pipeline and its
// mappings are HOSTED -- the registry's publish action names the pipeline's
// https URL, and the crawler fetches it the way the adapter fetches every
// mapping. These tests host this folder the same way, on a loopback server, so
// they run the file production runs, reached the way production reaches it.

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// Capability is the code the registry knows this pipeline by, as declared in
// the YAML's metadata.capability (spec_conformance_test checks they agree).
const Capability = "openagrinet:MandiPrice"

// Files, PipelinePath and mappingsDir address this folder on disk, for the
// tests that read the files directly.
var (
	Files        = os.DirFS(".").(fs.ReadFileFS)
	PipelinePath = "agmarknet.yaml"
	mappingsDir  = "mappings"
)

// host serves this folder over HTTP, as it is served in production.
var host = func() string {
	base, _, err := pipeline.ServeMappings(Files, ".")
	if err != nil {
		panic(err)
	}
	return base
}()

// RegistryPipelinePath is what the registry's publish action names for this
// pipeline: its URL.
var RegistryPipelinePath = host + "/" + PipelinePath

// Pipeline is what a runner needs to execute this capability.
func Pipeline() pipeline.Files { return pipeline.Files{URL: RegistryPipelinePath} }

// wantFiles is every file this pipeline needs hosted beside it.
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
