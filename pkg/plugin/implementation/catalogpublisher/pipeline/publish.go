package pipeline

// The pipeline's publish step. It is deliberately thin: the publishing itself,
// including how an answer is judged, lives in
// pkg/plugin/implementation/catalogpublisher/catalogpublish and
// is not reimplemented here. This file only turns the YAML's declared publish
// block into that package's Config, and refuses the run outright in the one
// case the YAML says it must.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/catalogpublisher/catalogpublish"
)

// publishAddressHint names the two ways an operator can supply the address, so
// an empty one says what to set rather than that something is missing.
const publishAddressHint = "set inputs.publishUrl (flag --publish-url or MANDI_PUBLISH_URL)"

// refuseWhenShape is the one form of refusal this step understands:
//
//	collection.<counter> > 0
//
// <counter> is a name the pipeline's own steps record into, so a second
// pipeline refuses on its own terms without editing Go. Anything else is
// REJECTED rather than ignored -- a safety rule that is quietly dropped is
// worse than one never declared -- and a file needing a richer rule needs a
// real expression evaluator here first.
var refuseWhenShape = regexp.MustCompile(`^collection\.([A-Za-z][A-Za-z0-9_]*)\s*>\s*0$`)

// refuseWhenCounter returns the counter a refuseWhen rule names.
func refuseWhenCounter(rule string) (string, bool) {
	match := refuseWhenShape.FindStringSubmatch(strings.TrimSpace(rule))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// publishCatalogs posts every catalog file in catalogDir, unless the declared
// safety rule forbids it.
//
// stateErrors is how many states failed to collect. A state that failed to
// collect is not a state with no markets, so publishing then would replace a
// whole state's catalog with a partial one, or with nothing.
func PublishCatalogues(ctx context.Context, spec Publish, resolved map[string]string,
	catalogDir, filenamePrefix string, stateErrors int) (catalogpublish.Result, error) {
	var result catalogpublish.Result

	if err := checkJudgement(spec); err != nil {
		return result, err
	}

	if rule := strings.TrimSpace(spec.RefuseWhen); rule != "" {
		counter, ok := refuseWhenCounter(rule)
		if !ok {
			return result, fmt.Errorf(
				"publish.refuseWhen is %q; this step understands only `collection.<counter> > 0` "+
					"and will not guess at another rule", rule)
		}
		if stateErrors > 0 {
			return result, fmt.Errorf(
				"refusing to publish: %d of the collection failed (%s), and a partial collection "+
					"will not be published as though it were whole (publish.refuseWhen: %s)",
				stateErrors, counter, rule)
		}
	}

	// The YAML's publish.url is ${inputs.publishUrl}/publish, but
	// catalogpublish.Publish appends "/publish" to Config.PublishURL itself:
	//
	//     base := strings.TrimRight(cfg.PublishURL, "/") + "/publish"
	//
	// So the BASE address goes in, not the YAML's rendered url. Passing the
	// rendered url would post to /publish/publish, which fails as a 404 far
	// from here and reads like an unreachable adapter rather than a bug.
	if err := checkPublishURL(spec); err != nil {
		return result, err
	}

	publishURL := strings.TrimSpace(resolved["publishUrl"])
	if publishURL == "" {
		return result, fmt.Errorf("no publish address: %s", publishAddressHint)
	}

	cfg := catalogpublish.Config{
		PublishURL:     publishURL,
		CatalogIn:      catalogDir,
		FilenamePrefix: filenamePrefix,
		AddressHint:    publishAddressHint,
	}
	if err := applyRetireOld(spec.RetireOld, resolved, &cfg); err != nil {
		return result, err
	}

	return catalogpublish.Publish(ctx, cfg)
}

// expectedPublishURL is the only publish.url this step can honour, for the
// reason given where the address is resolved: the base goes to
// catalogpublish, which appends the path itself.
const expectedPublishURL = "${inputs.publishUrl}/publish"

// checkPublishURL refuses a publish.url this step would ignore.
//
// The address actually used comes from inputs.publishUrl, not from this
// field. Without this check an operator could repoint publish.url at another
// host, watch the edit take no effect, and have the catalog posted to
// MANDI_PUBLISH_URL anyway -- the same silent-divergence failure checkJudgement
// exists to prevent, and the reason refuseWhen is compared literally above.
func checkPublishURL(spec Publish) error {
	if url := strings.TrimSpace(spec.URL); url != expectedPublishURL {
		return fmt.Errorf(
			"publish.url is %q, but this step publishes to inputs.publishUrl and would ignore it; "+
				"only %q is honoured", url, expectedPublishURL)
	}
	return nil
}

// applyRetireOld carries the declared retireOld block into the publish config.
//
// catalogpublish can deactivate a superseded catalog (it posts a tombstone),
// so a declared retireOld that never reached it would be a rule the file
// states and nothing performs -- the same failure checkJudgement guards.
//
// Enabled is `${inputs.retireOld}` in the file, and inputs: does not declare
// retireOld at all, so it cannot resolve. That is refused rather than read as
// false: treating an unresolvable enable flag as "off" would silently skip the
// retirement, which is the outcome an operator who wrote the block was trying
// to avoid. Declaring the input, or removing the block, both fix it.
func applyRetireOld(spec RetireOld, resolved map[string]string, cfg *catalogpublish.Config) error {
	enabled := strings.TrimSpace(spec.Enabled)
	if enabled == "" {
		return nil // no retireOld block, nothing to carry
	}

	value, ok := resolved[strings.TrimSuffix(strings.TrimPrefix(enabled, "${inputs."), "}")]
	if !ok {
		return fmt.Errorf(
			"publish.retireOld.enabled is %q, but no such input is declared, so the retirement "+
				"cannot be turned on or off; declare the input or remove the block", enabled)
	}
	if !strings.EqualFold(value, "true") {
		return nil
	}

	if strings.TrimSpace(spec.CatalogID) == "" {
		return fmt.Errorf("publish.retireOld is enabled but names no catalogId to retire")
	}
	cfg.RetireOld = true
	cfg.OldCatalogID = spec.CatalogID
	cfg.RetiredName = spec.DescriptorName
	return nil
}

// checkJudgement verifies the spec's declared verdicts are the ones
// catalogpublish actually applies: ACCEPTED is the only success, and anything
// else -- PARTIAL included -- is a failure.
//
// The judgement is not re-implemented here, so a pipeline declaring something
// different would not change behaviour, it would only make the YAML lie about
// it. A PARTIAL that read as success is precisely the failure this guards:
// the catalog indexes with resources missing (273 markets published as 128
// findable ones), and nothing downstream would say so.
func checkJudgement(spec Publish) error {
	const accepted = "ACCEPTED"

	if len(spec.Accept) != 1 || !strings.EqualFold(strings.TrimSpace(spec.Accept[0]), accepted) {
		return fmt.Errorf(
			"publish.accept is %v, but publishing treats %s as the only success; "+
				"this step cannot honour any other rule",
			spec.Accept, accepted)
	}

	// PARTIAL must be declared a failure where the spec lists failures at all:
	// a treatAsFailure that omits it reads as though PARTIAL were tolerated.
	failsOnPartial := false
	for _, status := range spec.TreatAsFailure {
		status = strings.TrimSpace(status)
		if strings.EqualFold(status, accepted) {
			return fmt.Errorf("publish.treatAsFailure lists %s, which publishing treats as success", accepted)
		}
		if strings.EqualFold(status, "PARTIAL") {
			failsOnPartial = true
		}
	}
	if len(spec.TreatAsFailure) > 0 && !failsOnPartial {
		return fmt.Errorf(
			"publish.treatAsFailure is %v and omits PARTIAL, but publishing counts a PARTIAL as a failure: "+
				"a PARTIAL means the catalog indexed with resources missing",
			spec.TreatAsFailure)
	}
	return nil
}
