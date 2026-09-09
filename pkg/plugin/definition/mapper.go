package definition

import (
	"context"
)

// Direction names which half of a mapping to run. A mapping file carries both,
// because both legs of one upstream call belong together.
type Direction string

const (
	// DirectionRequest translates an inbound payload into what the upstream wants.
	DirectionRequest Direction = "request"
	// DirectionResponse translates the upstream's answer back.
	DirectionResponse Direction = "response"
	// DirectionFanOut names the values a single inbound payload must be split
	// across, one upstream call each.
	//
	// It exists because some providers answer one question at a time. POCRA's
	// facility search takes exactly one category code -- a comma-separated pair
	// matches nothing and an array is refused outright -- so a payload asking
	// for two facility types cannot be served by one call to it, however the
	// request half is written.
	//
	// A mapping declaring no fan-out is called once, which is every provider
	// that can answer a whole payload in one exchange.
	//
	// Fan-out changes what the response half's `response` holds: a single
	// upstream body without it, an array of bodies (one per fan-out value)
	// with it. A mapping written with $count(response.hits) or a bare index
	// into response will behave differently between the two modes with no
	// error, so check the direction before assuming response's shape.
	DirectionFanOut Direction = "fanOut"
)

// Mapper transforms a document with a mapping fetched from a reference.
//
// It exists so that translating between the network's Beckn payloads and a provider's
// own shape is configuration rather than code: a new provider ships mapping
// files, not a new transformation routine. The mapper itself knows nothing
// about any provider, and nothing about what a mapping says -- it fetches,
// compiles and runs whatever the reference points at.
type Mapper interface {
	// Transform runs the mapping at mappingRef over input and returns the
	// result.
	//
	// mappingRef is what the registry carries verbatim: the URL of one published
	// file holding both directions.
	//
	// Which action the mapping serves is settled by the registry entry that
	// named it, so only the direction is passed here.
	//
	// input carries what a party sent -- the inbound payload, and on the way
	// back the provider's answer -- plus, under _local, any values the caller
	// resolved before making the call.
	//
	// _local is for what a payload cannot carry and a mapping cannot obtain: a
	// code looked up from a name, a point resolved to a market. A caller with
	// nothing to add passes an empty map, so a mapping referring to _local
	// reads nothing rather than failing. Values the caller already holds and
	// merely used to make the call do NOT belong here -- routing those back
	// through a mapping is a second name for the same data.
	//
	// A direction the file has no transform for produces nothing, with no error.
	// What nothing means belongs to the caller: on the request leg it means there
	// is no document to send.
	Transform(ctx context.Context, mappingRef string, direction Direction, input any) ([]byte, error)

	// Verify checks the preconditions the mapping at mappingRef declares, and
	// returns an error carrying the mapping's own explanation when one fails.
	//
	// It exists because a mapping otherwise cannot refuse. Without it, every
	// judgement about whether a payload can be served at all lives in Go, so a
	// provider with its own rule needs its own build -- and the rule and the
	// extraction it guards end up in different places.
	//
	// A mapping declaring no preconditions imposes none. That is what lets the
	// facility be adopted per provider rather than all at once.
	Verify(ctx context.Context, mappingRef string, input any) error
}

// MapperProvider initializes a new Mapper.
type MapperProvider interface {
	New(ctx context.Context, config map[string]string) (Mapper, func() error, error)
}
