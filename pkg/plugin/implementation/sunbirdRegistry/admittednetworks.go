package sunbirdRegistry

// admittednetworks.go — what our registry holds about other networks we
// admitted. Whether a direction is enough for a job is the caller's call.

import (
	"context"
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

// AdmittedNetwork reports what we agreed with one network, or nil if we never
// admitted it. Nil is an answer; an error means we could not ask.
func (c *Client) AdmittedNetwork(ctx context.Context, networkID string) (*model.AdmittedNetwork, error) {
	start := time.Now()
	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	ctx, span := tracer.Start(ctx, "registry network")
	defer span.End()

	if strings.TrimSpace(networkID) == "" {
		span.SetAttributes(telemetry.AttrErrorType.String(outcomeNotFound))
		c.emitMetrics(ctx, start, operationAdmittedNetwork, outcomeNotFound)
		return nil, nil
	}

	records, err := c.searchNetworks(ctx, tracer, map[string]eqFilter{
		"participantId": {Eq: networkID},
	})
	if err != nil {
		return nil, c.lookupFailed(ctx, span, start, operationAdmittedNetwork, err)
	}

	for _, record := range records {
		if network, ok := admittedFrom(record); ok {
			span.SetAttributes(telemetry.AttrErrorType.String(outcomeFound))
			c.emitMetrics(ctx, start, operationAdmittedNetwork, outcomeFound)
			log.Infof(ctx, "OAN registry resolved network %s, agreed %v", network.ID, network.Directions())
			return &network, nil
		}
	}

	// Not an error -- the registry answered, and the answer is no agreement.
	// Info so a refused crawl is traceable without reading as a fault.
	span.SetAttributes(telemetry.AttrErrorType.String(outcomeNotFound))
	c.emitMetrics(ctx, start, operationAdmittedNetwork, outcomeNotFound)
	log.Infof(ctx, "OAN registry has no usable agreement with network %s", networkID)
	return nil, nil
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

// agreementWith returns what we agreed with a participant, from its record
// alone. The zero Agreement means we admitted nobody by that id.
//
// Deliberately not AdmittedNetwork: that one answers "can we CALL this network",
// so it also wants a network role and a discover address. Whether we agreed to
// ANSWER someone is written in federationRole and nowhere else, and a record
// missing those other fields is a half-filled admission, not an absent one.
func (c *Client) agreementWith(ctx context.Context, participantID string) (model.Agreement, error) {
	if strings.TrimSpace(participantID) == "" {
		return model.Agreement{}, nil
	}
	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	records, err := searchRecords[participant](ctx, c, tracer, c.searchURL, map[string]eqFilter{
		fieldParticipantID: {Eq: participantID},
	})
	if err != nil {
		return model.Agreement{}, err
	}
	for _, record := range records {
		if agreed := record.FederationRole.agreed(); agreed.Agreed() {
			return agreed, nil
		}
	}
	return model.Agreement{}, nil
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
