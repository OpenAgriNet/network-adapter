package main

// The one-stage path: collect from the upstream and write per-state catalogs.
//
// There is no collection document. It existed because the two stages were
// built separately and joined by a file, which meant every publish went
// through an artifact nobody had to read. Collect hands its result straight to
// the builder in memory.

import "context"

// runConfig is one end-to-end run.
type runConfig struct {
	collect config
	build   buildConfig
}

// run collects and builds, returning what was built, what was left out, and
// the collection itself so a caller can report on it.
func run(ctx context.Context, cfg runConfig) ([]BuiltState, SkipSummary, Collection, error) {
	// collect validates credentials before doing anything, which is why
	// nothing on disk is touched until it returns.
	collection, err := collect(ctx, cfg.collect)
	if err != nil {
		return nil, SkipSummary{}, Collection{}, err
	}

	built, summary, err := buildCollection(ctx, collection, cfg.build)
	return built, summary, collection, err
}
