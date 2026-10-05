package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// federatedStep answers an action aimed at a provider on ANOTHER network, by
// calling that network rather than looking the provider up here.
//
// The gap it fills: a provider we do not serve falls through every provider
// step -- common.Step.Run returns nil when !serves(binding) -- and nothing
// answers it. Under federation that is no longer necessarily an error: the
// provider may be real and simply belong to a peer.
//
// WHY HERE, and not in the router. The router resolves a target from
// context.bppUri; in a consolidated deployment that URI IS this adapter, so the
// rule would route the call back to itself and match again. Deciding in-process
// cannot loop, and one implementation serves both the split and the consolidated
// deployment shape.
//
// It looks NOTHING up. Network, provider and endpoint all arrive in the Beckn
// envelope, which is what context.networkId, bppId and bppUri are for. A lookup
// here would also be against the wrong database: what we crawled lives in
// discovery's, not ours.
type federatedStep struct {
	// network is this deployment's own network id. A request naming any other
	// network is not ours to answer.
	network string
	client  *http.Client
}

// defaultFederatedTimeout bounds a call to a peer we do not control. Finite,
// because an unresponsive peer must not pin one of our handlers open.
const defaultFederatedTimeout = 30 * time.Second

// maxFederatedResponseBytes caps what a peer may return. A peer is admitted,
// not trusted with our memory.
const maxFederatedResponseBytes = 8 << 20

func newFederatedStep(network string, timeout time.Duration) (*federatedStep, error) {
	if network == "" {
		return nil, fmt.Errorf("invalid config: the federated step needs networkId set on the module, " +
			"otherwise no request can be recognised as ours")
	}
	if timeout <= 0 {
		timeout = defaultFederatedTimeout
	}
	return &federatedStep{network: network, client: &http.Client{Timeout: timeout}}, nil
}

// Run forwards the request when it belongs to another network, and does nothing
// when it does not.
//
// Doing nothing IS the dispatch, the same convention the provider steps follow:
// a step recognises its own work and otherwise lets the chain continue. So a
// request for us takes exactly the path it takes today, including the 404 when
// the binding is unknown.
func (s *federatedStep) Run(ctx *model.StepContext) error {
	network := networkIDFrom(ctx.Body)
	if network == "" || strings.EqualFold(network, s.network) {
		// Absent reads as OURS. Missing information must not widen what we do;
		// the alternative is calling out to another network because a field was
		// left off.
		return nil
	}

	target := strings.TrimSpace(contextString(ctx.Body, "bpp_uri", "bppUri", "receiverUri"))
	if target == "" {
		// Nowhere to send it, and the local path cannot help: our registry has
		// no record of a peer's provider, so falling through would produce a 404
		// about a binding that was never going to be here. The named field is in
		// the message so the caller can fix it.
		return model.NewBadReqErr("", fmt.Errorf(
			"request is for network %q but carries no context.bppUri to reach it", network))
	}

	action := extractBecknAction(ctx.Body)
	if action == "" {
		return model.NewBadReqErr("", fmt.Errorf(
			"request is for network %q but carries no context.action to call there", network))
	}
	return s.forward(ctx, target, action)
}

// forward calls the peer's endpoint and returns its answer as ours.
func (s *federatedStep) forward(ctx *model.StepContext, target, action string) error {
	endpoint := strings.TrimRight(target, "/") + "/" + action

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(ctx.Body))
	if err != nil {
		return fmt.Errorf("federated call to %s: %w", endpoint, err)
	}
	request.Header.Set("Content-Type", "application/json")
	// The body goes through byte for byte, and the caller's Authorization header
	// with it. This adapter is a hop, not a party: re-signing would replace a
	// proven identity with ours, and editing the body would break the signature
	// already covering it.
	if auth := ctx.Request.Header.Get(model.AuthHeaderSubscriber); auth != "" {
		request.Header.Set(model.AuthHeaderSubscriber, auth)
	}

	log.Debugf(ctx, "federated: forwarding %s for network to %s", action, endpoint)

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("federated call to %s: %w", endpoint, err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxFederatedResponseBytes))
	if err != nil {
		return fmt.Errorf("reading federated response from %s: %w", endpoint, err)
	}
	if response.StatusCode != http.StatusOK {
		// Reported as our failure, not relayed as the peer's. The caller asked
		// US; a peer's status code is evidence, not an answer to pass on.
		return fmt.Errorf("federated call to %s: peer answered %s", endpoint, response.Status)
	}

	// Returned in place of the generated ACK. The peer has already answered, and
	// an ACK here would discard that answer.
	ctx.ResponseBody = body
	return nil
}

func networkIDFrom(body []byte) string {
	return strings.TrimSpace(contextString(body, "network_id", "networkId"))
}

// contextString reads the first of several spellings out of the Beckn context,
// the way the router already does: snake_case, camelCase, then the v2 name.
func contextString(body []byte, keys ...string) string {
	_, reqContext, becknErr := model.ExtractContext(body)
	if becknErr != nil {
		return ""
	}
	for _, key := range keys {
		if value, ok := reqContext[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}
