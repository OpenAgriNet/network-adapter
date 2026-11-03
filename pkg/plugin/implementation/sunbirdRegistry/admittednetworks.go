package sunbirdRegistry

// admittednetworks.go — what our registry holds about other networks we
// admitted. Whether a direction is enough for a job is the caller's call.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// roleNetwork is the layer a network serves, and all the registry can filter on.
// Our own entry serves it too, so the caller drops itself by its configured id.
const roleNetwork = "network"

// AdmittedNetworks lists every network we admitted, in any direction.
func (c *Client) AdmittedNetworks(ctx context.Context) ([]model.AdmittedNetwork, error) {
	start := time.Now()
	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	ctx, span := tracer.Start(ctx, "registry networks")
	defer span.End()

	records, err := c.searchNetworks(ctx, tracer, nil)
	if err != nil {
		return nil, c.lookupFailed(ctx, span, start, operationAdmittedNetworks, err)
	}

	networks := make([]model.AdmittedNetwork, 0, len(records))
	for _, record := range records {
		if network, ok := admittedFrom(record); ok {
			networks = append(networks, network)
		}
	}

	span.SetAttributes(telemetry.AttrErrorType.String(outcomeFound))
	c.emitMetrics(ctx, start, operationAdmittedNetworks, outcomeFound)
	log.Infof(ctx, "OAN registry resolved %d networks from %d records", len(networks), len(records))
	return networks, nil
}

func (c *Client) lookupFailed(ctx context.Context, span trace.Span, start time.Time, operation string, err error) error {
	outcome := classify(err)
	span.RecordError(err)
	span.SetStatus(codes.Error, outcome)
	span.SetAttributes(telemetry.AttrErrorType.String(outcome))
	c.emitMetrics(ctx, start, operation, outcome)
	return err
}

func (c *Client) searchNetworks(ctx context.Context, tracer trace.Tracer, extra map[string]eqFilter) ([]participant, error) {
	filters := map[string]eqFilter{"role": {Contains: roleNetwork}}
	for name, filter := range extra {
		filters[name] = filter
	}
	return searchRecords[participant](ctx, c, tracer, c.searchURL, filters)
}

// agreementWith returns what we agreed with a participant, from federationRole
// alone. Not AdmittedNetwork: that answers whether we can CALL them, which a
// half-filled record fails without meaning we may answer it.
//
// useCache false skips the cached answer but still stores the fresh one.
// fromCache reports which it was, so a caller can refuse only on fresh data.
func (c *Client) agreementWith(ctx context.Context, participantID string, useCache bool) (agreed model.Agreement, fromCache bool, err error) {
	if strings.TrimSpace(participantID) == "" {
		return model.Agreement{}, false, nil
	}
	key := agreementCacheKey(participantID)
	if useCache {
		if agreed, ok := c.cachedAgreement(ctx, key); ok {
			log.Debugf(ctx, "OAN registry agreement cache hit for key: %s", key)
			return agreed, true, nil
		}
	}
	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	records, err := searchRecords[participant](ctx, c, tracer, c.searchURL, map[string]eqFilter{
		fieldParticipantID: {Eq: participantID},
	})
	if err != nil {
		// Never cached: an outage remembered as "no agreement" would outlive it.
		return model.Agreement{}, false, err
	}
	for _, record := range records {
		if a := record.FederationRole.agreed(); a.Agreed() {
			agreed = a
			break
		}
	}
	// "No agreement" is cached too: it is the answer for every local caller, so
	// it is most of the traffic. Like a key lookup, a withdrawn agreement takes
	// up to cacheTTL to be seen; a widened one does not, since a caller refusing
	// on a cached answer asks again with useCache false.
	c.cacheAgreement(ctx, key, agreed)
	return agreed, false, nil
}

func agreementCacheKey(participantID string) string {
	return "registry_agreement_" + participantID
}

// cachedAgreement reads a stored agreement. An empty one is stored as "{}", so
// "nothing agreed" never reads as a miss.
func (c *Client) cachedAgreement(ctx context.Context, key string) (model.Agreement, bool) {
	if !c.cachingEnabled() {
		return model.Agreement{}, false
	}
	raw, err := c.cache.Get(ctx, key)
	if err != nil || raw == "" {
		return model.Agreement{}, false
	}
	var agreed model.Agreement
	if err := json.Unmarshal([]byte(raw), &agreed); err != nil {
		log.Warnf(ctx, "Discarding unreadable cache entry for key %s: %v", key, err)
		return model.Agreement{}, false
	}
	return agreed, true
}

func (c *Client) cacheAgreement(ctx context.Context, key string, agreed model.Agreement) {
	if !c.cachingEnabled() {
		return
	}
	data, err := json.Marshal(agreed)
	if err != nil {
		log.Warnf(ctx, "Failed to encode OAN registry agreement for caching, key %s: %v", key, err)
		return
	}
	if err := c.cache.Set(ctx, key, string(data), c.cacheTTL); err != nil {
		log.Warnf(ctx, "Failed to cache OAN registry agreement for key %s: %v", key, err)
	}
}

// admittedFrom reports the record as a network, or ok=false when we cannot act
// on it: suspended, not a network, no discover address, or no agreed direction.
func admittedFrom(record participant) (model.AdmittedNetwork, bool) {
	network := model.AdmittedNetwork{
		ID:            record.ParticipantID,
		DiscoveryURL:  strings.TrimSpace(record.BaseURL),
		ProjectionTTL: time.Duration(record.ProjectionTTL) * time.Second,
		Agreement:     record.FederationRole.agreed(),
	}
	usable := strings.EqualFold(record.Status, statusActive) &&
		record.Role.has(roleNetwork) &&
		network.DiscoveryURL != "" &&
		(network.IsSource() || network.IsRequester())
	return network, usable
}
