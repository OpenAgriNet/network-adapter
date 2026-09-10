package WeatherObservation

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// prerequisiteNone is what a provider configures when its payload is enough. An
// absent prerequisite means the same thing.
const prerequisiteNone = "none"

// resolvers is the work this package can do before a call, BY NAME. A provider
// names one in its config block, so pointing a second weather provider at an
// existing resolver is configuration only.
//
// Names rather than binding keys on purpose. A binding key carries a
// participant id, which is a deployment's own -- this stack runs mausamgram-mock
// and agmarknet-mock, and a production one would not -- so a key written here
// is a key that silently matches nothing in the next environment: no error, an
// empty _local, and a mapping reporting a station it never received.
//
// Only real I/O belongs here -- a station id from a lookup, a market code from a
// table. Anything a mapping can read from the payload is the mapping's, which is
// why Mausamgram needs no entry.
var resolvers = map[string]func(context.Context, any) (map[string]any, error){
	"findStation": findStation,
}

// prerequisitesFor turns each provider's configured resolver name into the map
// the step dispatches on, which is keyed by binding key.
//
// An unknown name fails HERE, at startup, listing what this capability offers.
// The handler builds every provider step before it serves, so a typo stops the
// process rather than leaving a capability that loads cleanly and resolves
// nothing until the first request.
func prerequisitesFor(cfg *common.Config) (common.Prerequisites, error) {
	prerequisites := common.Prerequisites{}
	for _, bindingKey := range cfg.BindingKeys {
		provider, _, _ := strings.Cut(bindingKey, "|")
		name := cfg.PrerequisiteByProvider[strings.TrimSpace(provider)]
		if name == "" || name == prerequisiteNone {
			continue
		}
		resolver, known := resolvers[name]
		if !known {
			available := make([]string, 0, len(resolvers))
			for candidate := range resolvers {
				available = append(available, candidate)
			}
			slices.Sort(available)
			return nil, fmt.Errorf(
				"WeatherObservation: %s names an unknown prerequisite %q; "+
					"this capability offers %v or %q",
				provider, name, available, prerequisiteNone)
		}
		prerequisites[bindingKey] = resolver
	}
	return prerequisites, nil
}

const (
	// mockStationEnv overrides the station without a rebuild.
	mockStationEnv = "IMD_MOCK_STATION_ID"

	// mockStationID is NANCOWRY, in the Nicobars. A real IMD station rather
	// than a placeholder, so a call against the live API either answers or
	// fails for a reason worth reading.
	mockStationID = "43382"
)

// findStation returns the station an upstream call is addressed to, which a
// mapping reads as _local.stationId.
//
// MOCKED: it ignores the payload and returns one station, so every request is
// answered for the same place.
//
// IMD's API is addressed by a station id and a Beckn payload carries a point.
// Resolving one to the other is a spatial query, this adapter is not allowed a
// database, and whether it becomes a service call or is resolved by the
// discovery service before the select arrives is not settled -- so a search
// written here would be deleted either way.
//
// Replacing it is a change to this body alone. The name above is what a config
// names and stationId is what a mapping reads, so neither moves with it.
func findStation(context.Context, any) (map[string]any, error) {
	station := os.Getenv(mockStationEnv)
	if station == "" {
		station = mockStationID
	}
	return map[string]any{"stationId": station}, nil
}
