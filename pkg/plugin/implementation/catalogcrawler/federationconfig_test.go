package catalogcrawler

import (
	"testing"
	"time"
)

func federationOn() map[string]string {
	return map[string]string{
		cfgFederationEnabled: "true",
		cfgParticipantID:     ourNetwork,
		cfgSigningPrivateKey: "c2VlZA==",
		cfgKeyID:             "key-1",
		cfgFederationIntent:  `{"filters":{"type":"jsonpath","expression":"$.catalogs[*] ? (@.isActive == true)"}}`,
	}
}

func TestFederationConfigOffUnlessAskedFor(t *testing.T) {
	t.Parallel()

	for name, config := range map[string]map[string]string{
		"nothing set": {},
		"not enabled": {cfgParticipantID: ourNetwork, cfgSigningPrivateKey: "c2VlZA=="},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := federationConfigFrom(config)
			if err != nil {
				t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
			}
			if cfg.enabled {
				t.Error("federation should be off unless asked for")
			}
		})
	}
}

func TestFederationConfigReadsWhatItNeeds(t *testing.T) {
	t.Parallel()

	cfg, err := federationConfigFrom(federationOn())
	if err != nil {
		t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
	}
	if !cfg.enabled {
		t.Fatal("expected federation to be enabled")
	}
	if cfg.networkID != ourNetwork {
		t.Errorf("networkID = %q, want %q", cfg.networkID, ourNetwork)
	}
	if cfg.keyID != "key-1" {
		t.Errorf("keyID = %q, want key-1", cfg.keyID)
	}
	if cfg.intent == "" {
		t.Error("intent was not read")
	}
}

func TestFederationConfigRefusesWhatItCannotRunWithout(t *testing.T) {
	t.Parallel()

	for _, missing := range []string{cfgParticipantID, cfgSigningPrivateKey, cfgKeyID} {
		t.Run(missing, func(t *testing.T) {
			config := federationOn()
			delete(config, missing)
			if _, err := federationConfigFrom(config); err == nil {
				t.Errorf("expected %s to be required, got no error", missing)
			}
		})
	}
}

func TestFederationConfigRefusesAnUnreadableIntent(t *testing.T) {
	t.Parallel()

	config := federationOn()
	config[cfgFederationIntent] = "not json"

	if _, err := federationConfigFrom(config); err == nil {
		t.Error("expected an unreadable intent to be refused at startup, got none")
	}
}

func TestFederationConfigTimeoutHasADefault(t *testing.T) {
	t.Parallel()

	cfg, err := federationConfigFrom(federationOn())
	if err != nil {
		t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
	}
	if cfg.timeout != defaultFetchTimeout {
		t.Errorf("timeout = %v, want the fetch default %v", cfg.timeout, defaultFetchTimeout)
	}

	config := federationOn()
	config[cfgFetchTimeoutSec] = "9"
	cfg, err = federationConfigFrom(config)
	if err != nil {
		t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
	}
	if cfg.timeout != 9*time.Second {
		t.Errorf("timeout = %v, want 9s", cfg.timeout)
	}
}

func TestFederationConfigRefreshHasADefault(t *testing.T) {
	t.Parallel()

	cfg, err := federationConfigFrom(federationOn())
	if err != nil {
		t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
	}
	if cfg.refresh != DefaultFederationRefresh {
		t.Errorf("refresh = %v, want %v", cfg.refresh, DefaultFederationRefresh)
	}

	config := federationOn()
	config[cfgFederationRefreshSec] = "60"
	cfg, err = federationConfigFrom(config)
	if err != nil {
		t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
	}
	if cfg.refresh != time.Minute {
		t.Errorf("refresh = %v, want 1m", cfg.refresh)
	}
}

func TestFederationCrawlRunsOnlyWhenDue(t *testing.T) {
	t.Parallel()

	now := time.Now()
	schedule := newDueEvery(time.Minute, func() time.Time { return now })

	if !schedule.due() {
		t.Error("the first tick should run")
	}
	if schedule.due() {
		t.Error("a tick straight after should not run")
	}
	now = now.Add(61 * time.Second)
	if !schedule.due() {
		t.Error("a tick past the interval should run")
	}
}

func TestFederationTakesItsIdentityFromTheParticipantId(t *testing.T) {
	t.Parallel()

	cfg, err := federationConfigFrom(federationOn())
	if err != nil {
		t.Fatalf("federationConfigFrom() returned an unexpected error: %v", err)
	}
	if cfg.networkID != ourNetwork {
		t.Errorf("networkID = %q, want the participantId %q", cfg.networkID, ourNetwork)
	}
}

func TestASizeThatIsNotANumberFallsBackToTheDefault(t *testing.T) {
	cfg, err := federationConfigFrom(map[string]string{
		cfgFederationEnabled: "true", cfgParticipantID: ourNetwork,
		cfgSigningPrivateKey: "c2VlZA==", cfgKeyID: "key-1",
		cfgFederationPageSize: "one hundred",
	})
	if err != nil {
		t.Fatalf("federationConfigFrom: %v", err)
	}
	if cfg.pageSize != DefaultFederationPageSize {
		t.Errorf("pageSize = %d, want the default -- an unreadable size must not become a zero-length page", cfg.pageSize)
	}
}
