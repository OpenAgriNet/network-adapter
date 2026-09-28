// Package implementation carries every capability's publish pipeline into the
// binary, so that adding one is a folder and a registry entry -- not Go.
//
// A capability publishes by keeping its pipeline beside its code:
//
//	pkg/plugin/implementation/<Capability>/catalogpublish-<source>/<name>.yaml
//	pkg/plugin/implementation/<Capability>/catalogpublish-<source>/mappings/*.yaml
//
// and by the registry's publish action naming that YAML by its repo-relative
// path. The crawler asks the registry which capabilities publish, and hands
// each named path to PublishPipeline. Nothing in Go lists the capabilities.
//
// The files are EMBEDDED, not read from disk. The runtime image carries the
// server binary and its plugins, not pkg/, so a path read from disk would
// resolve nowhere in a container; and a pipeline read from disk would publish
// whatever an edited file on the host says, which is not what was reviewed and
// built. The cost is a rebuild to change a pipeline, which is the point.
package implementation

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// publishFiles is every capability's pipeline YAML and its mappings, found by
// folder convention. A folder named catalogpublish-* is a publish pipeline.
//
//go:embed */catalogpublish-*/*.yaml */catalogpublish-*/mappings/*.yaml
var publishFiles embed.FS

// registryRoot is where the registry's repo-relative paths start. The embed
// is rooted at this package's directory, so a path is found by removing it.
const registryRoot = "pkg/plugin/implementation/"

// pipelineFolderGlob names a publish-pipeline folder. The //go:embed line
// above cannot reference a constant, so it repeats this literal; the test
// TestEveryPipelineFolderOnDiskIsEmbedded holds the two together, because a
// folder that stops matching the embed is silently absent from the binary.
const pipelineFolderGlob = "catalogpublish-*"

// deprecatedPaths maps a registry path this binary used to embed to the file
// that replaced it.
//
// The pipeline path lives in a Sunbird registry record (the publish action's
// `mappings`), not only in this repo, so a rename here would leave the
// registry naming a path that no longer exists -- and the sweep would stop
// publishing at the next firing, quietly. Each entry is kept for ONE release,
// while the record is updated; a later change deletes it together with the
// run log's capability-keyed fallback.
var deprecatedPaths = map[string]string{
	"pkg/plugin/implementation/MandiPrice/cataloguepublish-agmarket/mandi-price-agmarket.yaml": "pkg/plugin/implementation/MandiPrice/catalogpublish-agmarknet/agmarknet.yaml",
}

// PublishPipeline resolves the pipeline the registry's publish action names.
//
// A path this binary does not embed is an error that says so, rather than a
// guess: the registry pointing at a pipeline that was never built in is a
// deployment problem, and running some other file in its place would publish
// something nobody sanctioned.
func PublishPipeline(registryPath string) (pipeline.Files, error) {
	cleaned := path.Clean(strings.TrimSpace(registryPath))
	alias := ""
	if replacement, deprecated := deprecatedPaths[cleaned]; deprecated {
		slog.Warn("publish pipeline: the registry names a deprecated pipeline path; "+
			"update the record's publish mappings", "deprecated", cleaned, "use", replacement)
		alias, cleaned = cleaned, replacement
	}
	if !strings.HasPrefix(cleaned, registryRoot) {
		return pipeline.Files{}, fmt.Errorf("pipeline path %q is not under %s, where publish pipelines live",
			registryPath, registryRoot)
	}
	inside := strings.TrimPrefix(cleaned, registryRoot)

	if _, err := fs.Stat(publishFiles, inside); err != nil {
		return pipeline.Files{}, fmt.Errorf("this binary embeds no pipeline at %q; a publish pipeline lives in "+
			"<Capability>/catalogpublish-<source>/ under %s and is built in by rebuilding", cleaned, registryRoot)
	}
	return pipeline.Files{FS: publishFiles, Path: inside, RegistryPath: cleaned, AliasOf: alias}, nil
}

// PublishPipelines lists the registry path of every pipeline this binary
// embeds, for a log line or an error that says what is available.
//
// A pipeline is a YAML file directly inside a catalogpublish-* folder; the
// ones under mappings/ are its mappings, not pipelines.
func PublishPipelines() []string {
	matches, err := fs.Glob(publishFiles, "*/"+pipelineFolderGlob+"/*.yaml")
	if err != nil {
		// Only a malformed pattern errors, and the pattern is a constant.
		panic(fmt.Sprintf("publish pipeline glob: %v", err))
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, registryRoot+match)
	}
	return out
}
