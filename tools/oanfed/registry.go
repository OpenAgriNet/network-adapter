package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/federation"
)

// registryClient writes peer records to this network's own SunbirdRC registry.
//
// It is the operator's client, not the adapter's: the adapter READS the registry
// through its plugin, and nothing in the request path writes to it. Admission is
// a decision an operator makes, so the thing that records it is a command they
// run.
type registryClient struct {
	baseURL string
	http    *http.Client

	// Keycloak, and the issuer it must mint for.
	//
	// The registry validates a token's issuer against its own configured URI,
	// which names Keycloak by its CONTAINER-INTERNAL address. A token minted
	// with the published host port in its issuer is refused with a 401 and an
	// empty body, which is a confusing way to learn this.
	keycloakURL      string
	keycloakIssuer   string
	realm            string
	clientID         string
	username         string
	password         string
	cachedBearer     string
	cachedBearerTime time.Time
}

type registryOptions struct {
	registryURL    string
	keycloakURL    string
	keycloakIssuer string
	realm          string
	clientID       string
	username       string
	password       string
	timeout        time.Duration
}

func newRegistryClient(o registryOptions) (*registryClient, error) {
	if o.registryURL == "" {
		return nil, fmt.Errorf("-registry-url is required")
	}
	if o.keycloakURL == "" {
		return nil, fmt.Errorf("-keycloak-url is required")
	}
	return &registryClient{
		baseURL:        strings.TrimRight(o.registryURL, "/"),
		http:           &http.Client{Timeout: o.timeout},
		keycloakURL:    strings.TrimRight(o.keycloakURL, "/"),
		keycloakIssuer: o.keycloakIssuer,
		realm:          o.realm,
		clientID:       o.clientID,
		username:       o.username,
		password:       o.password,
	}, nil
}

// bearer mints an access token, reusing one for a short window.
//
// The window is deliberately far shorter than any realistic token lifetime: a
// stale token here costs one extra round trip, while a token that outlives its
// validity costs a 401 with an empty body in the middle of a write.
func (c *registryClient) bearer(ctx context.Context) (string, error) {
	if c.cachedBearer != "" && time.Since(c.cachedBearerTime) < 30*time.Second {
		return c.cachedBearer, nil
	}

	form := url.Values{
		"client_id":  {c.clientID},
		"grant_type": {"password"},
		"username":   {c.username},
		"password":   {c.password},
	}
	endpoint := fmt.Sprintf("%s/auth/realms/%s/protocol/openid-connect/token", c.keycloakURL, c.realm)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Keycloak runs behind proxy-address forwarding and builds the token's
	// issuer from these. Without them it answers with an empty body.
	if c.keycloakIssuer != "" {
		request.Header.Set("X-Forwarded-Host", c.keycloakIssuer)
		request.Header.Set("X-Forwarded-Proto", "http")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("keycloak: %w", err)
	}
	defer response.Body.Close()

	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload.AccessToken == "" {
		return "", fmt.Errorf("keycloak issued no token (HTTP %s) -- check the realm, client and credentials",
			response.Status)
	}

	c.cachedBearer, c.cachedBearerTime = payload.AccessToken, time.Now()
	return c.cachedBearer, nil
}

// participantRecord is the registry's shape for a peer network.
//
// Only the fields admission owns. Anything else the schema permits belongs to
// whoever set it, and a blind overwrite would quietly discard it.
type participantRecord struct {
	ParticipantID string      `json:"participantId"`
	Name          string      `json:"name"`
	Type          string      `json:"type"`
	Status        string      `json:"status"`
	BaseURL       string      `json:"baseUrl"`
	Role          string      `json:"role"`
	Keys          []publicKey `json:"keys,omitempty"`
	Revision      int         `json:"revision,omitempty"`
	AdmittedKeyID string      `json:"admittedKeyId,omitempty"`

	// OSID is the registry's own id, present on a record that was read back and
	// absent on one being created. It is what an update is addressed to.
	OSID string `json:"osid,omitempty"`
}

