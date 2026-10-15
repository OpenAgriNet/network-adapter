package model

import "testing"

func TestADirectionIsAgreedWhenItIsPresent(t *testing.T) {
	t.Parallel()

	packs := &FederationDirection{SchemaPacks: []string{"https://x/MandiPrice/v0.1/attributes.yaml"}}

	for name, tc := range map[string]struct {
		network     AdmittedNetwork
		isSource    bool
		isRequester bool
	}{
		"source only":    {AdmittedNetwork{Source: packs}, true, false},
		"requester only": {AdmittedNetwork{Requester: packs}, false, true},
		"both":           {AdmittedNetwork{Source: packs, Requester: packs}, true, true},
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

	network := AdmittedNetwork{
		Source:    &FederationDirection{SchemaPacks: []string{"mandi"}},
		Requester: &FederationDirection{SchemaPacks: []string{"weather"}},
	}

	if got := network.SourcePacks(); len(got) != 1 || got[0] != "mandi" {
		t.Errorf("SourcePacks() = %v, want what we take, not what we give", got)
	}
	if got := network.RequesterPacks(); len(got) != 1 || got[0] != "weather" {
		t.Errorf("RequesterPacks() = %v, want what we give", got)
	}
}

func TestAnUnagreedDirectionHasNoPacks(t *testing.T) {
	t.Parallel()

	network := AdmittedNetwork{Source: &FederationDirection{SchemaPacks: []string{"mandi"}}}

	if got := network.RequesterPacks(); got != nil {
		t.Errorf("RequesterPacks() = %v, want none -- they may not read from us at all", got)
	}
}
