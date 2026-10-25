package schemav2validator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"

	"github.com/getkin/kin-openapi/openapi3"
)

// payload represents the structure of the data payload with context information.
type payload struct {
	Context struct {
		Action string `json:"action"`
	} `json:"context"`
}

// schemav2Validator implements the SchemaValidator interface.
type schemav2Validator struct {
	config          *Config
	specMutex       sync.RWMutex
	specsLoaded     bool                           // true once at least one successful loadAllSpecs has completed
	actionSchemas   map[string]*openapi3.SchemaRef // merged across primary + all auxiliary specs
	bodylessActions map[string]struct{}            // merged across primary + all auxiliary specs
	schemaCache     *schemaCache                   // cache for extended schemas
}

// cachedSpec holds a cached OpenAPI spec.
type cachedSpec struct {
	doc             *openapi3.T
	actionSchemas   map[string]*openapi3.SchemaRef // body operations: action → schema (O(1) lookup)
	bodylessActions map[string]struct{}            // bodyless operations: path without leading slash → exists
	loadedAt        time.Time
}

// AuxSpec holds the type and location for a single auxiliary OpenAPI spec.
type AuxSpec struct {
	Type     string // "url", "file", or "dir"
	Location string // URL, file path, or directory path
}

// Config struct for Schemav2Validator.
type Config struct {
	Type     string // "url", "file", or "dir" — primary spec
	Location string // URL, file path, or directory path — primary spec
	CacheTTL int

	// Auxiliary specs — operator-defined, unsigned, additive only.
	// Actions defined here must not overlap with the primary spec.
	Auxiliary []AuxSpec

	// Extended Schema configuration
	EnableExtendedSchema bool
	ExtendedSchemaConfig ExtendedSchemaConfig
}

// New creates a new Schemav2Validator instance.
func New(ctx context.Context, config *Config) (*schemav2Validator, func() error, error) {
	if config == nil {
		return nil, nil, fmt.Errorf("config cannot be nil")
	}

	// Primary spec is optional — a non-Beckn deployment may use only auxiliary specs.
	// If one of type/location is set, both must be set and type must be valid.
	if config.Type != "" || config.Location != "" {
		if config.Type == "" {
			return nil, nil, fmt.Errorf("config type cannot be empty when location is set")
		}
		if config.Location == "" {
			return nil, nil, fmt.Errorf("config location cannot be empty when type is set")
		}
		if config.Type != "url" && config.Type != "file" && config.Type != "dir" {
			return nil, nil, fmt.Errorf("config type must be 'url', 'file', or 'dir'")
		}
	}

	if config.CacheTTL == 0 {
		config.CacheTTL = 3600
	}

	v := &schemav2Validator{
		config:          config,
		actionSchemas:   make(map[string]*openapi3.SchemaRef),
		bodylessActions: make(map[string]struct{}),
	}

	// Initialize extended schema cache if enabled
	if config.EnableExtendedSchema {
		maxSize := 100
		if config.ExtendedSchemaConfig.MaxCacheSize > 0 {
			maxSize = config.ExtendedSchemaConfig.MaxCacheSize
		}

		v.schemaCache = newSchemaCache(maxSize)

		if p := config.ExtendedSchemaConfig.LocalSchemaPath; p != "" {
			log.Warnf(ctx, "Local schema mode: preloading schemas from %s", p)
			if err := preloadSchemasToCache(ctx, v.schemaCache, p); err != nil {
				return nil, nil, fmt.Errorf("failed to preload schemas: %w", err)
			}
		}

		log.Infof(ctx, "Initialized extended schema cache with max size: %d", maxSize)
	}

	if err := v.initialise(ctx); err != nil {
		return nil, nil, fmt.Errorf("failed to initialise schemav2Validator: %v", err)
	}

	go v.refreshLoop(ctx)

	return v, nil, nil
}

