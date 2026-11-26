package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole of this binary's environment. Nothing it needs is
// written in Go: a second capability is a block in this file, not a build.
//
// Values may name an environment variable with ${VAR} or ${VAR:-default},
// which is what lets one ConfigMap serve every deployment while the secrets
// and the per-cluster addresses arrive separately.
type Config struct {
	// Discovery is where built catalogs are sent. Shared by every pipeline,
	// because a deployment publishes to one network.
	Discovery struct {
		// PublishURL is the BASE address; /publish is appended by the sink.
		PublishURL string `yaml:"publishUrl"`
		// NetworkID scopes what a consumer can see, and is stamped on every
		// catalog as visibleTo.
		NetworkID string `yaml:"networkId"`
		Timeout   string `yaml:"timeout"`
	} `yaml:"discovery"`

	// Output decides what a run DOES with the catalogs it builds.
	Output OutputConfig `yaml:"output"`

	// Defaults apply to every pipeline, and each may override them.
	Defaults PipelineConfig `yaml:"defaults"`

	Pipelines []PipelineConfig `yaml:"pipelines"`

	// baseDir is the directory the config file was read from. A relative
	// `dir` resolves against it rather than against the working directory,
	// so the same command works from the repo root and from beside the
	// config -- resolving against the cwd meant a documented command that
	// failed depending on where it was typed.
	baseDir string
}

// Output modes. Building is observable and reversible; publishing is neither,
// which is why `files` is the default and `api` is opted into.
const (
	OutputFiles = "files" // write one document per group, publish nothing
	OutputAPI   = "api"   // publish to discovery, keep no files
	OutputBoth  = "both"  // write the documents AND publish them
)

// OutputConfig is the run's one real decision: files, the API, or both.
type OutputConfig struct {
	Mode string `yaml:"mode"`
	// Dir is where documents are written under `files` and `both`. Each
	// pipeline gets its own subdirectory beneath it.
	Dir string `yaml:"dir"`
}

// WritesFiles and PublishesToAPI read the mode, so no caller re-derives it.
func (o OutputConfig) WritesFiles() bool {
	return o.mode() == OutputFiles || o.mode() == OutputBoth
}

func (o OutputConfig) PublishesToAPI() bool {
	return o.mode() == OutputAPI || o.mode() == OutputBoth
}

func (o OutputConfig) mode() string {
	if strings.TrimSpace(o.Mode) == "" {
		return OutputFiles
	}
	return strings.ToLower(strings.TrimSpace(o.Mode))
}

// Describe is the mode as a sentence, for the log line and the report header.
func (o OutputConfig) Describe() string {
	switch o.mode() {
	case OutputAPI:
		return "api (publish to discovery, no files kept)"
	case OutputBoth:
		return "both (write the documents and publish them)"
	default:
		return "files (write the documents, publish nothing)"
	}
}

// PipelineConfig is one capability's publish job.
type PipelineConfig struct {
	// Name is both the CLI selector and the pipeline file's metadata.name,
	// which is what the input overrides below are keyed on.
	Name string `yaml:"name"`

	// Capability is the code the registry knows this pipeline by. It must
	// equal the file's metadata.capability.
	Capability string `yaml:"capability"`

	// ParticipantID is who the catalogs are published AS.
	ParticipantID string `yaml:"participantId"`

	// Dir holds the pipeline YAML and its mappings/ folder; File names the
	// YAML within it.
	Dir  string `yaml:"dir"`
	File string `yaml:"file"`

	// Schedule replaces the pipeline file's own cron. An external scheduler
	// owns timing, so this defaults to always-due -- see resolveSchedule.
	Schedule string `yaml:"schedule"`

	// Timeout bounds the whole run. OutDir keeps the built catalogs.
	Timeout string `yaml:"timeout"`
	OutDir  string `yaml:"outDir"`

	// Inputs are the pipeline file's own declared inputs, by their names in
	// that file -- mode, fromDate, baseUrl and so on. They are passed as
	// `publish.<name>.<input>` overrides, which the engine type-checks
	// against the file's declarations, so a wrong key fails loudly.
	//
	// SECRETS CANNOT GO HERE. The engine refuses an override of an input
	// marked secret; those are read from the environment by the names the
	// pipeline file states, so a credential never passes through this file.
	Inputs map[string]string `yaml:"inputs"`
}

