// Package agmarket publishes Agmarknet mandi prices as a Beckn catalogue.
//
// ALMOST NOTHING LIVES HERE, and what does is not Go. The pipeline is
// mandi-price-agmarket.yaml: which upstream to call, in what order, how to
// authenticate, how to judge a coordinate, how to group markets into
// catalogues and when to refuse to publish are all declared there and
// executed by catalogpublisher/pipeline.
//
// So this package is the YAML, the JSONata mappings it references, and the
// three lines below that carry them into the binary. Adding a second
// capability means writing its own YAML and mappings -- not writing Go.
package agmarket

import (
	"embed"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// Files carries this pipeline's YAML definition and its mappings into any
// binary that imports this package, so a deployment needs no working
// directory and cannot be pointed at an edited pipeline by accident.
//
//go:embed mandi-price-agmarket.yaml mappings
var Files embed.FS

// PipelinePath is Files' path to the pipeline definition itself.
const PipelinePath = "mandi-price-agmarket.yaml"

// RegistryPipelinePath is the repo-relative path the registry's publish
// action must name for this pipeline to run. The gate compares against it
// rather than deriving it, so a registry pointing elsewhere is refused
// instead of being served by whatever this binary happens to embed.
const RegistryPipelinePath = "pkg/plugin/implementation/MandiPrice/" +
	"cataloguepublish-agmarket/mandi-price-agmarket.yaml"

// Capability is the code the registry knows this pipeline by. It is declared
// in the YAML as metadata.capability; this constant exists only so the
// crawler's pipeline table can name it without parsing the file first, and
// the frame checks the two agree.
const Capability = "openagrinet:MandiPrice"

// Pipeline is what a runner needs to execute this capability.
func Pipeline() pipeline.Files {
	return pipeline.Files{FS: Files, Path: PipelinePath, RegistryPath: RegistryPipelinePath}
}
