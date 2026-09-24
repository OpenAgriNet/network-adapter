package pipeline

// build.go runs the `catalog:` block: the part of a pipeline file that turns
// one flat collection of records into the catalogue documents a run publishes.
//
// It is the generalisation of MandiPrice's catalog.go, whose behaviour it
// preserves rule for rule -- the geometry budget, the two exclusions, the
// annotation, the refusal to publish an empty group -- with every one of them
// read from the file instead of written in Go. Nothing here knows what a
// mandi, a market or a state is: a second capability gets a catalogue by
// writing a `catalog:` block and no Go at all.
//
// The order is the order the file reads in, and it is not interchangeable:
//
//	groupBy    one catalogue per group
//	exclude    drop a record, with a stated reason, from the catalogue entirely
//	annotate   label a record that still publishes
//	order      make two runs of one collection read the same way
//	chunk      split a group that carries more than a catalogue may hold
//	identity   the ids the document and its resources publish under
//	render     the mapping that turns a chunk into the document
//
// `output:` is deliberately NOT read here. A built catalogue is a value; where
// it lands on disk is run.go's business (see WriteCatalogues), and reading the
// same block in two places is how the two drift apart.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// Counter names the builder reports under. Prefixed keys carry the file's own
// wording -- "excluded:coordinate missing" -- because a bare total tells an
// operator that something was dropped but never which rule dropped it.
const (
	catalogCounterGroups     = "groups"
	catalogCounterEmpty      = "emptyGroups"
	catalogCounterCatalogues = "catalogues"
	catalogCounterPublished  = "published"
	catalogCounterExcluded   = "excluded"

	catalogExcludedPrefix  = "excluded:"
	catalogAnnotatedPrefix = "annotated:"
)

// catalogResourceField is where identity.resourceId lands on each record
// handed to the render mapping.
//
// The template has to DO something. A file that states the id its resources
// publish under, while the mapping quietly builds its own, is the failure
// spec.go's header warns about: a rule believed to be in force that no code
// ever reads.
const catalogResourceField = "resourceId"

// buildCatalogues applies a file's `catalog:` block to a collection.
//
// rc supplies everything a `${...}` outside a record can name (the resolved
// inputs, the built-ins); cache compiles and applies the JSONata that reaches
// inside one. mappingBase is the root the mappings are served from, as
// steps.go's mappingRef uses it.
//
// The counters are the domain's own report numbers, handed back rather than
// logged so a collector can put them in CollectResult.Counters.
func buildCatalogues(ctx context.Context, catalog Catalog, records []map[string]any,
	rc *runContext, cache *exprCache, mapper Mapper, mappingBase string) ([]Catalogue, map[string]int, error) {

	counters := map[string]int{}

	if catalog.GroupBy == "" {
		return nil, counters, fmt.Errorf("the catalog block names no groupBy field")
	}
	if catalog.Render.Mapping == "" {
		return nil, counters, fmt.Errorf("the catalog block names no render mapping")
	}
	if catalog.Identity.CatalogID == "" {
		return nil, counters, fmt.Errorf("the catalog block names no identity.catalogId: a catalogue with no id cannot be published")
	}
	if err := catalogCheckDirection(catalog.Order.Direction); err != nil {
		return nil, counters, err
	}

	keys, groups, err := catalogGroup(records, catalog.GroupBy)
	if err != nil {
		return nil, counters, err
	}
	counters[catalogCounterGroups] = len(keys)

	var built []Catalogue
	named := map[string]bool{}

	for _, key := range keys {
		members := groups[key]

		// The group's own values, reachable as ${group.…}: for each field, the
		// first value any member carries that is not blank. Taken from the
		// first member alone, a descriptor built from ${group.stateName} would
		// be named after whichever row happened to sort first, blank included.
		scope := rc.with(catalog.GroupBy, key).with("group", catalogGroupValues(members))

		publishable, err := catalogSelect(catalog, members, scope, cache, counters)
		if err != nil {
			return nil, counters, fmt.Errorf("group %q: %w", key, err)
		}

		// A group left with nothing publishable produces NO catalogue, not an
		// empty one. An empty catalogue is not "no news": publishing it
		// retires that group's resources from the network on the next MERGE.
		if len(publishable) == 0 {
			counters[catalogCounterEmpty]++
			continue
		}

		if catalog.Order.By != "" {
			sortRecords(publishable, catalog.Order.By, catalog.Order.Direction)
		}

		chunks, err := catalogChunk(publishable, catalog.Chunk, cache)
		if err != nil {
			return nil, counters, fmt.Errorf("group %q: %w", key, err)
		}

		for index, chunk := range chunks {
			catalogue, err := catalogRender(ctx, catalog, chunk, index+1, key, scope, cache, mapper, mappingBase)
			if err != nil {
				return nil, counters, fmt.Errorf("group %q: %w", key, err)
			}
			// Two catalogues under one slug are two catalogues under one
			// catalogId: the second MERGE would replace the first one's
			// resources instead of adding to them, and the second file would
			// overwrite the first on disk.
			if named[catalogue.Slug] {
				return nil, counters, fmt.Errorf("group %q: chunk %d renders the slug %q a second time; the chunk slug template does not tell chunks apart",
					key, index+1, catalogue.Slug)
			}
			named[catalogue.Slug] = true

			built = append(built, catalogue)
			counters[catalogCounterCatalogues]++
			counters[catalogCounterPublished] += len(chunk)
		}
	}

	return built, counters, nil
}