// publicKey is the registry's PublicKey shape: alg, key, validFrom and status
// are required, validUntil is not.
type publicKey struct {
	OSID       string `json:"osid,omitempty"`
	Alg        string `json:"alg"`
	Key        string `json:"key"`
	ValidFrom  string `json:"validFrom"`
	ValidUntil string `json:"validUntil,omitempty"`
	Status     string `json:"status"`
}

// recordFor renders an admitted peer as the registry stores it.
func recordFor(p federation.Participant) (participantRecord, error) {
	// validFrom is required, and is recorded as the moment we admitted the peer
	// -- the only moment we can actually attest to. The peer's published
	// documents carry no validity window, and inventing one would be recording
	// a claim nobody made.
	admittedAt := time.Now().UTC().Format(time.RFC3339)

	keys := make([]publicKey, 0, len(p.Keys))
	for _, k := range p.Keys {
		encoded, err := federation.RegistryKey(k.X)
		if err != nil {
			return participantRecord{}, fmt.Errorf("key %q: %w", k.Kid, err)
		}
		keys = append(keys, publicKey{
			Alg: "ed25519", Key: encoded, ValidFrom: admittedAt, Status: "active",
		})
	}
	return participantRecord{
		ParticipantID: p.NetworkID,
		Name:          p.Name,
		// A peer is a node, not an upstream: it signs, and its signatures are
		// verified against the keys recorded here.
		Type:          "node",
		Status:        p.Status,
		BaseURL:       p.DiscoveryURL,
		Role:          federation.RoleNetwork,
		Keys:          keys,
		Revision:      p.Revision,
		AdmittedKeyID: p.AdmittedKeyID,
	}, nil
}

// find returns the stored record for a participant, or nil when there is none.
func (c *registryClient) find(ctx context.Context, participantID string) (*participantRecord, error) {
	body, err := json.Marshal(map[string]any{
		"filters": map[string]any{"participantId": map[string]any{"eq": participantID}},
	})
	if err != nil {
		return nil, err
	}

	var records []participantRecord
	if err := c.call(ctx, http.MethodPost, "/api/v1/Participant/search", body, false, &records); err != nil {
		return nil, err
	}
	for i := range records {
		if records[i].ParticipantID == participantID {
			return &records[i], nil
		}
	}
	return nil, nil
}

// save creates a participant, or replaces it when it already exists.
func (c *registryClient) save(ctx context.Context, record participantRecord) error {
	existing, err := c.find(ctx, record.ParticipantID)
	if err != nil {
		return err
	}

	method, path := http.MethodPost, "/api/v1/Participant"
	if existing != nil {
		method = http.MethodPut
		path = fmt.Sprintf("/api/v1/Participant/%s", existing.OSID)
	}

	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return c.call(ctx, method, path, body, true, nil)
}

// call makes one registry request.
//
// authenticated is explicit because search is open and writes are not, and
// minting a token for a read that does not need one is a round trip that can
// fail for reasons unrelated to the read.
func (c *registryClient) call(ctx context.Context, method, path string, body []byte, authenticated bool, into any) error {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	if authenticated {
		bearer, err := c.bearer(ctx)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("registry %s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("registry %s %s: %w", method, path, err)
	}
	if response.StatusCode >= 400 {
		// A 401 comes back with an empty body, so report the status rather than
		// a decode error that says nothing about the cause.
		detail := strings.TrimSpace(string(raw))
		if detail == "" {
			detail = "(empty body)"
		}
		return fmt.Errorf("registry refused %s %s: HTTP %d: %s",
			method, path, response.StatusCode, truncate(detail, 300))
	}

	if into == nil {
		return nil
	}
	// Search answers {"data": [...]}, writes answer a bare object.
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Data != nil {
		return json.Unmarshal(envelope.Data, into)
	}
	return json.Unmarshal(raw, into)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
