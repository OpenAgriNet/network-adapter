package catalogcrawler

// federationconfig.go — what a deployment must say before it may crawl networks.

import (
	"fmt"
	"strings"
	"time"
)

const (
	cfgFederationEnabled = "federationEnabled" // "true" to crawl admitted networks

	// The same key the sign step uses, under the same names: a background pass
	// cannot reach that step, so it signs for itself rather than with a second.
	cfgSigningPrivateKey = "signingPrivateKey" // base64 ed25519 seed
	cfgKeyID             = "keyId"             // the key's osid, as the registry holds it

	cfgFederationIntent     = "federationIntent" // the Beckn intent sent to a network, as JSON
	cfgFederationRefreshSec = "federationRefreshSeconds"
	cfgFederationPageSize   = "federationPageSize" // catalogs asked for per call
)

// DefaultFederationRefresh is how often networks are asked when nothing says
// otherwise.
const DefaultFederationRefresh = 15 * time.Minute

// DefaultFederationPageSize is how many catalogs one call asks for. Discovery's
// own default is smaller, so unset would quietly collect a fraction of them.
const DefaultFederationPageSize = 100

type federationConfig struct {
	enabled    bool
	networkID  string
	signingKey string
	keyID      string
	intent     string
	timeout    time.Duration
	refresh    time.Duration
	pageSize   int
}

// federationConfigFrom reads the federation half of the crawler's config.
// Everything is refused at startup: a crawl that cannot sign fails silently.
func federationConfigFrom(config map[string]string) (federationConfig, error) {
	if config[cfgFederationEnabled] != "true" {
		return federationConfig{}, nil
	}

	cfg := federationConfig{
		enabled:    true,
		networkID:  strings.TrimSpace(config[cfgParticipantID]),
		signingKey: strings.TrimSpace(config[cfgSigningPrivateKey]),
		keyID:      strings.TrimSpace(config[cfgKeyID]),
		intent:     strings.TrimSpace(config[cfgFederationIntent]),
		timeout:    durationSecondsOr(config[cfgFetchTimeoutSec], defaultFetchTimeout),
		refresh:    durationSecondsOr(config[cfgFederationRefreshSec], DefaultFederationRefresh),
		pageSize:   intOr(config[cfgFederationPageSize], DefaultFederationPageSize),
	}

	if err := cfg.usable(); err != nil {
		return federationConfig{}, err
	}
	return cfg, nil
}

// usable refuses at startup what would otherwise fail on every pass: a crawl
// that cannot sign is rejected by the far end, and silence there is harder to read.
func (cfg federationConfig) usable() error {
	for key, value := range map[string]string{
		cfgParticipantID:     cfg.networkID,
		cfgSigningPrivateKey: cfg.signingKey,
		cfgKeyID:             cfg.keyID,
	} {
		if value == "" {
			return fmt.Errorf(
				"catalogcrawler: config %q is required when %q is true", key, cfgFederationEnabled)
		}
	}
	_, err := parseIntent(cfg.intent)
	return err
}

// dueEvery decides whether enough time has passed to act. The crawl rides the
// catalog-sync ticker, which fires far more often than a network should be asked.
type dueEvery struct {
	interval time.Duration
	clock    func() time.Time
	last     time.Time
}

func newDueEvery(interval time.Duration, now func() time.Time) *dueEvery {
	return &dueEvery{interval: interval, clock: now}
}

func (d *dueEvery) due() bool {
	at := d.clock()
	if !d.last.IsZero() && at.Sub(d.last) < d.interval {
		return false
	}
	d.last = at
	return true
}