// catalogGroup splits the collection on one field, returning the keys in
// sorted order so a run partitions the same way twice.
//
// A record that does not carry the field is an ERROR. Grouped under the empty
// key it would publish as a catalogue named after nothing, and a mistyped
// groupBy would collapse an entire collection into one such catalogue.
func catalogGroup(records []map[string]any, by string) ([]string, map[string][]map[string]any, error) {
	groups := map[string][]map[string]any{}
	var keys []string

	for i, record := range records {
		value, ok := record[by]
		if !ok {
			return nil, nil, fmt.Errorf("record %d has no %q to group by", i, by)
		}
		key := renderScalar(value)
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], record)
	}

	sort.Strings(keys)
	return keys, groups, nil
}

// catalogGroupValues is what ${group.…} resolves against: for every field any
// member carries, the first non-blank value in collection order.
func catalogGroupValues(members []map[string]any) map[string]any {
	values := map[string]any{}
	for _, member := range members {
		for field, value := range member {
			if existing, seen := values[field]; seen && !catalogBlank(existing) {
				continue
			}
			values[field] = value
		}
	}
	return values
}

func catalogBlank(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	default:
		return false
	}
}

// catalogSelect applies the exclusions and then the annotations, in that
// order: a record kept out of the catalogue is not a record to label.
//
// The records it returns are COPIES. An annotation writing onto the caller's
// collection would leave the previous run's labels on a reused record.
func catalogSelect(catalog Catalog, members []map[string]any, scope *runContext,
	cache *exprCache, counters map[string]int) ([]map[string]any, error) {

	var publishable []map[string]any

	for _, record := range members {
		excluded, err := catalogExcluded(catalog.Exclude, record, scope, cache, counters)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}

		kept := cloneRecord(record)
		for i, rule := range catalog.Annotate {
			if rule.As == "" {
				return nil, fmt.Errorf("annotate rule %d says no `as:` field to set", i)
			}
			if rule.When == "" {
				return nil, fmt.Errorf("annotate rule %d has no `when:`", i)
			}
			// Evaluated against the record as it arrived, so one rule cannot
			// fire because an earlier one just labelled the record.
			match, err := catalogMatches(cache, scope, rule.When, record)
			if err != nil {
				return nil, fmt.Errorf("annotate rule %d: %w", i, err)
			}
			if match {
				kept[rule.As] = true
				counters[catalogAnnotatedPrefix+rule.As]++
			}
		}
		publishable = append(publishable, kept)
	}

	return publishable, nil
}

// catalogExcluded reports whether any exclusion rule claims this record, and
// counts the claim under the rule's own reason.
func catalogExcluded(rules []ExcludeRule, record map[string]any, scope *runContext,
	cache *exprCache, counters map[string]int) (bool, error) {

	for i, rule := range rules {
		if rule.When == "" {
			return false, fmt.Errorf("exclude rule %d has no `when:`", i)
		}
		if rule.Reason == "" {
			// Every excluded record is reported with a stated reason. A
			// silent exclusion is a resource that vanishes from the network
			// and is indistinguishable from one that never existed.
			return false, fmt.Errorf("exclude rule %d states no `reason:`", i)
		}
		match, err := catalogMatches(cache, scope, rule.When, record)
		if err != nil {
			return false, fmt.Errorf("exclude rule %d: %w", i, err)
		}
		if !match {
			continue
		}
		// The reason is interpolated against the record, so a rule covering
		// several defects ("coordinate ${coordinateQuality}") reports each of
		// them by name rather than lumping them together.
		reason, err := catalogRecordScope(scope, record).interpolate(rule.Reason)
		if err != nil {
			return false, fmt.Errorf("exclude rule %d reason: %w", i, err)
		}
		counters[catalogCounterExcluded]++
		counters[catalogExcludedPrefix+reason]++
		return true, nil
	}
	return false, nil
}

