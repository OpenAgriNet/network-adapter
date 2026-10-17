package model

import (
	"strings"
	"time"
)

// A schema pack names its attributes document; a message is interpreted
// against the context document beside it.
const (
	packFile    = "attributes.yaml"
	contextFile = "context.jsonld"
)

// Agreement is what we agreed with another network. A direction is agreed when
// it is present; our own entry, and every provider and upstream, has neither.
type Agreement struct {
	Source    *FederationDirection `json:"source,omitempty"`
	Requester *FederationDirection `json:"requester,omitempty"`
}

// AdmittedNetwork is another network we admitted, as our registry holds it.
type AdmittedNetwork struct {
	ID           string
	DiscoveryURL string

	// How long this network lets us keep what we crawled. Zero forbids caching.
	ProjectionTtl time.Duration

	Agreement
}

// FederationDirection is one agreed direction. SchemaPacks is what the
// agreement covers: for a source what we take, for a requester what they may ask.
type FederationDirection struct {
	SchemaPacks []string `json:"schema_packs,omitempty"`
}

// Agreed reports whether this is a network we admitted at all. False is an
// ordinary participant, which no federation rule applies to.
func (a Agreement) Agreed() bool { return a.Source != nil || a.Requester != nil }

// IsSource reports whether we agreed to read from this network.
func (a Agreement) IsSource() bool { return a.Source != nil }

// IsRequester reports whether we agreed this network may read from us.
func (a Agreement) IsRequester() bool { return a.Requester != nil }

// SourcePacks is what we take from this network, empty if we read nothing.
func (a Agreement) SourcePacks() []string { return a.Source.packs() }

// RequesterPacks is what this network may ask us for, empty if it may not ask.
func (a Agreement) RequesterPacks() []string { return a.Requester.packs() }

// Directions names what we agreed, for a log line. Empty means nothing.
func (a Agreement) Directions() []string {
	var agreed []string
	if a.IsSource() {
		agreed = append(agreed, "source")
	}
	if a.IsRequester() {
		agreed = append(agreed, "requester")
	}
	return agreed
}

// SourceContexts is what we take, as the context documents a discover names.
func (a Agreement) SourceContexts() []string { return contextsOf(a.SourcePacks()) }

// RequesterContexts is what this network may ask us for, named the same way.
func (a Agreement) RequesterContexts() []string { return contextsOf(a.RequesterPacks()) }

func contextsOf(packs []string) []string {
	contexts := make([]string, 0, len(packs))
	for _, pack := range packs {
		if trimmed := strings.TrimSpace(pack); trimmed != "" {
			contexts = append(contexts, strings.TrimSuffix(trimmed, packFile)+contextFile)
		}
	}
	return contexts
}

func (d *FederationDirection) packs() []string {
	if d == nil {
		return nil
	}
	return d.SchemaPacks
}
