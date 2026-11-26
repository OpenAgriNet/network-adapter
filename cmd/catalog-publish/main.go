// Command catalog-publish runs ONE scheduled publish pipeline and exits, so an
// external scheduler -- a crontab, a Kubernetes CronJob -- can drive it.
//
// It is the crawler's publish sweep with the crawler removed. The pipeline
// itself is unchanged and unaware: this supplies the three things the sweep
// would otherwise supply.
//
//  1. A pipeline is a HOSTED file, fetched over HTTP. ServeMappings puts the
//     folder on a loopback port for the length of the run.
//  2. The registry sanctions a run by naming the pipeline's URL in a publish
//     action. There is no registry here, so the record is built from config
//     and names the URL we just served it on.
//  3. A Publisher makes the HTTP call. The crawler's own sink is internal to
//     that package, so this is the same contract in forty lines.
//
// Nothing about a deployment is written in Go. Every address, identity, date
// window and mode comes from the config file; credentials come from the
// environment, by the names the pipeline file itself declares, and never pass
// through the config. A second capability is a block in that file.
//
// Timing is the scheduler's: the pipeline file's own cron is overridden to
// always-due, and no run log is kept, so every invocation does the work.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/pipeline"
)

func main() {
	var (
		configPath = flag.String("config", envOr("CATALOG_PUBLISH_CONFIG", "scripts/catalog-publish/catalog-publish.yaml"), "path to the config file")
		name       = flag.String("pipeline", os.Getenv("CATALOG_PUBLISH_PIPELINE"), "which pipeline in the config to run")
		showConfig = flag.Bool("show-config", false, "print the resolved config and exit")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*configPath, *name, *showConfig, log); err != nil {
		log.Error("catalog-publish failed", "error", err)
		os.Exit(1)
	}
}

