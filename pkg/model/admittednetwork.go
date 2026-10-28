package model

import "time"

// AdmittedNetwork is another network we admitted, as our registry holds it.
// Unlike Subscription, it carries what we agreed with that network.
type AdmittedNetwork struct {
	ID           string
	DiscoveryURL string

	// How long this network lets us keep what we crawled. Zero forbids caching.
	ProjectionTTL time.Duration

	// A direction is agreed when it is present. Our own entry, and every
	// provider and upstream, carries neither.
	Source    *FederationDirection
	Requester *FederationDirection
}

// FederationDirection is one agreed direction. SchemaPacks is what the
// agreement covers: for a source what we take, for a requester what they may ask.
type FederationDirection struct {
	SchemaPacks []string
}

// IsSource reports whether we agreed to read from this network.
func (n AdmittedNetwork) IsSource() bool { return n.Source != nil }

// IsRequester reports whether we agreed this network may read from us.
func (n AdmittedNetwork) IsRequester() bool { return n.Requester != nil }

// SourcePacks is what we take from this network, empty if we read nothing.
func (n AdmittedNetwork) SourcePacks() []string { return n.Source.packs() }

// RequesterPacks is what this network may ask us for, empty if it may not ask.
func (n AdmittedNetwork) RequesterPacks() []string { return n.Requester.packs() }

// Directions names what we agreed, for a log line. Empty means nothing.
func (n AdmittedNetwork) Directions() []string {
	var agreed []string
	if n.IsSource() {
		agreed = append(agreed, "source")
	}
	if n.IsRequester() {
		agreed = append(agreed, "requester")
	}
	return agreed
}

func (d *FederationDirection) packs() []string {
	if d == nil {
		return nil
	}
	return d.SchemaPacks
}