// Validate validates the given data against the OpenAPI schema.
// reqURL carries the Beckn endpoint action in its Path field, already stripped
// of the module base path by the step layer (e.g. "catalog/subscription").
// When the request body is empty (GET/DELETE) it performs an O(1) lookup in
// the pre-built bodylessActions index using reqURL.Path as the key.
// For body-bearing requests the action is extracted from the JSON payload
// and reqURL is not used.
func (v *schemav2Validator) Validate(ctx context.Context, reqURL *url.URL, data []byte) error {
	if len(data) == 0 {
		if reqURL == nil {
			return model.NewBadReqErr("", fmt.Errorf("request URL is required for bodyless validation"))
		}

		v.specMutex.RLock()
		specsLoaded := v.specsLoaded
		bodylessActions := v.bodylessActions
		v.specMutex.RUnlock()

		if !specsLoaded {
			return model.NewBadReqErr("", fmt.Errorf("no OpenAPI spec loaded"))
		}

		// reqURL.Path is the clean endpoint action (e.g. "catalog/subscription"),
		// matching the keys built in buildActionIndex. No further stripping needed.
		// Empty-body POST requests are rejected upstream by validateSchemaStep before
		// reaching here, so only GET/DELETE arrive at this branch.
		action := reqURL.Path
		if _, ok := bodylessActions[action]; !ok {
			return model.NewBadReqErr("", fmt.Errorf("unsupported bodyless request for endpoint: %s", action))
		}

		log.Debugf(ctx, "bodyless request to %s: skipping body schema validation", action)
		return nil
	}

	var payloadData payload
	err := json.Unmarshal(data, &payloadData)
	if err != nil {
		return &model.SchemaValidationErr{Errors: []model.Error{
			*model.NewCodedError("SCH_INVALID_JSON", fmt.Sprintf("failed to parse JSON payload: %v", err)),
		}}
	}

	if payloadData.Context.Action == "" {
		return model.NewBadReqErr("", fmt.Errorf("missing field Action in context"))
	}

	v.specMutex.RLock()
	specsLoaded := v.specsLoaded
	actionSchemas := v.actionSchemas
	v.specMutex.RUnlock()

	if !specsLoaded {
		return model.NewBadReqErr("", fmt.Errorf("no OpenAPI spec loaded"))
	}

	action := payloadData.Context.Action

	// O(1) lookup from merged action index
	schema := actionSchemas[action]
	if schema == nil || schema.Value == nil {
		return model.NewBadReqErr("", fmt.Errorf("unsupported action: %s", action))
	}

	log.Debugf(ctx, "Validating action: %s", action)

	var jsonData any
	if err := json.Unmarshal(data, &jsonData); err != nil {
		return &model.SchemaValidationErr{Errors: []model.Error{
			*model.NewCodedError("SCH_INVALID_JSON", fmt.Sprintf("invalid JSON: %v", err)),
		}}
	}

	opts := []openapi3.SchemaValidationOption{
		openapi3.VisitAsRequest(),
		openapi3.EnableFormatValidation(),
	}
	if err := schema.Value.VisitJSON(jsonData, opts...); err != nil {
		log.Debugf(ctx, "Schema validation failed: %v", err)
		return v.formatValidationError(err)
	}

	log.Debugf(ctx, "base schema validation passed for action: %s", action)

	// Extended Schema validation (if enabled)
	if v.config.EnableExtendedSchema && v.schemaCache != nil {
		log.Debugf(ctx, "Starting Extended Schema validation for action: %s", action)
		if err := v.validateExtendedSchemas(ctx, jsonData, action); err != nil {
			// Extended Schema failure - return error
			log.Debugf(ctx, "Extended Schema validation failed for action %s: %v", action, err)
			return err
		}
		log.Debugf(ctx, "Extended Schema validation passed for action: %s", action)
	}

	return nil
}

// initialise loads all specs (primary + auxiliary) from the configuration.
// Auxiliary load failures are skipped at startup — the adapter starts with whatever loaded.
func (v *schemav2Validator) initialise(ctx context.Context) error {
	return v.loadAllSpecs(ctx, false)
}

// loadAllSpecs loads the primary spec (if configured) and all auxiliary specs,
// merging their action indexes into the validator-level maps.
//
// failOnAuxError controls auxiliary failure handling:
//   - false (startup): a failed auxiliary is skipped and logged; the adapter starts
//     with whatever was successfully loaded.
//   - true (TTL refresh): any auxiliary failure returns an error so the caller
//     retains the previous valid index intact rather than silently dropping
//     the actions that were contributed by the failed spec.
//
// Auxiliary specs may only add new actions — any collision is always a hard error.
func (v *schemav2Validator) loadAllSpecs(ctx context.Context, failOnAuxError bool) error {
	mergedActionSchemas := make(map[string]*openapi3.SchemaRef)
	mergedBodylessActions := make(map[string]struct{})

	hasPrimary := v.config.Type != "" && v.config.Location != ""

	if !hasPrimary {
		log.Warnf(ctx, "schemav2validator: no primary spec configured — operating without a signed trust anchor")
	}

	// Load primary spec first.
	if hasPrimary {
		spec, err := v.loadSingleSpec(ctx, v.config.Type, v.config.Location)
		if err != nil {
			return fmt.Errorf("failed to load primary spec: %w", err)
		}
		for action, schema := range spec.actionSchemas {
			mergedActionSchemas[action] = schema
		}
		for action := range spec.bodylessActions {
			mergedBodylessActions[action] = struct{}{}
		}
		log.Debugf(ctx, "Primary spec loaded: %d body actions, %d bodyless actions", len(spec.actionSchemas), len(spec.bodylessActions))
	}

	// Load each auxiliary spec and merge — hard-reject on action collision.
	for i, aux := range v.config.Auxiliary {
		spec, err := v.loadSingleSpec(ctx, aux.Type, aux.Location)
		if err != nil {
			if failOnAuxError {
				// On TTL refresh: propagate the error so reloadAllSpecs retains
				// the previous valid index rather than committing an index that
				// is missing this auxiliary's actions.
				return fmt.Errorf("auxiliary spec[%d] (%s: %s) failed to reload — retaining previous index: %w",
					i, aux.Type, aux.Location, err)
			}
			log.Errorf(ctx, err, "Failed to load auxiliary spec[%d] (%s: %s) — skipping", i, aux.Type, aux.Location)
			continue
		}
		for action, schema := range spec.actionSchemas {
			if _, exists := mergedActionSchemas[action]; exists {
				return fmt.Errorf("auxiliary spec[%d] (%s) defines action %q which is already defined in a previously loaded spec — auxiliary specs may only add new actions",
					i, aux.Location, action)
			}
			mergedActionSchemas[action] = schema
		}
		for action := range spec.bodylessActions {
			if _, exists := mergedBodylessActions[action]; exists {
				return fmt.Errorf("auxiliary spec[%d] (%s) defines bodyless action %q which is already defined in a previously loaded spec — auxiliary specs may only add new actions",
					i, aux.Location, action)
			}
			mergedBodylessActions[action] = struct{}{}
		}
		log.Debugf(ctx, "Auxiliary spec[%d] loaded from %s: %d body actions, %d bodyless actions", i, aux.Location, len(spec.actionSchemas), len(spec.bodylessActions))
	}

	if len(mergedActionSchemas) == 0 && len(mergedBodylessActions) == 0 {
		return fmt.Errorf("schemav2validator: no actions indexed after loading all specs — configure at least one valid primary or auxiliary spec")
	}

	v.specMutex.Lock()
	v.specsLoaded = true
	v.actionSchemas = mergedActionSchemas
	v.bodylessActions = mergedBodylessActions
	v.specMutex.Unlock()

	log.Debugf(ctx, "schemav2validator: merged index ready — %d body actions, %d bodyless actions",
		len(mergedActionSchemas), len(mergedBodylessActions))
	return nil
}

