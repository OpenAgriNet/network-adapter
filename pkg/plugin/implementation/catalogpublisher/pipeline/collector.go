package pipeline

// collector.go is the seam between this frame and a capability's pipeline.
//
// There is almost nothing to it, and that is the point. A capability supplies
// its pipeline YAML and its mappings; everything else -- what to fetch, in
// what order, how to judge it, what a catalogue of it looks like -- is
// declared in that YAML and executed by this package.
//
// An earlier version of this seam was a Go interface with a Collect method,
// and each capability wrote a few hundred lines implementing it. That made
// every pipeline a programming task and made the YAML decorative: the file
// declared a sequence of steps that nothing read. Now the file is the program.

import "embed"

// Files is a capability's pipeline definition: the YAML, and the mappings it
// references, embedded in the binary.
type Files struct {
	// FS and Path locate the pipeline YAML inside the binary. The mappings
	// are expected in a `mappings/` directory alongside it, because that is
	// what a file's `mapping:` references are relative to.
	FS   embed.FS
	Path string

	// RegistryPath is the repo-relative path the registry's publish action is
	// expected to name, e.g.
	// "pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml".
	//
	// The gate compares the registry's answer against this rather than
	// deriving it from the capability's name. Deriving it would mean guessing
	// a filesystem layout and running a pipeline the registry never
	// sanctioned; comparing means a registry pointing somewhere else is
	// refused rather than silently served by whatever this binary embeds.
	RegistryPath string
}

// Catalogue is one rendered catalogue document and the identity it carries.
type Catalogue struct {
	// Slug names the file on disk and distinguishes catalogues within a run.
	Slug string

	// CatalogID is the network-facing identity the document publishes under.
	CatalogID string

	Content []byte
}
