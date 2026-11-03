package providerstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// activeProviderLister is the half of the registry this step needs. Narrowed so
// a registry that cannot list providers fails at startup, not per request.
type activeProviderLister interface {
	ActiveProviderIDs(ctx context.Context) ([]string, error)
}

// step keeps providers the registry does not mark active out of publish and
// discover.
type step struct {
	registry activeProviderLister
}

// New fails when the registry cannot list providers: the gate would otherwise
// wave every publish through.
func New(registry definition.RegistryLookup) (definition.Step, error) {
	lister, ok := registry.(activeProviderLister)
	if !ok {
		return nil, fmt.Errorf("invalid config: the Registry plugin cannot list active providers, " +
			"so providerstatus would let every provider through")
	}
	return &step{registry: lister}, nil
}

func (s *step) Run(ctx *model.StepContext) error {
	_, becknContext, becknErr := model.ExtractContext(ctx.Body)
	if becknErr != nil {
		return model.WrapExtractContextErr("could not read the request context", becknErr)
	}

	action, _ := becknContext["action"].(string)
	switch action {
	case "catalog/publish", "publish":
		return s.checkPublish(ctx)
	case "discover":
		return s.narrowDiscover(ctx)
	}
	return nil
}

func (s *step) checkPublish(ctx *model.StepContext) error {
	named, err := providerIDsOf(ctx.Body)
	if err != nil {
		return model.NewBadReqErr("", err)
	}

	active, err := s.registry.ActiveProviderIDs(ctx)
	if err != nil {
		// Not a verdict on the provider: the registry could not be asked, and an
		// unanswered question must never read as "active".
		return fmt.Errorf("could not check provider status: %w", err)
	}

	// The whole batch is refused on one bad provider, so a partial publish never
	// leaves the sender guessing which catalogs landed.
	for _, id := range named {
		if !slices.Contains(active, id) {
			return model.NewBadReqErr("", fmt.Errorf(
				"provider %q is not an active provider in the registry", id))
		}
	}
	return nil
}

func (s *step) narrowDiscover(ctx *model.StepContext) error {
	active, err := s.registry.ActiveProviderIDs(ctx)
	if err != nil {
		return fmt.Errorf("could not check provider status: %w", err)
	}
	narrowed, err := narrowFilter(ctx.Body, active)
	if err != nil {
		return err
	}
	ctx.Body = narrowed
	return nil
}

// narrowFilter restricts a discover to the given provider ids. Left for the
// discover change; the body passes through unchanged until then.
func narrowFilter(body []byte, ids []string) ([]byte, error) {
	return body, nil
}

// publishRequest is the part of a publish this step reads.
type publishRequest struct {
	Message struct {
		Catalogs []struct {
			Provider struct {
				ID string `json:"id"`
			} `json:"provider"`
		} `json:"catalogs"`
	} `json:"message"`
}

// providerIDsOf returns the provider of every catalog in a publish. A catalog
// that names none is an error rather than a skip: an unnamed provider cannot be
// checked, so it must not slip through.
func providerIDsOf(body []byte) ([]string, error) {
	var req publishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("could not read the publish body: %w", err)
	}
	if len(req.Message.Catalogs) == 0 {
		return nil, fmt.Errorf("the publish carries no catalogs: set message.catalogs")
	}
	ids := make([]string, 0, len(req.Message.Catalogs))
	for i, c := range req.Message.Catalogs {
		if c.Provider.ID == "" {
			return nil, fmt.Errorf("catalog %d names no provider: set message.catalogs[%d].provider.id", i, i)
		}
		ids = append(ids, c.Provider.ID)
	}
	return ids, nil
}