// specHTTPClient is used by freshReadFromURI for all remote spec fetches.
// The 30 s timeout prevents a hanging server from stalling the reload goroutine
// indefinitely; caller context deadlines further constrain it when set.
var specHTTPClient = &http.Client{Timeout: 30 * time.Second}

// maxSpecBodyBytes caps how much of a spec response is read into memory.
// No legitimate OpenAPI spec approaches this limit; it guards against
// misconfigured proxies and adversarial servers sending arbitrarily large bodies.
var maxSpecBodyBytes int64 = 32 * 1024 * 1024 // 32 MB

// freshReadFromURI reads bytes directly from disk or network, bypassing the
// kin-openapi package-level URIMapCache so TTL reloads always fetch current content.
func freshReadFromURI(loader *openapi3.Loader, u *url.URL) ([]byte, error) {
	switch u.Scheme {
	case "http", "https":
		ctx := context.Background()
		if loader.Context != nil {
			ctx = loader.Context
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := specHTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, u)
		}
		limited := io.LimitReader(resp.Body, maxSpecBodyBytes+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxSpecBodyBytes {
			return nil, fmt.Errorf("spec response exceeds %d MB limit", maxSpecBodyBytes/(1024*1024))
		}
		return data, nil
	default: // "file" scheme or bare path
		return os.ReadFile(u.Path)
	}
}

// newFreshLoader returns a loader whose ReadFromURIFunc bypasses the global
// URIMapCache. Use this everywhere instead of openapi3.NewLoader() so that
// TTL-driven reloads always read the current file or remote content.
//
// Note: setting ReadFromURIFunc causes kin-openapi to skip its own
// IsExternalRefsAllowed guard (the custom function becomes the sole access
// policy). IsExternalRefsAllowed=true is kept for documentation intent but
// has no runtime effect once ReadFromURIFunc is non-nil.
func newFreshLoader() *openapi3.Loader {
	l := openapi3.NewLoader()
	l.IsExternalRefsAllowed = true
	l.ReadFromURIFunc = freshReadFromURI
	return l
}

// loadSingleSpec loads one OpenAPI document from the given type and location.
// For type "dir", all top-level *.yaml and *.json files are loaded and their
// action indexes are merged into a single cachedSpec.
func (v *schemav2Validator) loadSingleSpec(ctx context.Context, specType, location string) (*cachedSpec, error) {
	if specType == "dir" {
		return v.loadSpecFromDir(ctx, location)
	}

	loader := newFreshLoader()
	loader.Context = ctx

	var doc *openapi3.T
	var err error

	switch specType {
	case "url":
		u, parseErr := url.Parse(location)
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse URL: %v", parseErr)
		}
		doc, err = loader.LoadFromURI(u)
	case "file":
		doc, err = loader.LoadFromFile(location)
	default:
		return nil, fmt.Errorf("unsupported type: %s", specType)
	}

	if err != nil {
		log.Errorf(ctx, err, "Failed to load from %s: %s", specType, location)
		return nil, fmt.Errorf("failed to load OpenAPI document: %v", err)
	}

	if err := doc.Validate(ctx); err != nil {
		log.Debugf(ctx, "Spec validation warnings (non-fatal) for %s: %v", location, err)
	}

	actionSchemas, bodylessActions := v.buildActionIndex(ctx, doc)

	log.Debugf(ctx, "Loaded spec from %s: %s — %d body actions, %d bodyless actions",
		specType, location, len(actionSchemas), len(bodylessActions))

	return &cachedSpec{
		doc:             doc,
		actionSchemas:   actionSchemas,
		bodylessActions: bodylessActions,
		loadedAt:        time.Now(),
	}, nil
}

