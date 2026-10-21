package model

import (
	"slices"
	"testing"
)

func TestADirectionIsAgreedWhenItIsPresent(t *testing.T) {
	t.Parallel()

	packs := &FederationDirection{SchemaPacks: []string{"https://x/MandiPrice/v0.1/attributes.yaml"}}

	for name, tc := range map[string]struct {
		network     AdmittedNetwork
		isSource    bool
		isRequester bool
	}{
		"source only":    {AdmittedNetwork{Agreement: Agreement{Source: packs}}, true, false},
		"requester only": {AdmittedNetwork{Agreement: Agreement{Requester: packs}}, false, true},
		"both":           {AdmittedNetwork{Agreement: Agreement{Source: packs, Requester: packs}}, true, true},
		"neither":        {AdmittedNetwork{}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.network.IsSource(); got != tc.isSource {
				t.Errorf("IsSource() = %v, want %v", got, tc.isSource)
			}
			if got := tc.network.IsRequester(); got != tc.isRequester {
				t.Errorf("IsRequester() = %v, want %v", got, tc.isRequester)
			}
		})
	}
}

func TestThePacksWeTakeComeFromTheSourceDirection(t *testing.T) {
	t.Parallel()

	network := AdmittedNetwork{Agreement: Agreement{
		Source:    &FederationDirection{SchemaPacks: []string{"mandi"}},
		Requester: &FederationDirection{SchemaPacks: []string{"weather"}},
	}}

	if got := network.SourcePacks(); len(got) != 1 || got[0] != "mandi" {
		t.Errorf("SourcePacks() = %v, want what we take, not what we give", got)
	}
	if got := network.RequesterPacks(); len(got) != 1 || got[0] != "weather" {
		t.Errorf("RequesterPacks() = %v, want what we give", got)
	}
}

func TestAnUnagreedDirectionHasNoPacks(t *testing.T) {
	t.Parallel()

	network := AdmittedNetwork{Agreement: Agreement{Source: &FederationDirection{SchemaPacks: []string{"mandi"}}}}

	if got := network.RequesterPacks(); got != nil {
		t.Errorf("RequesterPacks() = %v, want none -- they may not read from us at all", got)
	}
}

func TestAnAgreementReadsAsTheContextsADiscoverNames(t *testing.T) {
	t.Parallel()

	a := Agreement{
		Source:    &FederationDirection{SchemaPacks: []string{"https://x/MandiPrice/v0.1/attributes.yaml"}},
		Requester: &FederationDirection{SchemaPacks: []string{"https://x/Weather/v0.1/attributes.yaml", "  "}},
	}

	if got := a.SourceContexts(); len(got) != 1 || got[0] != "https://x/MandiPrice/v0.1/context.jsonld" {
		t.Errorf("SourceContexts() = %v, want the context document beside the pack", got)
	}
	if got := a.RequesterContexts(); len(got) != 1 || got[0] != "https://x/Weather/v0.1/context.jsonld" {
		t.Errorf("RequesterContexts() = %v, want the blank entry dropped", got)
	}
}

func TestAnUnagreedDirectionNamesNoContexts(t *testing.T) {
	t.Parallel()

	if got := (Agreement{}).RequesterContexts(); len(got) != 0 {
		t.Errorf("RequesterContexts() = %v, want none", got)
	}
}

func TestOnlyAnAdmittedNetworkHasAnAgreement(t *testing.T) {
	t.Parallel()

	packs := &FederationDirection{SchemaPacks: []string{"mandi"}}

	for name, tc := range map[string]struct {
		agreement  Agreement
		agreed     bool
		directions []string
	}{
		"source only":    {Agreement{Source: packs}, true, []string{"source"}},
		"requester only": {Agreement{Requester: packs}, true, []string{"requester"}},
		"both":           {Agreement{Source: packs, Requester: packs}, true, []string{"source", "requester"}},
		"a provider":     {Agreement{}, false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.agreement.Agreed(); got != tc.agreed {
				t.Errorf("Agreed() = %v, want %v", got, tc.agreed)
			}
			if got := tc.agreement.Directions(); !slices.Equal(got, tc.directions) {
				t.Errorf("Directions() = %v, want %v", got, tc.directions)
			}
		})
	}
}

func TestAPackAlreadyNamingItsContextIsLeftAlone(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ pack, want string }{
		"attributes": {
			"https://x/MandiPrice/v0.1/attributes.yaml",
			"https://x/MandiPrice/v0.1/context.jsonld",
		},
		"already the context": {
			"https://x/MandiPrice/v0.1/context.jsonld",
			"https://x/MandiPrice/v0.1/context.jsonld",
		},
		"trailing slash": {
			"https://x/MandiPrice/v0.1/",
			"https://x/MandiPrice/v0.1/context.jsonld",
		},
		"no filename": {
			"https://x/MandiPrice/v0.1",
			"https://x/MandiPrice/v0.1/context.jsonld",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := Agreement{Source: &FederationDirection{SchemaPacks: []string{tc.pack}}}
			got := a.SourceContexts()
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("SourceContexts(%q) = %v, want [%s]", tc.pack, got, tc.want)
			}
		})
	}
}
