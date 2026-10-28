package catalogcrawler

// discover.go — building a discover for another network and sending it, a
// page at a time, until that network has shared everything it has.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/signer"
	"github.com/google/uuid"
)

const (
	protocolVersion = "2.0.0"
	discoverAction  = "discover"

	packFile    = "attributes.yaml"
	contextFile = "context.jsonld"

	// How long a crawl's signature stays valid. The network rejects anything
	// outside it, so a slow pass fails rather than replaying.
	signatureWindow = 5 * time.Minute
)

// discoverBody builds the discover we send to one network. We name both ends; the
// network decides what we may see from the agreement it holds.
func discoverBody(network model.AdmittedNetwork, ourNetworkID, intent string,
	offset, size int) ([]byte, error) {
	beckn := map[string]any{
		"action":        discoverAction,
		"version":       protocolVersion,
		"senderId":      ourNetworkID,
		"receiverId":    network.ID,
		"networkId":     ourNetworkID,
		"messageId":     uuid.NewString(),
		"transactionId": uuid.NewString(),
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	}
	if contexts := schemaContexts(network.SourcePacks()); len(contexts) > 0 {
		beckn["schemaContext"] = contexts
	}

	asked, err := parseIntent(intent)
	if err != nil {
		return nil, err
	}

	// In the body, not the query string: a signature does not cover a query
	// string, so the page a crawl asked for would not be covered either.
	return json.Marshal(map[string]any{
		"context": beckn,
		"message": map[string]any{
			"intent": asked,
			"limit":  size,
			"offset": offset,
		},
	})
}

// parseIntent refuses a malformed intent here rather than at the network.
func parseIntent(intent string) (map[string]any, error) {
	if strings.TrimSpace(intent) == "" {
		return map[string]any{}, nil
	}
	var asked map[string]any
	if err := json.Unmarshal([]byte(intent), &asked); err != nil {
		return nil, fmt.Errorf("catalogcrawler: the configured intent is not JSON: %w", err)
	}
	return asked, nil
}

// schemaContexts turns the packs we take into the context documents a discover
// filters on. Discovery matches any of them, so three asked for is three back.
func schemaContexts(packs []string) []string {
	contexts := make([]string, 0, len(packs))
	for _, pack := range packs {
		if trimmed := strings.TrimSpace(pack); trimmed != "" {
			contexts = append(contexts, strings.TrimSuffix(trimmed, packFile)+contextFile)
		}
	}
	return contexts
}

// discoverURL is the address a network published, called as it stands.
// services.discovery names the discover endpoint itself, not a mount to append
// to, so a network is free to serve it from any path.
func discoverURL(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("catalogcrawler: %q is not an address a discover can be sent to", base)
	}
	return parsed.String(), nil
}

type networkCallerConfig struct {
	networkID  string
	signingKey string
	keyID      string
	timeout    time.Duration
	pageSize   int
}

type networkCaller struct {
	cfg    networkCallerConfig
	signer *signer.Signer
	client *http.Client
	log    *slog.Logger
}

func newNetworkCaller(ctx context.Context, cfg networkCallerConfig, log *slog.Logger) (*networkCaller, error) {
	switch {
	case cfg.networkID == "":
		return nil, fmt.Errorf("catalogcrawler: federationNetworkId is required to crawl networks")
	case cfg.signingKey == "":
		return nil, fmt.Errorf("catalogcrawler: federationSigningKey is required to crawl networks")
	case cfg.keyID == "":
		return nil, fmt.Errorf("catalogcrawler: federationKeyId is required to crawl networks")
	}

	// The same signer the sign step uses, so a network verifies our crawl exactly
	// as it verifies any other call from us.
	sgn, _, err := signer.New(ctx, &signer.Config{})
	if err != nil {
		return nil, fmt.Errorf("catalogcrawler: building the signer: %w", err)
	}

	return &networkCaller{
		cfg:    cfg,
		signer: sgn,
		client: &http.Client{Timeout: cfg.timeout},
		log:    log,
	}, nil
}

// discover hands every catalog a network shares to each, a page at a time.
// No total comes back, so the walk ends on a short page.
func (c *networkCaller) discover(ctx context.Context, network model.AdmittedNetwork, intent string,
	each func(page []json.RawMessage) error) error {
	// The config resolves the default; a second one here would quietly outlive it.
	size := c.cfg.pageSize

	total := 0
	for offset := 0; ; offset += size {
		page, err := c.discoverPage(ctx, network, intent, offset, size)
		if err != nil {
			return err
		}
		if len(page) > 0 {
			if err := each(page); err != nil {
				return err
			}
			total += len(page)
		}
		if len(page) < size {
			c.log.InfoContext(ctx, "catalogcrawler: a network shared everything it has",
				"networkId", network.ID, "catalogs", total)
			return nil
		}
	}
}