// catalogMatches applies one rule's `when:` to one record.
//
// An expression that fails to evaluate is an error, never false: a predicate
// that silently reads as false is how an exclusion rule stops excluding with
// nothing to see.
func catalogMatches(cache *exprCache, scope *runContext, when string, record map[string]any) (bool, error) {
	expr, err := interpolatePredicate(scope, when)
	if err != nil {
		return false, err
	}
	return cache.truthy(expr, record)
}

// catalogRecordScope puts a record's own fields within reach of `${...}`, so
// a reason or an id template can name them: `coordinate ${coordinateQuality}`,
// `resource:mandi-price:market:${marketId}`.
func catalogRecordScope(scope *runContext, record map[string]any) *runContext {
	locals := make(map[string]any, len(scope.locals)+len(record))
	for name, value := range scope.locals {
		locals[name] = value
	}
	for name, value := range record {
		locals[name] = value
	}
	return &runContext{inputs: scope.inputs, token: scope.token, outputs: scope.outputs, locals: locals}
}

func catalogCheckDirection(direction string) error {
	switch {
	case direction == "",
		strings.EqualFold(direction, "asc"),
		strings.EqualFold(direction, "desc"):
		return nil
	default:
		return fmt.Errorf("order direction %q is not supported; use asc or desc", direction)
	}
}