// loadSpecFromDir loads all top-level *.yaml and *.json files in the given
// directory as independent specs and merges their action indexes.
func (v *schemav2Validator) loadSpecFromDir(ctx context.Context, dir string) (*cachedSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	merged := &cachedSpec{
		actionSchemas:   make(map[string]*openapi3.SchemaRef),
		bodylessActions: make(map[string]struct{}),
		loadedAt:        time.Now(),
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".json") {
			continue
		}
		filePath := filepath.Join(dir, name)
		spec, err := v.loadSingleSpec(ctx, "file", filePath)
		if err != nil {
			log.Errorf(ctx, err, "Skipping file in dir spec: %s", filePath)
			continue
		}
		for action, schema := range spec.actionSchemas {
			if _, exists := merged.actionSchemas[action]; exists {
				return nil, fmt.Errorf("dir spec %q: action %q defined in multiple files — last seen in %s; remove the duplicate", dir, action, filePath)
			}
			merged.actionSchemas[action] = schema
		}
		for action := range spec.bodylessActions {
			if _, exists := merged.bodylessActions[action]; exists {
				return nil, fmt.Errorf("dir spec %q: bodyless action %q defined in multiple files — last seen in %s; remove the duplicate", dir, action, filePath)
			}
			merged.bodylessActions[action] = struct{}{}
		}
		loaded++
	}

	if loaded == 0 {
		log.Warnf(ctx, "No valid OpenAPI files found in directory: %s", dir)
	}

	return merged, nil
}

// refreshLoop periodically reloads all specs based on TTL.
func (v *schemav2Validator) refreshLoop(ctx context.Context) {
	coreTicker := time.NewTicker(time.Duration(v.config.CacheTTL) * time.Second)
	defer coreTicker.Stop()

	// Ticker for extended schema cleanup
	var refTicker *time.Ticker
	var refTickerCh <-chan time.Time // Default nil, blocks forever

	if v.config.EnableExtendedSchema {
		ttl := v.config.ExtendedSchemaConfig.CacheTTL
		if ttl <= 0 {
			ttl = 86400 // Default 24 hours
		}
		refTicker = time.NewTicker(time.Duration(ttl) * time.Second)
		defer refTicker.Stop()
		refTickerCh = refTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-coreTicker.C:
			v.reloadAllSpecs(ctx)
		case <-refTickerCh:
			if v.schemaCache != nil {
				count := v.schemaCache.cleanupExpired()
				if count > 0 {
					log.Debugf(ctx, "Cleaned up %d expired extended schemas", count)
				}
			}
		}
	}
}

// reloadAllSpecs rebuilds the merged action index from all specs on TTL expiry.
// Any failure (primary, auxiliary, or collision) causes the previous valid index
// to be retained — the new index is only committed when all specs load cleanly.
func (v *schemav2Validator) reloadAllSpecs(ctx context.Context) {
	if err := v.loadAllSpecs(ctx, true); err != nil {
		log.Errorf(ctx, err, "Failed to reload specs — serving stale action index until next TTL cycle")
	} else {
		log.Debugf(ctx, "Reloaded all specs successfully")
	}
}

// formatValidationError converts kin-openapi validation errors to ONIX error format.
func (v *schemav2Validator) formatValidationError(err error) error {
	var schemaErrors []model.Error

	// Check if it's a MultiError (collection of errors)
	if multiErr, ok := err.(openapi3.MultiError); ok {
		for _, e := range multiErr {
			v.extractSchemaErrors(e, &schemaErrors)
		}
	} else {
		v.extractSchemaErrors(err, &schemaErrors)
	}

	return &model.SchemaValidationErr{Errors: schemaErrors}
}

// schemaFieldCodes maps an openapi3.SchemaError's SchemaField (the JSON-schema
// keyword that failed) to the corresponding Beckn v2.0.0 SCH_* code.
var schemaFieldCodes = map[string]string{
	"required": "SCH_REQUIRED_FIELD_MISSING",
	// kin-openapi uses SchemaField "properties" specifically for the
	// additionalProperties-disallowed case ("property %q is unsupported").
	"properties": "SCH_FIELD_NOT_ALLOWED",
	"enum":       "SCH_INVALID_ENUM",
	// "const" is JSON Schema's own sugar for an enum with a single allowed
	// value — there is no dedicated SCH_* code for it, so it shares enum's.
	"const":  "SCH_INVALID_ENUM",
	"format": "SCH_INVALID_FORMAT",
	"type":   "SCH_TYPE_NOT_SUPPORTED",
}

// schemaFieldToCode looks up schemaFieldCodes. Composite keywords
// (oneOf/anyOf/allOf) and any constraint without a dedicated code fall back
// to SCH_SCHEMA_VALIDATION_FAILED, since kin-openapi doesn't attribute those
// to one single underlying cause.
func schemaFieldToCode(schemaField string) string {
	if code, ok := schemaFieldCodes[schemaField]; ok {
		return code
	}
	return "SCH_SCHEMA_VALIDATION_FAILED"
}

