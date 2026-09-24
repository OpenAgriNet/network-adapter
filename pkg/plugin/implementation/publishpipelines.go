// Package implementation carries every capability's publish pipeline into the
// binary, so that adding one is a folder and a registry entry -- not Go.
//
// A capability publishes by keeping its pipeline beside its code:
//
//	pkg/plugin/implementation/<Capability>/cataloguepublish-<source>/<name>.yaml
//	pkg/plugin/implementation/<Capability>/cataloguepublish-<source>/mappings/*.yaml
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
	"path"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

// publishFiles is every capability's pipeline YAML and its mappings, found by
// folder convention. A folder named cataloguepublish-* is a publish pipeline.
//
//go:embed */cataloguepublish-*/*.yaml */cataloguepublish-*/mappings/*.yaml
var publishFiles embed.FS

// registryRoot is where the registry's repo-relative paths start. The embed
// is rooted at this package's directory, so a path is found by removing it.
const registryRoot = "pkg/plugin/implementation/"

// PublishPipeline resolves the pipeline the registry's publish action names.
//
// A path this binary does not embed is an error that says so, rather than a
// guess: the registry pointing at a pipeline that was never built in is a
// deployment problem, and running some other file in its place would publish
// something nobody sanctioned.
func PublishPipeline(registryPath string) (pipeline.Files, error) {
	cleaned := path.Clean(strings.TrimSpace(registryPath))
	if !strings.HasPrefix(cleaned, registryRoot) {
		return pipeline.Files{}, fmt.Errorf("pipeline path %q is not under %s, where publish pipelines live",
			registryPath, registryRoot)
	}
	inside := strings.TrimPrefix(cleaned, registryRoot)

	if _, err := fs.Stat(publishFiles, inside); err != nil {
		return pipeline.Files{}, fmt.Errorf("this binary embeds no pipeline at %q; a publish pipeline lives in "+
			"<Capability>/cataloguepublish-<source>/ under %s and is built in by rebuilding", cleaned, registryRoot)
	}
	return pipeline.Files{FS: publishFiles, Path: inside, RegistryPath: cleaned}, nil
}

// PublishPipelines lists the registry path of every pipeline this binary
// embeds, for a log line or an error that says what is available.
//
// A pipeline is a YAML file directly inside a cataloguepublish-* folder; the
// ones under mappings/ are its mappings, not pipelines.
func PublishPipelines() []string {
	matches, _ := fs.Glob(publishFiles, "*/cataloguepublish-*/*.yaml")
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, registryRoot+match)
	}
	return out
}