// catalogChunk splits a group into runs that each stay within the budget.
//
// A record costing nothing never forces a split; a run is cut only when adding
// the NEXT record would exceed the budget, so exactly budget-many costing
// records still make one catalogue. Order is preserved, so the partition is
// deterministic and a record lands in exactly one chunk.
//
// A budget of zero or less means the group is not split at all -- a catalogue
// with no declared limit is one catalogue, not an unbounded number of them.
func catalogChunk(records []map[string]any, spec Chunk, cache *exprCache) ([][]map[string]any, error) {
	if spec.Budget <= 0 {
		return [][]map[string]any{records}, nil
	}

	var chunks [][]map[string]any
	var current []map[string]any
	spent := 0

	for _, record := range records {
		cost, err := catalogCost(spec.Cost, record, cache)
		if err != nil {
			return nil, err
		}
		if len(current) > 0 && spent+cost > spec.Budget {
			chunks = append(chunks, current)
			current, spent = nil, 0
		}
		current = append(current, record)
		spent += cost
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks, nil
}

// catalogCost is what one record spends of the budget. An absent `cost:`
// means one per record, which is chunking by count.
func catalogCost(expr string, record map[string]any, cache *exprCache) (int, error) {
	if strings.TrimSpace(expr) == "" {
		return 1, nil
	}
	value, err := cache.evaluate(expr, record)
	if err != nil {
		return 0, fmt.Errorf("chunk cost: %w", err)
	}
	number, ok := numeric(value)
	if !ok {
		// A cost that is not a number would otherwise count as zero and the
		// budget would never be reached, which looks exactly like a group that
		// fits.
		return 0, fmt.Errorf("chunk cost %q produced %v, which is not a number", expr, value)
	}
	if number < 0 {
		return 0, fmt.Errorf("chunk cost %q produced %v: a negative cost would buy back budget already spent", expr, number)
	}
	return int(number), nil
}

// catalogRender names one chunk and turns it into a document.
func catalogRender(ctx context.Context, catalog Catalog, chunk []map[string]any,
	index int, key string, scope *runContext, cache *exprCache,
	mapper Mapper, mappingBase string) (Catalogue, error) {

	slug, err := catalogSlug(catalog, index, key, scope, cache)
	if err != nil {
		return Catalogue{}, err
	}

	chunkScope := scope.with("slug", slug).with("chunkIndex", index)

	catalogID, err := chunkScope.interpolate(catalog.Identity.CatalogID)
	if err != nil {
		return Catalogue{}, fmt.Errorf("identity.catalogId: %w", err)
	}

	if catalog.Identity.ResourceID != "" {
		for _, record := range chunk {
			resourceID, err := catalogRecordScope(chunkScope, record).interpolate(catalog.Identity.ResourceID)
			if err != nil {
				return Catalogue{}, fmt.Errorf("identity.resourceId: %w", err)
			}
			record[catalogResourceField] = resourceID
		}
	}

	local := make(map[string]any, len(catalog.Render.Local))
	for name, template := range catalog.Render.Local {
		value, err := chunkScope.interpolate(template)
		if err != nil {
			return Catalogue{}, fmt.Errorf("render local %q: %w", name, err)
		}
		local[name] = value
	}

	// The same input shape the mapping half of every other call receives: the
	// records under `response`, everything the payload does not carry under
	// `_local`.
	input := map[string]any{"response": chunk, "_local": local}

	// A file writes `mappings/catalog.yaml`; the mappings are served from the
	// root of that directory. Same rule as steps.go's mappingRef.
	ref := mappingBase + "/" + strings.TrimPrefix(catalog.Render.Mapping, "mappings/")

	content, err := mapper.Transform(ctx, ref, definition.DirectionResponse, input)
	if err != nil {
		return Catalogue{}, fmt.Errorf("rendering %s: %w", slug, err)
	}

	return Catalogue{Slug: slug, CatalogID: catalogID, Content: content}, nil
}

// catalogSlug renders the chunk's name.
//
// Each ${...} in the template is a JSONata expression, not a dotted path,
// because the one this file has to support is a ternary:
//
//	slug: "${stateCode}${chunkIndex > 1 ? '-' & chunkIndex : ''}"
//
// which keeps a group small enough to fit under its plain name and numbers
// only the second and later chunks -- so a group's catalogId never changes
// merely because another group grew.
//
// The expressions are evaluated against the group's own values plus
// chunkIndex and inputs, and nothing else: a slug is a name for this group,
// and one reaching into another step's output would name a file after
// something that has nothing to do with what is in it. An expression that
// resolves to nothing is an error, not an empty piece of name.
func catalogSlug(catalog Catalog, index int, key string, scope *runContext, cache *exprCache) (string, error) {
	if catalog.Chunk.Slug == "" {
		// No template: the group's own key is its name.
		return key, nil
	}

	document := map[string]any{}
	if group, ok := scope.locals["group"].(map[string]any); ok {
		for field, value := range group {
			document[field] = value
		}
	}
	document[catalog.GroupBy] = key
	document["chunkIndex"] = index
	document["inputs"] = scope.inputs

	var failed error
	out := placeholder.ReplaceAllStringFunc(catalog.Chunk.Slug, func(match string) string {
		expr := strings.TrimSpace(placeholder.FindStringSubmatch(match)[1])
		value, err := cache.evaluate(expr, document)
		if err != nil {
			if failed == nil {
				failed = fmt.Errorf("chunk slug: %w", err)
			}
			return match
		}
		switch value.(type) {
		case nil:
			if failed == nil {
				failed = fmt.Errorf("chunk slug: %q resolved to nothing", expr)
			}
			return match
		case map[string]any, []any:
			if failed == nil {
				failed = fmt.Errorf("chunk slug: %q produced a %T, which is not a name", expr, value)
			}
			return match
		}
		return renderScalar(value)
	})
	if failed != nil {
		return "", failed
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("chunk slug %q rendered empty: a catalogue with no name cannot be written or published", catalog.Chunk.Slug)
	}
	return out, nil
}

// WriteCatalogues writes each built catalog to <dir>/<filenamePrefix>-<slug>.json,
// after clearing any catalog this run did not produce.
//
// Indented, because these files exist to be read: somebody reviews what is
// about to go onto the network, and a single-line document of several hundred
// markets cannot be reviewed. The publish step reads the same directory back,
// which is why the naming is fixed rather than a caller's choice.
//
// THE CLEARING IS NOT HOUSEKEEPING. The publish step globs every
// <filenamePrefix>-*.json in this directory and posts all of them, so a
// catalog left behind by an earlier run is republished as though it were
// current. Maharashtra splitting into MH and MH-2 on Monday and fitting into
// one catalog on Tuesday would, without this, republish Monday's MH-2 on
// Tuesday: a document of markets that no longer belong in one. A human
// running this by hand could see the directory and notice; the daily
// unattended loop this package exists for cannot, and the bug is invisible on
// the first run and wrong on every one after it.
func WriteCatalogues(built []Catalogue, dir, filenamePrefix string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create catalog output directory: %w", err)
	}

	if err := RemoveStaleCatalogues(dir, filenamePrefix); err != nil {
		return err
	}

	for _, catalog := range built {
		var indented bytes.Buffer
		if err := json.Indent(&indented, catalog.Content, "", "  "); err != nil {
			return fmt.Errorf("indent JSON for catalogue %s: %w", catalog.Slug, err)
		}

		path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", filenamePrefix, catalog.Slug))
		if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write catalog file %s: %w", path, err)
		}
	}
	return nil
}

// RemoveStaleCatalogues deletes the catalogs already in dir, so only this run's
// output is left for the publish step to find.
//
// The match is deliberately the SAME one catalogueFiles (publish.go) makes --
// a non-directory entry whose name starts with "<filenamePrefix>-" and ends
// in ".json" -- because the set this removes has to be exactly the set that
// would otherwise be published. Matching more broadly would delete a file
// somebody else put here; matching more narrowly would leave one that still
// gets posted.
//
// Nothing else is touched: no recursion, no directories, and no file outside
// that pattern. This directory is an operator's to point wherever they like,
// and it may hold things that are not ours.
func RemoveStaleCatalogues(dir, filenamePrefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read catalog output directory: %w", err)
	}

	namePrefix := filenamePrefix + "-"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale catalog %s: %w", name, err)
		}
	}
	return nil
}