// extractSchemaErrors recursively extracts detailed error information from SchemaError.
func (v *schemav2Validator) extractSchemaErrors(err error, schemaErrors *[]model.Error) {
	if becknErr, ok := err.(*model.Error); ok {
		// Already a fully-classified error (e.g. from validateReferencedObject's
		// domain/JSON-LD checks) — pass it through as-is.
		*schemaErrors = append(*schemaErrors, *becknErr)
	} else if schemaErr, ok := err.(*openapi3.SchemaError); ok {
		// A composite keyword (allOf/oneOf) carries its branches' real causes
		// under Origin. Report those instead of the composite itself: they
		// name the field and carry the keyword that actually failed, so the
		// caller is told "property X is missing" rather than that some
		// unnamed branch did not match.
		if leaves := schemaErrorLeaves(schemaErr); len(leaves) > 0 {
			for _, leaf := range rankSchemaLeaves(leaves) {
				*schemaErrors = append(*schemaErrors, *codedSchemaError(leaf))
			}
			return
		}
		// No reachable cause — anyOf and not discard theirs, so the composite
		// error is all there is to report.
		*schemaErrors = append(*schemaErrors, *codedSchemaError(schemaErr))
	} else if multiErr, ok := err.(openapi3.MultiError); ok {
		// Nested MultiError
		for _, e := range multiErr {
			v.extractSchemaErrors(e, schemaErrors)
		}
	} else {
		// Generic error — no schema keyword to classify against.
		*schemaErrors = append(*schemaErrors, *model.NewCodedError("SCH_SCHEMA_VALIDATION_FAILED", err.Error()))
	}
}

// discriminatorSchemaFields are the keywords a schema uses to say which of
// several alternatives a payload is for: a const or an enum pinning a scalar
// to one value. Inside a failed composite they usually mean "this branch is
// not the one" rather than "this value is wrong", so they are reported after
// the causes a caller can act on.
var discriminatorSchemaFields = map[string]bool{"enum": true, "const": true}

// guardSchemaFields is the narrower keyword that rules an alternative out
// altogether, as opposed to merely ranking its causes low.
//
// Every pack on the network writes a guard the same way: a const pinning the
// one value that says this alternative applies, kept as its own conjunct so
// the rest of the alternative is read only once the guard holds. Ordinary
// value constraints use an enum -- an information mode, a season, a list of
// category codes. Reading an enum as a guard confuses the two, and a payload
// whose only fault was an enum field then looked like a payload meant for
// some other alternative entirely: a PMFBY catalog with a numeric category
// code was answered with PM-KISAN's complaints about its scheme code.
var guardSchemaFields = map[string]bool{"const": true}

// foreignFieldSchemaFields is the keyword that fires when a closed branch is
// shown a property it does not declare. Among branches of a disjunction that
// is the weakest cause of all: a payload for one alternative carries the
// fields of that alternative, so every other branch reports each of them as
// unsupported whatever is actually wrong with the payload. Reporting those
// last keeps a sibling branch from answering "applicantMobile is unsupported"
// to a payload whose only fault is a misspelt season.
//
// It still ranks ahead of nothing when it is the whole story: a payload with a
// genuinely unknown field produces this and only this, and order among equals
// is unchanged, so the unknown field is still what the caller is told.
var foreignFieldSchemaFields = map[string]bool{"properties": true}

// rankSchemaLeaves puts the actionable causes first while keeping every one of
// them. Only the order changes: a SchemaValidationErr reports all its causes,
// but takes its single wire-level code from the first, so a missing property
// should outrank the enum on whichever guard the payload did not satisfy, and
// both should outrank a sibling branch calling their fields unsupported.
//
// This is a presentation heuristic, not a judgement about which branch of a
// failed oneOf the caller meant. That cannot be recovered here: kin-openapi
// evaluates a schema's set operations before its properties and returns as
// soon as the set operations fail, so a branch's discriminating property
// never gets evaluated and never appears in the tree to be matched against.
func rankSchemaLeaves(leaves []*openapi3.SchemaError) []*openapi3.SchemaError {
	ranked := make([]*openapi3.SchemaError, 0, len(leaves))
	for _, tier := range []func(*openapi3.SchemaError) bool{
		func(l *openapi3.SchemaError) bool {
			return !discriminatorSchemaFields[l.SchemaField] && !foreignFieldSchemaFields[l.SchemaField]
		},
		func(l *openapi3.SchemaError) bool { return discriminatorSchemaFields[l.SchemaField] },
		func(l *openapi3.SchemaError) bool { return foreignFieldSchemaFields[l.SchemaField] },
	} {
		for _, leaf := range leaves {
			if tier(leaf) {
				ranked = append(ranked, leaf)
			}
		}
	}
	return ranked
}