func (c *networkCaller) discoverPage(ctx context.Context, network model.AdmittedNetwork, intent string,
	offset, size int) ([]json.RawMessage, error) {
	start := time.Now()

	// Recorded even though nothing left the process: a network we never managed
	// to ask would otherwise show no attempts and no errors at all.
	req, err := c.buildRequest(ctx, network, intent, offset, size)
	if err != nil {
		c.log.ErrorContext(ctx, "catalogcrawler: a network could not be asked",
			"networkId", network.ID, "error", err)
		recordCrawl(ctx, start, operationNetworkAsk, outcomeNotSent)
		return nil, err
	}
	endpoint := req.URL.String()

	c.log.InfoContext(ctx, "catalogcrawler: asking a network",
		"networkId", network.ID, "url", endpoint, "offset", offset, "limit", size)
	res, err := c.client.Do(req)
	if err != nil {
		c.log.ErrorContext(ctx, "catalogcrawler: a network could not be reached",
			"networkId", network.ID, "url", endpoint, "error", err)
		recordCrawl(ctx, start, operationNetworkAsk, outcomeUnreachable)
		return nil, fmt.Errorf("catalogcrawler: asking %s: %w", network.ID, err)
	}
	defer res.Body.Close()

	return c.readPage(ctx, network, res, start, offset)
}

// readPage turns one answer into catalogs. Each way of failing is counted
// apart, so a refusal is never read as a network with nothing to share.
func (c *networkCaller) readPage(ctx context.Context, network model.AdmittedNetwork,
	res *http.Response, start time.Time, offset int) ([]json.RawMessage, error) {
	if res.StatusCode != http.StatusOK {
		c.log.ErrorContext(ctx, "catalogcrawler: a network refused the crawl",
			"networkId", network.ID, "status", res.Status)
		recordCrawl(ctx, start, operationNetworkAsk, outcomeRefused)
		return nil, fmt.Errorf("catalogcrawler: %s answered %s", network.ID, res.Status)
	}

	var answer struct {
		Message struct {
			Catalogs []json.RawMessage `json:"catalogs"`
		} `json:"message"`
	}
	if err := json.NewDecoder(res.Body).Decode(&answer); err != nil {
		c.log.ErrorContext(ctx, "catalogcrawler: a network answered something unreadable",
			"networkId", network.ID, "error", err)
		recordCrawl(ctx, start, operationNetworkAsk, outcomeUnreadable)
		return nil, fmt.Errorf("catalogcrawler: %s answered something unreadable: %w", network.ID, err)
	}

	c.log.InfoContext(ctx, "catalogcrawler: a network answered",
		"networkId", network.ID, "offset", offset, "catalogs", len(answer.Message.Catalogs))
	recordCrawl(ctx, start, operationNetworkAsk, outcomeOK)
	return answer.Message.Catalogs, nil
}

// buildRequest builds one page's signed discover. limit and offset are set
// here alone, so they can move to the body when discovery moves them.
func (c *networkCaller) buildRequest(ctx context.Context, network model.AdmittedNetwork, intent string,
	offset, size int) (*http.Request, error) {
	endpoint, err := discoverURL(network.DiscoveryURL)
	if err != nil {
		return nil, err
	}
	body, err := discoverBody(network, c.cfg.networkID, intent, offset, size)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("catalogcrawler: building the discover for %s: %w", network.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.sign(ctx, req, body); err != nil {
		return nil, err
	}
	return req, nil
}

func (c *networkCaller) sign(ctx context.Context, req *http.Request, body []byte) error {
	created := time.Now().Unix()
	expires := time.Now().Add(signatureWindow).Unix()

	signature, err := c.signer.Sign(ctx, body, c.cfg.signingKey, created, expires)
	if err != nil {
		return fmt.Errorf("catalogcrawler: signing the discover: %w", err)
	}

	req.Header.Set(model.AuthHeaderSubscriber, fmt.Sprintf(
		`Signature keyId="%s|%s|ed25519",algorithm="ed25519",created="%d",expires="%d",`+
			`headers="(created) (expires) digest",signature="%s"`,
		c.cfg.networkID, c.cfg.keyID, created, expires, signature))
	return nil
}
