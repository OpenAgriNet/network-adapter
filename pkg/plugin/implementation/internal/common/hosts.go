package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// HostOf returns the registry's base URL for bindingKey's participant; an
// unpublished binding is a 502 (ADR-0002).
func HostOf(ctx context.Context, registry definition.ProviderRecordLookup, bindingKey string) (string, error) {
	if registry == nil {
		return "", fmt.Errorf("no registry to find %s's host in", bindingKey)
	}
	record, err := registry.ProviderRecord(ctx, bindingKey)
	if err != nil {
		if errors.Is(err, definition.ErrProviderRecordNotFound) {
			return "", model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable,
				fmt.Errorf("the registry publishes no active binding for %s: %w", bindingKey, err))
		}
		return "", fmt.Errorf("no host for %s: %w", bindingKey, err)
	}
	if record == nil || record.BaseURL == "" {
		return "", model.NewCodedErr(http.StatusBadGateway, util.CodeUpstreamUnavailable,
			fmt.Errorf("the registry has no base URL for %s", bindingKey))
	}
	return record.BaseURL, nil
}