// codedSchemaError renders one openapi3.SchemaError as a model.Error, taking
// the taxonomy code from the JSON-schema keyword that failed and the path from
// the error's own JSON pointer.
func codedSchemaError(se *openapi3.SchemaError) *model.Error {
	path := strings.Join(se.JSONPointer(), "/")
	if path == "" {
		path = se.SchemaField
	}
	// SchemaError.Error() appends a full schema-and-value dump after the
	// reason, which is what made a NACK body unreadable. Reason alone is the
	// sentence worth returning; "not" is the one keyword that leaves it empty.
	message := se.Reason
	if message == "" {
		// "not" is the one keyword kin-openapi leaves without a reason: it
		// fires when the subschema it negates matched, and a match has
		// nothing to say.
		message = "value is not allowed here"
		if se.SchemaField != "not" {
			message = fmt.Sprintf("value does not satisfy %q", se.SchemaField)
		}
	}
	errItem := model.NewCodedError(schemaFieldToCode(se.SchemaField), message)
	if path != "" {
		errItem.Details = &model.ErrorDetails{Path: path}
	}
	return errItem
}

// compositeSchemaFields are the keywords that report only that some branch
// failed. They are never the cause a caller can act on, so a leaf walk steps
// through them to whatever they wrap.
//
// "not" is absent deliberately. A failed "not" has nothing underneath it by
// construction -- the subschema it negates SUCCEEDED, so there is no inner
// error -- but it is a cause in its own right: something is present that may
// not be, and the pointer says what. Stepping through it reaches nothing and
// loses the only thing the schema said.
var compositeSchemaFields = map[string]bool{
	"allOf": true, "anyOf": true, "oneOf": true,
}

// disjunctiveSchemaFields are the composites whose branches are alternatives
// rather than conjuncts: one of them was meant to match, and the rest are
// shapes the payload was never claiming to be.
var disjunctiveSchemaFields = map[string]bool{"oneOf": true, "anyOf": true}

// schemaErrorLeaves walks a kin-openapi error tree and returns the concrete
// assertion failures at its leaves, in encounter order and deduplicated.
//
// Two shapes have to be stepped through to reach them. kin-openapi stores a
// composite's nested causes as Origin = fmt.Errorf("...: %w", multiErr), so
// the slice of per-branch errors sits one errors.Unwrap below Origin; and
// those multierror types are named slice types rather than openapi3.MultiError
// itself, so they are walked by reflection over the slice. Reading
// Origin.Error() as text instead of walking it is what loses the per-branch
// attribution: the branches' messages are concatenated, and picking one out of
// the resulting string selects by position rather than by relevance.
//
// anyOf and not are returned empty: kin-openapi discards their branch errors
// before the caller ever sees them, so there is nothing underneath to find.
func schemaErrorLeaves(err error) []*openapi3.SchemaError {
	se, ok := err.(*openapi3.SchemaError)
	if !ok || se.Origin == nil {
		return nil
	}
	// Start below the composite itself, so a composite with no reachable
	// cause yields nothing and the caller falls back to reporting it.
	return dedupeSchemaLeaves(leavesUnder(se.Origin, disjunctiveSchemaFields[se.SchemaField]))
}

// leavesUnder returns the reportable leaves under one node of the tree.
// alternatives says whether e holds the branches of a oneOf or an anyOf, in
// which case the ones the schema ruled out are dropped; otherwise its members
// are conjuncts and every one of them is a cause in its own right.
func leavesUnder(e error, alternatives bool) []*openapi3.SchemaError {
	if e == nil {
		return nil
	}
	if se, ok := e.(*openapi3.SchemaError); ok {
		if se.Origin != nil {
			// Something more specific underneath; the composite that wrapped
			// it adds nothing.
			if sub := leavesUnder(se.Origin, disjunctiveSchemaFields[se.SchemaField]); len(sub) > 0 {
				return sub
			}
		}
		if compositeSchemaFields[se.SchemaField] {
			return nil
		}
		// A "not" that fired on the object itself rather than on one of its
		// members has no pointer and no reason, so it says only that
		// something somewhere is wrong. Dropping it lets the caller fall
		// back to whatever else the tree holds.
		if se.SchemaField == "not" && len(se.JSONPointer()) == 0 {
			return nil
		}
		return []*openapi3.SchemaError{se}
	}
	if branches, ok := errorBranches(e); ok {
		every := make([][]*openapi3.SchemaError, 0, len(branches))
		ruledOut := make([]bool, 0, len(branches))
		allRuledOut := alternatives
		for _, branch := range branches {
			every = append(every, leavesUnder(branch, false))
			out := alternatives && discriminatedAway(branch)
			ruledOut = append(ruledOut, out)
			allRuledOut = allRuledOut && out
		}
		// An alternative still standing on its own terms is the one the
		// caller was aiming at, so the alternatives their own pinned values
		// ruled out have nothing to add -- whatever else those happened to
		// report. Only when every alternative was ruled out is what ruled
		// them out worth reporting, and then only from the ones that failed
		// on a pinned value and nothing else: such an alternative is one the
		// caller all but sent, so the pinned value is the field they got
		// wrong. A season outside its enum is the fault. An alternative that
		// wants more besides is the other reading, and reporting what it
		// wants sends the caller to fill in fields that are not theirs.
		var kept [][]*openapi3.SchemaError
		for i, leaves := range every {
			if !ruledOut[i] || (allRuledOut && onlyDiscriminators(leaves)) {
				kept = append(kept, leaves)
			}
		}
		// Every alternative was ruled out, so none of them tells the caller
		// which was meant. Most of what they said is about shapes the payload
		// was never going to be; what ruled each one out is the whole of the
		// news, so that is what is reported when there is any.
		if len(kept) == 0 {
			if kept = discriminatorsOf(every); len(kept) == 0 {
				kept = every
			}
		}
		if alternatives {
			kept = nearest(kept)
		}
		return flatten(kept)
	}
	if u := errors.Unwrap(e); u != nil {
		return leavesUnder(u, alternatives)
	}
	return nil
}

