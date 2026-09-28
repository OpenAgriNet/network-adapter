package implementation

// embedded_test.go proves the embed carries what the registry names,
// and holds every embedded pipeline to its contract, so a folder added without
// Go is still checked here.

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

const mandiRegistryPath = "pkg/plugin/implementation/MandiPrice/catalogpublish-agmarknet/agmarknet.yaml"

// deprecatedMandiPath is where the Mandi pipeline lived before the rename. A
// registry record may still name it for one release.
const deprecatedMandiPath = "pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml"

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
		"pkg/plugin/implementation/Nope/catalogpublish-x/pipeline.yaml",
		"somewhere/else/pipeline.yaml",
		"",
	} {
		if _, err := PublishPipeline(bad); err == nil {
			t.Errorf("PublishPipeline(%q) succeeded; want a refusal", bad)
		}
	}
}

// The embed must live in this parent package: //go:embed forbids "..", so no
// sibling can embed */catalogpublish-*/. The price is that this package must
// stay a LEAF of its children -- importing one would make every capability's
// Go a dependency of every other's pipeline. Held here, not by convention.
func TestEmbedPackageImportsNoChild(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	const self = "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/"
	const allowed = self + "catalogpublisher/pipeline"
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, self) && path != allowed {
				t.Errorf("%s imports %s; the embed package may import only %s", name, path, allowed)
			}
		}
	}
}

// Every publish-pipeline folder on disk must reach the binary. The embed glob
// and the folder convention are two statements of one rule; this is where
// they are held to each other, so a renamed folder or a narrowed glob fails
// here instead of as "this binary embeds no pipeline" at midnight.
func TestEveryPipelineFolderOnDiskIsEmbedded(t *testing.T) {
	embedded := PublishPipelines()
	if len(embedded) == 0 {
		t.Fatal("PublishPipelines() is empty: the embed glob matches nothing")
	}
	onDisk, err := filepath.Glob(filepath.Join("*", pipelineFolderGlob, "*.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(onDisk) == 0 {
		t.Fatalf("no */%s/*.yaml on disk; the convention and this test disagree", pipelineFolderGlob)
	}
	carried := map[string]bool{}
	for _, path := range embedded {
		carried[strings.TrimPrefix(path, registryRoot)] = true
	}
	for _, path := range onDisk {
		if !carried[filepath.ToSlash(path)] {
			t.Errorf("%s is on disk but not embedded (embedded: %v)", path, embedded)
		}
	}
}

// A folder that merely LOOKS like a pipeline folder -- the old British
// spelling -- is not one, and must not sit on disk silently unembedded.
func TestNoPipelineFolderUsesAnotherSpelling(t *testing.T) {
	stray, err := filepath.Glob(filepath.Join("*", "*publish-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range stray {
		if matched, _ := filepath.Match(pipelineFolderGlob, filepath.Base(path)); !matched {
			t.Errorf("%s looks like a publish-pipeline folder but does not match %s, so it is not embedded",
				path, pipelineFolderGlob)
		}
	}
}

// A registry record still naming the old path resolves to the new file for
// one release, under the NEW path (which is what the run log keys on), and
// passes the registry gate. Without this, the rename would stop the sweep
// publishing at the first midnight after deploy, quietly.
func TestDeprecatedMandiPathResolvesToTheNewFile(t *testing.T) {
	files, err := PublishPipeline(deprecatedMandiPath)
	if err != nil {
		t.Fatalf("PublishPipeline(old path): %v", err)
	}
	if files.RegistryPath != mandiRegistryPath {
		t.Fatalf("RegistryPath = %q, want the new path %q", files.RegistryPath, mandiRegistryPath)
	}
	if files.AliasOf != deprecatedMandiPath {
		t.Fatalf("AliasOf = %q, want %q", files.AliasOf, deprecatedMandiPath)
	}
	record := &model.ProviderRecord{BindingKey: "agmarknet-live|openagrinet:MandiPrice",
		Actions: map[string]model.ActionPlan{"publish": {Mappings: deprecatedMandiPath}}}
	if _, err := pipeline.Run(context.Background(), pipeline.RunOptions{
		Pipeline: files, Record: record, DryRun: true,
		Lookup: func(string) (string, bool) { return "", false },
	}); err != nil {
		t.Fatalf("Run(dry) through the old path: %v", err)
	}
}