// Load reads the config, expands ${VAR} references and applies defaults.
//
// Expansion happens AFTER parsing, over the parsed values only. Expanding the
// raw text would also expand the comments, and a comment documenting ${VAR}
// would then fail the load.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	var missing []string
	tree = expandTree(tree, &missing)
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("config %s references variables that are not set: %s",
			path, strings.Join(dedupe(missing), ", "))
	}

	expanded, err := yaml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("re-encoding config %s: %w", path, err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(expanded)))
	decoder.KnownFields(true) // a misspelled key is an error, not a silent no-op
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if len(cfg.Pipelines) == 0 {
		return nil, fmt.Errorf("config %s declares no pipelines", path)
	}
	cfg.baseDir = filepath.Dir(path)
	for i := range cfg.Pipelines {
		cfg.Pipelines[i] = merge(cfg.Defaults, cfg.Pipelines[i])
	}
	return &cfg, nil
}

// PipelineDir is where this pipeline's YAML and mappings live, resolved. An
// absolute `dir` is taken as given; a relative one is relative to the config
// file.
func (c *Config) PipelineDir(p PipelineConfig) string {
	if p.Dir == "" || filepath.IsAbs(p.Dir) {
		return p.Dir
	}
	return filepath.Clean(filepath.Join(c.baseDir, p.Dir))
}

// expandTree rewrites every string in a parsed document, keys included.
func expandTree(node any, missing *[]string) any {
	switch v := node.(type) {
	case string:
		return expand(v, missing)
	case []any:
		for i := range v {
			v[i] = expandTree(v[i], missing)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[expand(key, missing)] = expandTree(value, missing)
		}
		return out
	case map[any]any:
		out := make(map[any]any, len(v))
		for key, value := range v {
			out[expandTree(key, missing)] = expandTree(value, missing)
		}
		return out
	}
	return node
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Pipeline returns the named pipeline, or the only one when the name is empty
// and there is exactly one. Naming nothing against several is an error rather
// than a guess: publishing the wrong capability is not recoverable by rerunning.
func (c *Config) Pipeline(name string) (PipelineConfig, error) {
	if name == "" {
		if len(c.Pipelines) == 1 {
			return c.Pipelines[0], nil
		}
		return PipelineConfig{}, fmt.Errorf(
			"the config declares %d pipelines (%s), so -pipeline must name one",
			len(c.Pipelines), strings.Join(c.names(), ", "))
	}
	for _, p := range c.Pipelines {
		if p.Name == name {
			return p, nil
		}
	}
	return PipelineConfig{}, fmt.Errorf("no pipeline named %q; the config declares %s",
		name, strings.Join(c.names(), ", "))
}

func (c *Config) names() []string {
	out := make([]string, 0, len(c.Pipelines))
	for _, p := range c.Pipelines {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Validate refuses a config that would publish under an empty identity or to
// nowhere. Every one of these is checked here rather than at the point of use,
// so a misconfigured CronJob fails in its first second and says why.
func (c *Config) Validate(p PipelineConfig) error {
	switch c.Output.mode() {
	case OutputFiles, OutputAPI, OutputBoth:
	default:
		return fmt.Errorf("output.mode is %q; it must be %q, %q or %q",
			c.Output.Mode, OutputFiles, OutputAPI, OutputBoth)
	}
	// A mode that writes files but names no directory would write them to a
	// temporary one and delete them, which reads as "the run produced
	// nothing" rather than as a missing setting.
	if c.Output.WritesFiles() && strings.TrimSpace(c.OutDir(p)) == "" {
		return fmt.Errorf("output.mode is %q but output.dir is empty, so the documents would be written to a temporary directory and removed", c.Output.mode())
	}

	var missing []string
	need := map[string]string{
		"name":          p.Name,
		"capability":    p.Capability,
		"participantId": p.ParticipantID,
		"dir":           p.Dir,
		"file":          p.File,
	}
	// Discovery's address and network only matter to a run that publishes.
	// Requiring them of a files-only run would mean inventing a URL to write
	// catalogs to disk.
	if c.Output.PublishesToAPI() {
		need["discovery.publishUrl"] = c.Discovery.PublishURL
		need["discovery.networkId"] = c.Discovery.NetworkID
	}
	for k, v := range need {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("config is incomplete: %s", strings.Join(missing, ", "))
	}
	dir := c.PipelineDir(p)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("pipeline dir %q (from `dir: %s`, resolved against %s): %w",
			dir, p.Dir, c.baseDir, err)
	}
	if _, err := c.Timeout(p); err != nil {
		return err
	}
	if _, err := c.DiscoveryTimeout(); err != nil {
		return err
	}
	return nil
}

// OutDir is where this pipeline's documents are written: the pipeline's own
// setting when it states one, otherwise the shared output directory. Empty
// when the mode keeps no files.
func (c *Config) OutDir(p PipelineConfig) string {
	if !c.Output.WritesFiles() {
		return ""
	}
	if strings.TrimSpace(p.OutDir) != "" {
		return p.OutDir
	}
	return c.Output.Dir
}

// Timeout bounds one run; DiscoveryTimeout bounds one publish call.
func (c *Config) Timeout(p PipelineConfig) (time.Duration, error) {
	return duration("timeout", p.Timeout, 2*time.Hour)
}

func (c *Config) DiscoveryTimeout() (time.Duration, error) {
	return duration("discovery.timeout", c.Discovery.Timeout, 2*time.Minute)
}

func duration(field, value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration (try 90m, 2h): %w", field, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s %q is not positive", field, value)
	}
	return d, nil
}