// discriminatedAway reports whether an alternative was ruled out by its own
// terms rather than by a fault in the payload: a const or an enum pinning the
// value that says which alternative applies. The rest of such a branch is
// about a shape the caller never claimed to be sending, and reporting it sends
// them to fix a field that is not theirs -- a PMFBY support call was told to
// supply PM-KISAN's challenge, on a branch its scheme code had already
// excluded.
//
// A const or an enum only: the payload carries the value, so the schema is
// saying which alternative it is. A tripped "not" is NOT read this way even
// though an alternative can be guarded with one, because the same keyword is
// how every pack says a field is not allowed here -- reading those as
// discrimination silences the arm that was actually meant.
//
// Only the alternative's own conjuncts count. The search stops at a nested
// oneOf or anyOf: a discriminator inside one of *its* alternatives rules that
// one out, not the branch that contains the choice.
func discriminatedAway(e error) bool {
	if e == nil {
		return false
	}
	if se, ok := e.(*openapi3.SchemaError); ok {
		if disjunctiveSchemaFields[se.SchemaField] {
			return false
		}
		if guardSchemaFields[se.SchemaField] {
			return true
		}
		if se.Origin == nil {
			return false
		}
		e = se.Origin
	}
	if branches, ok := errorBranches(e); ok {
		for _, branch := range branches {
			if discriminatedAway(branch) {
				return true
			}
		}
		return false
	}
	if u := errors.Unwrap(e); u != nil {
		return discriminatedAway(u)
	}
	return false
}

// onlyDiscriminators reports whether an alternative failed on its pinned
// values and on nothing else.
func onlyDiscriminators(leaves []*openapi3.SchemaError) bool {
	if len(leaves) == 0 {
		return false
	}
	for _, leaf := range leaves {
		if !discriminatorSchemaFields[leaf.SchemaField] {
			return false
		}
	}
	return true
}

// errorBranches returns the members of a multierror. openapi3.MultiError and
// the unexported multiErrorForOneOf / multiErrorForAllOf are all []error
// underneath but share no interface, so the slice is read by reflection.
func errorBranches(e error) ([]error, bool) {
	v := reflect.ValueOf(e)
	if v.Kind() != reflect.Slice {
		return nil, false
	}
	branches := make([]error, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		if sub, ok := v.Index(i).Interface().(error); ok {
			branches = append(branches, sub)
		}
	}
	return branches, true
}

// discriminatorsOf keeps, from each alternative, only what said the
// alternative was not the one -- dropping the demands it would have made had
// it been.
func discriminatorsOf(arms [][]*openapi3.SchemaError) [][]*openapi3.SchemaError {
	var out [][]*openapi3.SchemaError
	for _, arm := range arms {
		var why []*openapi3.SchemaError
		for _, leaf := range arm {
			if discriminatorSchemaFields[leaf.SchemaField] {
				why = append(why, leaf)
			}
		}
		if len(why) > 0 {
			out = append(out, why)
		}
	}
	return out
}

// nearest drops the alternatives that ask for everything another one asks for
// and more.
//
// All of them failed, so the caller meant one of them and the schema cannot
// say which. The one that failed for fewest reasons is the one they came
// closest to sending; the extra demands of the others hold only if a different
// alternative was meant. Reporting those is telling a caller to add a field
// that may well not be theirs to send, next to one that certainly is.
//
// An alternative with no reachable cause rules nothing out -- it is a gap in
// what the validator reported, not an easier way to satisfy the schema.
func nearest(arms [][]*openapi3.SchemaError) [][]*openapi3.SchemaError {
	demands := make([]map[string]bool, len(arms))
	for i, arm := range arms {
		demands[i] = make(map[string]bool, len(arm))
		for _, leaf := range arm {
			demands[i][leafKey(leaf)] = true
		}
	}
	var out [][]*openapi3.SchemaError
	for i, arm := range arms {
		if !anyNearerThan(demands, i) {
			out = append(out, arm)
		}
	}
	return out
}

// anyNearerThan reports whether some other alternative failed for a strict
// subset of alternative i's reasons.
func anyNearerThan(demands []map[string]bool, i int) bool {
	for j, other := range demands {
		if j == i || len(other) == 0 || len(other) >= len(demands[i]) {
			continue
		}
		contained := true
		for key := range other {
			if !demands[i][key] {
				contained = false
				break
			}
		}
		if contained {
			return true
		}
	}
	return false
}

