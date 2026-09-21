package agmarket

import "embed"

// Files carries this pipeline's YAML definition and its mappings into any
// binary that imports this package, so a deployment needs no working
// directory and cannot be pointed at an edited pipeline by accident.
//
//go:embed mandi-price-agmarket.yaml mappings
var Files embed.FS

// PipelinePath is Files' path to the pipeline definition itself.
const PipelinePath = "mandi-price-agmarket.yaml"