// Overrides is what the engine is handed: the pipeline file's own inputs, by
// the names that file declares, plus the schedule.
func (c *Config) Overrides(p PipelineConfig) map[string]string {
	out := map[string]string{
		"publish." + p.Name + ".schedule": resolveSchedule(p.Schedule),
	}
	for key, value := range p.Inputs {
		out["publish."+p.Name+"."+key] = value
	}
	// Identity and scope are the deployment's, not the pipeline file's, so
	// they are set here and cannot be overridden by an inputs: block that
	// happens to name them.
	out["publish."+p.Name+".participantId"] = p.ParticipantID
	out["publish."+p.Name+".networkId"] = c.Discovery.NetworkID
	return out
}

// resolveSchedule defaults to always-due. An external scheduler -- cron, a
// Kubernetes CronJob -- decides when this process starts, and a second opinion
// inside it would mean a CronJob that fires and silently does nothing.
func resolveSchedule(declared string) string {
	if strings.TrimSpace(declared) == "" {
		return "* * * * *"
	}
	return declared
}

// merge fills a pipeline's empty fields from the defaults block.
func merge(defaults, p PipelineConfig) PipelineConfig {
	if p.Capability == "" {
		p.Capability = defaults.Capability
	}
	if p.ParticipantID == "" {
		p.ParticipantID = defaults.ParticipantID
	}
	if p.Dir == "" {
		p.Dir = defaults.Dir
	}
	if p.File == "" {
		p.File = defaults.File
	}
	if p.Schedule == "" {
		p.Schedule = defaults.Schedule
	}
	if p.Timeout == "" {
		p.Timeout = defaults.Timeout
	}
	if p.OutDir == "" {
		p.OutDir = defaults.OutDir
	}
	merged := map[string]string{}
	for k, v := range defaults.Inputs {
		merged[k] = v
	}
	for k, v := range p.Inputs {
		merged[k] = v
	}
	p.Inputs = merged
	return p
}

// expand resolves ${VAR} and ${VAR:-default} in one value. An unset variable
// with no default is RECORDED AS MISSING rather than becoming an empty
// string: an empty participantId or publishUrl is exactly the kind of value
// that publishes something wrong instead of failing.
func expand(in string, missing *[]string) string {
	if !strings.Contains(in, "${") {
		return in
	}
	var out strings.Builder
	for i := 0; i < len(in); {
		if in[i] != '$' || i+1 >= len(in) || in[i+1] != '{' {
			out.WriteByte(in[i])
			i++
			continue
		}
		end := strings.IndexByte(in[i:], '}')
		if end < 0 {
			out.WriteString(in[i:]) // unterminated: leave it alone, it is not a reference
			break
		}
		ref := in[i+2 : i+end]
		i += end + 1

		name, fallback, hasFallback := strings.Cut(ref, ":-")
		if value, ok := os.LookupEnv(name); ok && value != "" {
			out.WriteString(value)
			continue
		}
		if hasFallback {
			out.WriteString(fallback)
			continue
		}
		*missing = append(*missing, name)
	}
	return out.String()
}