func run(configPath, name string, showConfig bool, log *slog.Logger) error {
	cfg, err := Load(configPath)
	if err != nil {
		return err
	}
	p, err := cfg.Pipeline(name)
	if err != nil {
		return err
	}
	if err := cfg.Validate(p); err != nil {
		return err
	}
	runTimeout, _ := cfg.Timeout(p)
	pubTimeout, _ := cfg.DiscoveryTimeout()
	overrides := cfg.Overrides(p)
	// What this run DOES with what it builds is config, not a flag: a
	// scheduled run has no command line to pass one on.
	publishing := cfg.Output.PublishesToAPI()
	outDir := cfg.OutDir(p)

	if showConfig {
		printConfig(configPath, cfg, p, overrides, runTimeout, pubTimeout)
		return nil
	}

	log.Info("catalog-publish: starting",
		"config", configPath, "pipeline", p.Name, "capability", p.Capability,
		"participantId", p.ParticipantID, "networkId", cfg.Discovery.NetworkID,
		"output", cfg.Output.Describe())

	// 1. Serve the pipeline folder the way the registry would host it.
	base, stop, err := pipeline.ServeMappings(os.DirFS(p.Dir), ".")
	if err != nil {
		return fmt.Errorf("serving %s: %w", p.Dir, err)
	}
	defer stop()
	url := base + "/" + p.File

	// 2. The registry's answer, made here. Mappings MUST equal the URL above:
	//    the pipeline refuses to run a file its record does not name.
	record := &model.ProviderRecord{
		BindingKey:     p.ParticipantID + "|" + p.Capability,
		ParticipantID:  p.ParticipantID,
		CapabilityCode: p.Capability,
		Actions:        map[string]model.ActionPlan{"publish": {Mappings: url}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	report, err := pipeline.Run(ctx, pipeline.RunOptions{
		Pipeline: pipeline.Files{URL: url},
		Record:   record,
		// No RunLog: the scheduler decides when this runs, so the file's own
		// cron is overridden to always-due and nothing remembers a firing.
		Config:     overrides,
		Now:        time.Now(),
		OutDir:     outDir,
		Publish:    publishing,
		PublishURL: cfg.Discovery.PublishURL,
		Publisher:  &discoverySink{client: &http.Client{Timeout: pubTimeout}},
		Log:        log,
	})
	if err != nil {
		return err
	}
	if !report.Due {
		return fmt.Errorf("not due, which cannot happen with an always-due schedule: %s", report.Reason)
	}

	printReport(report, p, cfg, overrides, publishing)

	if report.Published != nil && report.Published.HasFailures() {
		return fmt.Errorf("some catalogs did not reach the index intact")
	}
	if report.Errors > 0 {
		return fmt.Errorf("%d parts of the collection failed", report.Errors)
	}
	return nil
}

// printConfig is what -show-config prints: everything this run WOULD use,
// so a misconfigured CronJob can be diagnosed without publishing anything.
func printConfig(path string, cfg *Config, p PipelineConfig, overrides map[string]string,
	runTimeout, pubTimeout time.Duration) {
	fmt.Printf("config file:      %s\n", path)
	fmt.Printf("pipeline:         %s\n", p.Name)
	fmt.Printf("capability:       %s\n", p.Capability)
	fmt.Printf("participantId:    %s\n", p.ParticipantID)
	fmt.Printf("networkId:        %s\n", cfg.Discovery.NetworkID)
	if cfg.Output.PublishesToAPI() {
		fmt.Printf("publishUrl:       %s/publish\n", cfg.Discovery.PublishURL)
	} else {
		fmt.Printf("publishUrl:       (not used; this mode publishes nothing)\n")
	}
	fmt.Printf("pipeline file:    %s\n", filepath.Join(p.Dir, p.File))
	fmt.Printf("output mode:      %s\n", cfg.Output.Describe())
	fmt.Printf("output dir:       %s\n", orNone(cfg.OutDir(p)))
	fmt.Printf("run timeout:      %s\n", runTimeout)
	fmt.Printf("publish timeout:  %s\n", pubTimeout)
	fmt.Printf("\ninput overrides handed to the engine:\n")
	for _, k := range sortedStrings(overrides) {
		fmt.Printf("  %-44s %s\n", k, overrides[k])
	}
	fmt.Printf("\ncredentials are NOT read from this file; the pipeline reads them\n")
	fmt.Printf("from the environment by the names it declares as secret.\n")
}

func orNone(s string) string {
	if s == "" {
		return "(none; this mode keeps no files)"
	}
	return s
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func sortedStrings(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// discoverySink is pipeline.Publisher: POST baseURL+/publish, and treat
// anything but an ACCEPTED verdict as a rejection -- a PARTIAL means the
// catalog indexed with pieces missing, which is a failure, not a success.
type discoverySink struct{ client *http.Client }

func (d *discoverySink) Publish(ctx context.Context, baseURL string, body []byte) pipeline.Outcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/publish", bytes.NewReader(body))
	if err != nil {
		return pipeline.Outcome{Status: pipeline.StatusTransportError, Reason: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return pipeline.Outcome{Status: pipeline.StatusTransportError, Reason: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return pipeline.Outcome{Status: pipeline.StatusTransportError, Reason: "reading the answer: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return pipeline.Outcome{Status: pipeline.StatusRejected,
			Reason: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(answer))}
	}

	// An unreadable 200 is not a success: the verdict is what says the
	// catalog landed, and a body we cannot parse carries no verdict.
	var parsed struct {
		Message struct {
			Results []struct {
				CatalogID string `json:"catalogId"`
				Status    string `json:"status"`
			} `json:"results"`
		} `json:"message"`
	}
	if err := json.Unmarshal(answer, &parsed); err != nil {
		return pipeline.Outcome{Status: pipeline.StatusTransportError,
			Reason: "200 with an unreadable body: " + truncate(answer)}
	}
	if len(parsed.Message.Results) == 0 {
		return pipeline.Outcome{Status: pipeline.StatusRejected, Reason: "200 with no results: " + truncate(answer)}
	}
	for _, r := range parsed.Message.Results {
		if r.Status != "ACCEPTED" {
			return pipeline.Outcome{Status: pipeline.StatusRejected,
				Reason: fmt.Sprintf("%s: %s", r.CatalogID, r.Status)}
		}
	}
	return pipeline.Outcome{Status: pipeline.StatusPublished}
}

// Retire is unused: the onDemand pipeline declares no retireOld block.
func (d *discoverySink) Retire(ctx context.Context, baseURL string, r pipeline.Retirement) pipeline.Outcome {
	return pipeline.Outcome{Status: pipeline.StatusRejected,
		Reason: "mandi-publish does not retire catalogs; the pipeline declares no retireOld"}
}

func defaultDir() string {
	return filepath.Join("pkg", "plugin", "implementation", "MandiPrice", "publish")
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// catalogFacts is what one built catalog turned out to contain, read back from
// the document itself rather than from the builder's own counters: the table
// is meant to say what was PUBLISHED, and the document is the only thing that
// was.
type catalogFacts struct {
	slug       string
	catalogID  string
	name       string
	resources  int
	geometries int
	updateMode string
	bytes      int
	status     string
}

// printReport is the run's whole output: a line per state saying what it
// published and in which mode, then the totals. A run that goes wrong is
// meant to be readable from this alone.
func printReport(report pipeline.RunReport, p PipelineConfig, cfg *Config, overrides map[string]string, publishing bool) {
	facts := make([]catalogFacts, 0, len(report.Catalogs))
	for _, built := range report.Catalogs {
		facts = append(facts, readCatalog(built))
	}

	// The publish verdict per catalogId, so each state's row says whether its
	// document actually landed rather than only that it was built.
	verdict := map[string]string{}
	if report.Published != nil {
		for _, o := range report.Published.Outcomes {
			v := o.Status
			if o.Status != pipeline.StatusPublished && o.Reason != "" {
				v = o.Status + " (" + o.Reason + ")"
			}
			verdict[o.CatalogID] = v
		}
	}
	for i := range facts {
		switch v, ok := verdict[facts[i].catalogID]; {
		case ok:
			facts[i].status = v
		case !publishing:
			facts[i].status = "written, not published"
		default:
			facts[i].status = "NO VERDICT"
		}
	}

	sort.Slice(facts, func(i, j int) bool { return facts[i].slug < facts[j].slug })

	rule := strings.Repeat("=", 122)
	in := func(key string) string {
		if v, ok := overrides["publish."+p.Name+"."+key]; ok && v != "" {
			return v
		}
		return "(pipeline default)"
	}
	fmt.Printf("\n%s\n", rule)
	fmt.Printf("PUBLISH RESULT   pipeline=%s   capability=%s\n", p.Name, p.Capability)
	fmt.Printf("  participantId=%s   networkId=%s   discovery=%s/publish\n",
		p.ParticipantID, cfg.Discovery.NetworkID, cfg.Discovery.PublishURL)
	fmt.Printf("  mode=%s   window=%s..%s   updateMode=%s\n",
		in("mode"), in("fromDate"), in("toDate"), updateModes(facts))
	fmt.Printf("  output=%s\n", cfg.Output.Describe())
	fmt.Printf("%s\n", rule)
	fmt.Printf("%-6s %-30s %-30s %9s %9s %6s  %s\n",
		"SLUG", "CATALOG NAME", "CATALOG ID", "RESOURCES", "GEOMETRY", "KiB", "PUBLISH")
	fmt.Printf("%s\n", strings.Repeat("-", 122))

	var totRes, totGeo, landed int
	for _, f := range facts {
		fmt.Printf("%-6s %-30s %-30s %9d %9d %6d  %s\n",
			f.slug, truncateStr(f.name, 30), f.catalogID,
			f.resources, f.geometries, f.bytes/1024, f.status)
		totRes += f.resources
		totGeo += f.geometries
		if f.status == pipeline.StatusPublished {
			landed++
		}
	}
	fmt.Printf("%s\n", strings.Repeat("-", 122))
	fmt.Printf("%-6s %-30s %-30s %9d %9d\n",
		"", fmt.Sprintf("%d catalogs", len(facts)), "", totRes, totGeo)

	// The states that produced nothing are invisible in a table of what was
	// published, and they are one of the two reasons a state goes missing.
	fmt.Printf("\ngroups published:     %d\n", report.Counters["groups"])
	fmt.Printf("catalogs built:       %d  (a group over the geometry budget splits)\n", len(report.Catalogs))
	fmt.Printf("groups with no rows:  %d\n", report.Counters["emptyGroups"])
	fmt.Printf("resources excluded:   %d\n", report.Counters["excluded"])
	fmt.Printf("failed parts:         %d\n", report.Errors)
	if publishing {
		fmt.Printf("published OK:         %d / %d\n", landed, len(facts))
	}
	if report.OutDir != "" {
		fmt.Printf("catalogs written to:  %s\n", report.OutDir)
	}

	fmt.Printf("\ncounters:\n")
	for _, name := range sortedKeys(report.Counters) {
		fmt.Printf("  %-38s %d\n", name, report.Counters[name])
	}
}

// updateModes reports the mode every catalog went out under, naming all of
// them if they disagree -- a run where half the states replace and half merge
// is a fault worth seeing in the header rather than per row.
func updateModes(facts []catalogFacts) string {
	seen := map[string]bool{}
	var modes []string
	for _, f := range facts {
		if f.updateMode != "" && !seen[f.updateMode] {
			seen[f.updateMode] = true
			modes = append(modes, f.updateMode)
		}
	}
	if len(modes) == 0 {
		return "none"
	}
	sort.Strings(modes)
	return strings.Join(modes, "+")
}

// readCatalog counts what a built document actually carries. Geometries are
// counted the way discovery counts them -- by GeoJSON shape at any depth,
// not by field name -- so this is the number the publish budget is spent
// against, not an approximation of it.
func readCatalog(built pipeline.BuiltCatalog) catalogFacts {
	f := catalogFacts{slug: built.Slug, catalogID: built.CatalogID, bytes: len(built.Content)}

	var doc struct {
		Message struct {
			Catalogs []struct {
				Descriptor struct {
					Name string `json:"name"`
				} `json:"descriptor"`
				Resources []json.RawMessage `json:"resources"`
			} `json:"catalogs"`
			PublishDirectives []struct {
				UpdateMode string `json:"updateMode"`
			} `json:"publishDirectives"`
		} `json:"message"`
	}
	if err := json.Unmarshal(built.Content, &doc); err != nil {
		f.name = "UNREADABLE: " + err.Error()
		return f
	}
	if len(doc.Message.PublishDirectives) > 0 {
		f.updateMode = doc.Message.PublishDirectives[0].UpdateMode
	}
	if len(doc.Message.Catalogs) == 0 {
		return f
	}
	f.name = doc.Message.Catalogs[0].Descriptor.Name
	f.resources = len(doc.Message.Catalogs[0].Resources)

	var whole any
	if err := json.Unmarshal(built.Content, &whole); err == nil {
		f.geometries = countGeometries(whole)
	}
	return f
}

// countGeometries recognises GeoJSON by shape -- a "type" beside a
// "coordinates" -- which is exactly how discovery's publish walk finds them.
func countGeometries(node any) int {
	switch v := node.(type) {
	case map[string]any:
		n := 0
		_, hasType := v["type"]
		_, hasCoords := v["coordinates"]
		if hasType && hasCoords {
			n++
		}
		for _, child := range v {
			n += countGeometries(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range v {
			n += countGeometries(child)
		}
		return n
	}
	return 0
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
