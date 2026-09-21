// spec.go types and parses mandi-price-agmarket.yaml itself. The engine that
// runs a CataloguePipeline doesn't exist yet (see the file's own header
// comment), so this is deliberately just the YAML<->struct mapping the
// polling layer will need to validate and read the file with -- not a
// pipeline executor. Field names and shapes come straight from the file, not
// from what would be convenient to build later.
package agmarket

import (
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Spec is the whole pipeline definition, one file mapped one-to-one.
type Spec struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   Metadata         `yaml:"metadata"`
	Schedule   Schedule         `yaml:"schedule"`
	Inputs     map[string]Input `yaml:"inputs"`
	Upstream   Upstream         `yaml:"upstream"`
	Pipeline   []Step           `yaml:"pipeline"`
	Catalog    Catalog          `yaml:"catalog"`
	Publish    Publish          `yaml:"publish"`
}

// Metadata identifies this pipeline for registry lookups (see the file's
// registry block: capabilityCode there is metadata.capability).
type Metadata struct {
	Name       string `yaml:"name"`
	Capability string `yaml:"capability"`
	Provider   string `yaml:"provider"`
	Summary    string `yaml:"summary"`
}

// Schedule is when the polling layer should run this pipeline.
type Schedule struct {
	At       string `yaml:"at"`
	Timezone string `yaml:"timezone"`
}

// Input is one entry of the CLI/env surface, resolved flag > env > default
// by tools/publish/mandi_publish's proven order. Secret marks the two
// credential inputs that have no flag and must never be logged or echoed.
type Input struct {
	Flag    string      `yaml:"flag,omitempty"`
	Env     string      `yaml:"env,omitempty"`
	Type    string      `yaml:"type,omitempty"`
	Format  string      `yaml:"format,omitempty"`
	Default interface{} `yaml:"default,omitempty"`
	Enum    []string    `yaml:"enum,omitempty"`
	Secret  bool        `yaml:"secret,omitempty"`
}

// Upstream is the one service this pipeline calls and how it authenticates.
type Upstream struct {
	BaseURL string `yaml:"baseUrl"`
	Auth    Auth   `yaml:"auth"`
}

// Auth is the token-exchange upstream auth needs: no expiry in the response,
// so a token is held for the run and re-exchanged only on the statuses in
// Token.ReexchangeOn, never on a timer.
type Auth struct {
	Kind    string        `yaml:"kind"`
	Request AuthRequest   `yaml:"request"`
	Token   AuthTokenSpec `yaml:"token"`
}

// AuthRequest is the call that mints a token.
type AuthRequest struct {
	Method string            `yaml:"method"`
	Path   string            `yaml:"path"`
	Body   map[string]string `yaml:"body"`
}

// AuthTokenSpec is where the token comes from in the response and how it
// rides on later requests.
type AuthTokenSpec struct {
	At           string `yaml:"at"`
	CarriedAs    string `yaml:"carriedAs"`
	Name         string `yaml:"name"`
	ReexchangeOn []int  `yaml:"reexchangeOn"`
}

// Step is one pipeline stage. Only With's fields a given step actually sets
// are populated; the rest stay zero.
type Step struct {
	ID   string `yaml:"id"`
	Uses string `yaml:"uses"`
	With With   `yaml:"with"`
	Out  string `yaml:"out,omitempty"`
}

// With is a step's parameters. It is the union of every field any step in
// this file uses -- http.get steps set Path/Mapping/Local, join sets
// Left/Right/On/Type/Carry, derive sets Field/Rules/Then, dedupe sets Key --
// because YAML steps are heterogeneous and Go structs are not.
type With struct {
	Path    string            `yaml:"path,omitempty"`
	Mapping string            `yaml:"mapping,omitempty"`
	Local   map[string]string `yaml:"local,omitempty"`

	Left  string   `yaml:"left,omitempty"`
	Right string   `yaml:"right,omitempty"`
	On    string   `yaml:"on,omitempty"`
	Type  string   `yaml:"type,omitempty"`
	Carry []string `yaml:"carry,omitempty"`

	Field string                   `yaml:"field,omitempty"`
	Rules []map[string]interface{} `yaml:"rules,omitempty"`
	Then  []map[string]interface{} `yaml:"then,omitempty"`

	Key string `yaml:"key,omitempty"`
}

// Catalog is how a collection becomes catalog files, one per group.
type Catalog struct {
	GroupBy string `yaml:"groupBy"`
	Chunk   Chunk  `yaml:"chunk"`
	Render  Render `yaml:"render"`
}

// Chunk is the geometry-budget split described in CatalogGeometryBudget's
// comment in steps.go -- Budget here is that same cap, read from the file
// rather than duplicated as a second constant.
type Chunk struct {
	Budget int    `yaml:"budget"`
	Of     string `yaml:"of"`
	Cost   string `yaml:"cost"`
	Slug   string `yaml:"slug"`
}

// Render is the mapping that turns a chunk into a catalog document.
type Render struct {
	Mapping string            `yaml:"mapping"`
	Local   map[string]string `yaml:"local,omitempty"`
}

// Publish is where catalog files go and what counts as success.
type Publish struct {
	URL            string   `yaml:"url"`
	Concurrency    int      `yaml:"concurrency,omitempty"`
	Timeout        string   `yaml:"timeout,omitempty"`
	Accept         []string `yaml:"accept"`
	TreatAsFailure []string `yaml:"treatAsFailure,omitempty"`
}

// LoadSpec reads and parses the pipeline definition at path inside files.
// It takes an embed.FS rather than a bare path so callers always read the
// copy a binary was built with (see Files in pipeline_files.go), never one
// edited on disk after the fact.
func LoadSpec(files embed.FS, path string) (Spec, error) {
	data, err := files.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("read %s: %w", path, err)
	}

	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return spec, nil
}
