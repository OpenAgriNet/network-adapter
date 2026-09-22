package pipeline

// collector.go is the seam between this frame and a capability's own pipeline.
//
// The frame owns everything that does not differ by domain: the registry gate,
// the run log, the cron schedule, input resolution, the upstream client, and
// the publish step. A Collector owns the one part that does -- what to fetch,
// how to shape it, and what a catalogue of it looks like.
//
// The interface is deliberately narrow. An earlier version of this code had
// the whole collection inside the frame, parameterised by a dozen knobs, on
// the assumption that every pipeline walks a list of regions and joins them to
// a master list. The second pipeline does not, so the frame does not know how
// collection works at all -- it knows only that it produces catalogues.

import (
	"context"
	"embed"
	"log/slog"
)

// Collector is the domain half of a publish pipeline.
type Collector interface {
	// Capability is the code this collector serves, e.g.
	// "openagrinet:MandiPrice". The frame keys the run log and the output
	// directory on it, so it must be stable across restarts and unique among
	// the pipelines a binary carries.
	Capability() string

	// Pipeline is the YAML and mappings this collector embeds.
	Pipeline() Files

	// Collect fetches and builds. Everything domain-specific happens here,
	// and the frame does not inspect how.
	//
	// Returning an error fails the run, which means it is NOT recorded and
	// will be retried on the next tick. A partial collection is reported
	// through CollectResult.Errors instead, so the publish step's refuseWhen
	// rule can decide whether partial is publishable.
	Collect(ctx context.Context, env RunEnv) (CollectResult, error)
}

// Files is where a collector's pipeline definition lives.
type Files struct {
	// FS and Path locate the pipeline YAML inside the binary.
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
	// refused rather than silently served by whatever this binary happens to
	// embed.
	RegistryPath string
}

// RunEnv is what the frame has already established by the time a collector
// runs: the parsed pipeline, its resolved inputs, a mapper serving its
// mappings, and a client holding a token for its upstream.
type RunEnv struct {
	Spec   Spec
	Inputs map[string]string

	Mapper      Mapper
	MappingBase string

	// Client talks to the pipeline's declared upstream.
	Client *Client

	// Token is already exchanged. Collectors pass it into each call's local
	// map, because it is the mappings that decide where a token belongs in a
	// request -- query parameter here, header elsewhere.
	Token string

	// OutDir is this pipeline's OWN directory, not one shared with other
	// pipelines. A collector may write there freely.
	OutDir string

	Log *slog.Logger
}

// CollectResult is what a collector hands back.
type CollectResult struct {
	// Catalogues are ready to publish, in the order they should be sent.
	Catalogues []Catalogue

	// Errors counts the parts of the collection that failed for a real
	// reason -- as opposed to being legitimately empty. It feeds the publish
	// step's refuseWhen rule: publishing a collection with holes in it would
	// replace whole sections of the network's catalogue with nothing.
	Errors int

	// Counters are the domain's own report numbers, printed for an operator
	// and otherwise not interpreted. Mandi reports states with no data,
	// markets excluded for missing coordinates, and so on; another pipeline
	// reports whatever it has.
	Counters map[string]int
}

// Catalogue is one rendered catalogue document and the identity it carries.
type Catalogue struct {
	// Slug names the file on disk and distinguishes catalogues within a run.
	Slug string

	// CatalogID is the network-facing identity the document publishes under.
	CatalogID string

	Content []byte
}