// flatten joins the alternatives' leaves back into one list, in branch order.
func flatten(arms [][]*openapi3.SchemaError) []*openapi3.SchemaError {
	var out []*openapi3.SchemaError
	for _, arm := range arms {
		out = append(out, arm...)
	}
	return out
}

// leafKey identifies a leaf by what it says, so the same complaint reached by
// two routes counts once.
func leafKey(leaf *openapi3.SchemaError) string {
	return leaf.SchemaField + "\x00" + strings.Join(leaf.JSONPointer(), "/") + "\x00" + leaf.Reason
}

// dedupeSchemaLeaves drops leaves that repeat an earlier one, keeping the
// first. Alternatives of the same gate routinely fail the same way.
func dedupeSchemaLeaves(leaves []*openapi3.SchemaError) []*openapi3.SchemaError {
	out := make([]*openapi3.SchemaError, 0, len(leaves))
	seen := map[string]bool{}
	for _, leaf := range leaves {
		key := leafKey(leaf)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, leaf)
	}
	return out
}

// buildActionIndex builds two indexes from the loaded OpenAPI spec:
//   - actionSchemas: body-bearing operations (POST/PUT/PATCH) keyed by context.action value
//   - bodylessActions: GET/DELETE operations without a requestBody, keyed by path without leading slash
//
// Both are built in a single pass over the spec paths for efficiency.
func (v *schemav2Validator) buildActionIndex(ctx context.Context, doc *openapi3.T) (map[string]*openapi3.SchemaRef, map[string]struct{}) {
	actionSchemas := make(map[string]*openapi3.SchemaRef)
	bodylessActions := make(map[string]struct{})

	for specPath, item := range doc.Paths.Map() {
		if item == nil {
			continue
		}

		// Pass 1: body-bearing operations (any verb that declares a requestBody).
		// All five standard verbs are checked so that unusual specs (e.g. GET with
		// body) are still indexed correctly.
		for _, op := range []*openapi3.Operation{item.Post, item.Get, item.Put, item.Patch, item.Delete} {
			if op == nil || op.RequestBody == nil || op.RequestBody.Value == nil {
				continue
			}
			content := op.RequestBody.Value.Content.Get("application/json")
			if content == nil || content.Schema == nil || content.Schema.Value == nil {
				continue
			}
			action := v.extractActionFromSchema(content.Schema.Value)
			if action != "" {
				actionSchemas[action] = content.Schema
				log.Debugf(ctx, "Indexed body action '%s' from path %s", action, specPath)
			}
		}

		// Pass 2: bodyless operations — GET and DELETE that declare no requestBody.
		// A separate loop (rather than sharing pass 1) keeps the two indexing concerns
		// independent and avoids complicating the guard logic for body-bearing ops.
		// Key is the spec path without its leading slash so it matches
		// the endpointAction passed to Validate's bodyless branch.
		for _, op := range []*openapi3.Operation{item.Get, item.Delete} {
			if op == nil || op.RequestBody != nil {
				continue
			}
			key := strings.TrimPrefix(specPath, "/")
			bodylessActions[key] = struct{}{}
			log.Debugf(ctx, "Indexed bodyless action '%s' from path %s", key, specPath)
		}
	}

	return actionSchemas, bodylessActions
}

// extractActionFromSchema extracts the action value from a schema.
func (v *schemav2Validator) extractActionFromSchema(schema *openapi3.Schema) string {
	// Check direct properties
	if ctxProp := schema.Properties["context"]; ctxProp != nil && ctxProp.Value != nil {
		if action := v.getActionValue(ctxProp.Value); action != "" {
			return action
		}
	}

	// Check allOf at schema level
	for _, allOfSchema := range schema.AllOf {
		if allOfSchema.Value != nil {
			if ctxProp := allOfSchema.Value.Properties["context"]; ctxProp != nil && ctxProp.Value != nil {
				if action := v.getActionValue(ctxProp.Value); action != "" {
					return action
				}
			}
		}
	}

	return ""
}

// getActionValue extracts action value from context schema.
func (v *schemav2Validator) getActionValue(contextSchema *openapi3.Schema) string {
	if actionProp := contextSchema.Properties["action"]; actionProp != nil && actionProp.Value != nil {
		// Native OpenAPI 3.1 const (kin-openapi >= v0.137 parses this into the typed field).
		if action, ok := actionProp.Value.Const.(string); ok && action != "" {
			return action
		}
		// Fallback: older kin-openapi versions surfaced const via Extensions.
		if constVal, ok := actionProp.Value.Extensions["const"]; ok {
			if action, ok := constVal.(string); ok {
				return action
			}
		}
		// Check enum field (return first value)
		if len(actionProp.Value.Enum) > 0 {
			if action, ok := actionProp.Value.Enum[0].(string); ok {
				return action
			}
		}
	}

	// Check allOf in context
	for _, allOfSchema := range contextSchema.AllOf {
		if allOfSchema.Value != nil {
			if action := v.getActionValue(allOfSchema.Value); action != "" {
				return action
			}
		}
	}

	return ""
}
