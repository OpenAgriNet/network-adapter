// Package KnowledgeAdvisory serves the network's knowledge advisory capability.
//
// One package per schema pack family, so which plugin owns a capability is
// readable from its binding key: openagrinet:KnowledgeAdvisory is this one's,
// openagrinet:MandiPrice is MandiPrice's.
//
// Almost nothing lives here, and that is the point. Recognising a capability,
// resolving the call plan, authenticating, calling with the registry's budget
// and translating in both directions are all internal/common's, because none
// of them differ by domain. What this package owns is its name.
//
// The upstream this was written against is a retrieval service: it takes a
// query and answers with ranked passages from a document corpus. Turning the
// caller's topics into that query, and those passages into an advisory, is
// entirely the mapping's work -- so this package is a name and nothing else.
// See prerequisites.go for why that is worth stating rather than assuming.
package KnowledgeAdvisory

import (
	"context"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// Config is upstream's, unchanged. Aliased here so a domain plugin's cmd
// package need not know where the machinery lives.
type Config = common.Config

// New creates the knowledge advisory step.
//
// Which capabilities it answers to is configuration, with no default: a package
// serving a family cannot guess which of them a deployment has providers for.
func New(ctx context.Context, registry definition.ProviderRecordLookup, mapper definition.Mapper,
	cfg *Config) (definition.Step, func() error, error) {
	return common.New(ctx, registry, mapper, prerequisites, cfg)
}
