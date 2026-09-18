package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/merge"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

const (
	defaultFanoutMaxConcurrency = 8
	defaultFanoutTimeout        = 30 * time.Second

	// maxTargetResponseBytes bounds one target's response read. Not
	// configurable yet -- promote it if a deployment needs to tune it.
	maxTargetResponseBytes = 10 << 20
)

// degradedCountHeader carries the COUNT of targets that did not contribute,
// never their addresses (internal), never a body member (a v2 action is
// additionalProperties:false).
const degradedCountHeader = "X-Beckn-Degraded-Count"

// hopByHopHeaders are connection-scoped and must not be forwarded.
// ReverseProxy strips these itself; http.Client.Do does not.
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Content-Length", // derived by http.NewRequest from the body; a copied one may disagree
}

// targetResult is one target's answer, or the reason there isn't one.
type targetResult struct {
	status int
	header http.Header
	body   []byte
	err    error
}

// fanout calls every target of a multi-target route in parallel and answers
// with one response, the array at mergeFieldPath merged across them. A
// target that fails is counted in the degraded header rather than denying
// the caller what the others returned; only a total failure NACKs.
//
// Merge policy (donor selection, ordering) lives in pkg/merge, not here --
// this file is the executor.
func fanout(ctx *model.StepContext, r *http.Request, w http.ResponseWriter, httpClient *http.Client, responseSteps []definition.ResponseStep, ackSigner *ackSignerStep, cfg FanoutConfig, signNack nackSignerFunc, responseBody *[]byte) {
	targets := ctx.Route.URLs
	mergeFieldPath := ctx.Route.MergeFieldPath

	fail := func(err error) {
		signNack(ctx, err)
		*responseBody = sendNack(ctx, w, err)
	}

	results := callTargets(ctx.Context, ctx.Body, r, httpClient, targets, cfg)

	kept, degraded := collect(ctx, targets, results, responseSteps, mergeFieldPath)
	log.Infof(ctx.Context, "fanout: %d/%d targets contributed, %d degraded, mergeFieldPath=%s", len(kept), len(targets), len(degraded), mergeFieldPath)
	if len(kept) == 0 {
		// Wire error carries only a count; the degraded targets (already logged
		// per-target in collect) stay out of it -- that's the whole point of
		// the count-not-hosts header.
		unreachable := model.NewCodedErr(http.StatusBadGateway, model.CodeUpstreamUnavailable,
			fmt.Errorf("no target could be reached (%d unreachable)", len(degraded)))
		log.Errorf(ctx.Context, unreachable, "fanout: every target unreachable: %s", strings.Join(degraded, ", "))
		fail(unreachable)
		return
	}

	// The reply's context is the caller's own, action flipped -- not a
	// donor's. A target is the caller's proxy for one catalog, not this
	// adapter's identity, so echoing a donor's context would have the merged
	// reply claim to be whichever target happened to answer first.
	requestAction := extractBecknAction(ctx.Body)
	if requestAction == "" {
		err := fmt.Errorf("request context carries no action, cannot build a reply context")
		log.Errorf(ctx.Context, err, "fanout: merge aborted")
		fail(err)
		return
	}

	merged, err := merge.Responses(kept, mergeFieldPath, ctx.Body, "on_"+requestAction)
	if err != nil {
		log.Errorf(ctx.Context, err, "fanout: merge failed across %d kept response(s) at mergeFieldPath=%s", len(kept), mergeFieldPath)
		fail(err)
		return
	}

	// Signed once, over the merged body: per-response signing writes into
	// that upstream's own header map, which fan-out never copies out.
	if ackSigner != nil {
		ctx.ResponseBody = merged
		if err := ackSigner.RunOnResponse(ctx, nil); err != nil {
			log.Errorf(ctx.Context, err, "fanout: signing the merged response failed")
			fail(err)
			return
		}
	}

	if len(degraded) > 0 {
		w.Header().Set(degradedCountHeader, strconv.Itoa(len(degraded)))
	}
	*responseBody = writeJSONResponse(ctx, w, merged)
}

// callTargets calls every target in parallel under one deadline and one
// concurrency cap, returning their answers in target order.
//
// One deadline for the fan-out as a whole, not per target: with a
// concurrency cap, targets run in waves, and a per-target timeout would
// become N waves x timeout.
func callTargets(parentCtx context.Context, body []byte, r *http.Request, httpClient *http.Client, targets []*url.URL, cfg FanoutConfig) []targetResult {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultFanoutTimeout
	}
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = defaultFanoutMaxConcurrency
	}
	log.Infof(parentCtx, "fanout: calling %d target(s), maxConcurrency=%d, timeout=%s", len(targets), concurrency, timeout)

	fanCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	group, groupCtx := errgroup.WithContext(fanCtx)
	group.SetLimit(concurrency)

	// Indexed, never appended: each goroutine owns one cell, so target order
	// survives for the merge with no lock needed.
	results := make([]targetResult, len(targets))
	for i, t := range targets {
		group.Go(func() error {
			results[i] = callTarget(groupCtx, body, r, httpClient, t)
			return nil
		})
	}
	_ = group.Wait() // callTarget never returns an error; failures live in targetResult.

	// A target the deadline caught before it ever got a concurrency slot
	// never wrote its cell.
	for i, t := range targets {
		if results[i].err == nil && results[i].body == nil && results[i].status == 0 {
			results[i] = targetResult{err: fmt.Errorf("calling %s: %w", t, fanCtx.Err())}
		}
	}
	return results
}

// callTarget sends the already-validated body to one target and reads its
// whole answer.
//
// body is ctx.Body, passed down rather than r.Body: r.Body is a stream the
// first target would drain, so each target gets its own bytes.Reader over
// the same backing array instead.
//
// target's query string is used as-is, not re-merged with the inbound
// request's: the router already lays the caller's query over each target's
// own (withRawQuery, in the router plugin) before this ever runs, the same
// way proxy() trusts route.URL as-is for the single-target path. Redoing it
// here was dead work on every fan-out call.
func callTarget(fanCtx context.Context, body []byte, r *http.Request, httpClient *http.Client, target *url.URL) targetResult {
	req, err := http.NewRequestWithContext(fanCtx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return targetResult{err: fmt.Errorf("building request for %s: %w", target, err)}
	}

	// The signature rides along in this copy: the body is identical at every
	// target, so one signature is valid at all of them.
	for name, values := range r.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	// Checked against r.Header, not req.Header: by the time hop-by-hop
	// stripping runs below, the copy has already lost what it strips.
	teTrailers := wantsTeTrailers(r.Header["Te"])
	for _, named := range strings.Split(req.Header.Get("Connection"), ",") {
		if name := strings.TrimSpace(named); name != "" {
			req.Header.Del(name) // RFC 7230 6.1: Connection can name further headers to strip
		}
	}
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	if teTrailers {
		req.Header.Set("Te", "trailers") // re-added per RFC 7230 4.3, same as ReverseProxy does
	}
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			clientIP = prior + ", " + clientIP
		}
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	req.Header.Set("X-Forwarded-Host", r.Host)
	req.Host = target.Host

	// Debug, not log.Request: the body is identical at every target (see the
	// comment on this function), and stdHandler.go already logs it once for
	// the inbound request -- logging it again here per target is pure
	// duplication, N times over for N targets.
	log.Debugf(fanCtx, "fanout: calling %s", target)

	resp, err := httpClient.Do(req)
	if err != nil {
		return targetResult{err: fmt.Errorf("calling %s: %w", target, err)}
	}
	defer resp.Body.Close()

	// +1 so a body exactly at the cap isn't mistaken for a truncated one.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxTargetResponseBytes+1))
	if err != nil {
		return targetResult{err: fmt.Errorf("reading response from %s: %w", target, err)}
	}
	if len(respBody) > maxTargetResponseBytes {
		return targetResult{err: fmt.Errorf("response from %s exceeds %d bytes", target, maxTargetResponseBytes)}
	}
	return targetResult{status: resp.StatusCode, header: resp.Header, body: respBody}
}

// wantsTeTrailers reports whether values (a request's Te header) names
// "trailers", per RFC 7230 4.3.
func wantsTeTrailers(values []string) bool {
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "trailers") {
				return true
			}
		}
	}
	return false
}

// collect turns the per-target answers into the responses worth merging and
// the hosts to report degraded.
//
// Response steps run here, serially, not inside the goroutines that made the
// calls: ackSignerStep writes to the response writer's own header map, and
// running steps concurrently would race that write. The ack signer itself is
// skipped -- it runs once over the merged body in fanout instead.
func collect(ctx *model.StepContext, targets []*url.URL, results []targetResult, responseSteps []definition.ResponseStep, mergeFieldPath string) ([]merge.KeptResponse, []string) {
	// Filtered once, not per target: which steps are the ack signer never
	// changes across a fan-out call, so checking it again for every
	// (target, step) pair below is wasted work that scales with target count.
	steps := nonAckSignerSteps(responseSteps)

	var (
		kept     []merge.KeptResponse
		degraded []string
	)
	for i, res := range results {
		targetURL := targets[i].String()
		switch {
		case res.err != nil:
			log.Errorf(ctx.Context, res.err, "fanout: target %s did not answer, marking degraded", targetURL)
			degraded = append(degraded, targetURL)
			continue
		case res.status < 200 || res.status >= 300:
			log.Warnf(ctx, "fanout: target %s answered %d, excluding it from the merge", targetURL, res.status)
			degraded = append(degraded, targetURL)
			continue
		}

		rctx := &model.ResponseStepContext{StatusCode: res.status, Header: res.header, Body: res.body}
		failed := false
		for _, step := range steps {
			if err := step.RunOnResponse(ctx, rctx); err != nil {
				log.Errorf(ctx.Context, err, "fanout: response step rejected target %s, marking degraded", targetURL)
				degraded = append(degraded, targetURL)
				failed = true
				break
			}
		}
		if failed {
			continue
		}

		items, present, err := merge.ItemsOf(res.body, mergeFieldPath)
		if err != nil {
			log.Errorf(ctx.Context, err, "fanout: target %s (status %d) has an unreadable body at mergeFieldPath=%s, marking degraded", targetURL, res.status, mergeFieldPath)
			degraded = append(degraded, targetURL)
			continue
		}
		kept = append(kept, merge.KeptResponse{Body: res.body, Items: items, HasItems: present})
	}
	return kept, degraded
}

// isAckSigner reports whether a response step is the ack signer, seeing
// through the telemetry wrapper the handler puts around configured steps.
func isAckSigner(step definition.ResponseStep) bool {
	var inner any = step
	if instrumented, ok := step.(*InstrumentedResponseStep); ok {
		inner = instrumented.step
	}
	_, ok := inner.(*ackSignerStep)
	return ok
}

// nonAckSignerSteps filters the ack signer out of responseSteps, once per
// fan-out call rather than once per (target, step) pair inside collect.
func nonAckSignerSteps(responseSteps []definition.ResponseStep) []definition.ResponseStep {
	steps := make([]definition.ResponseStep, 0, len(responseSteps))
	for _, step := range responseSteps {
		if !isAckSigner(step) {
			steps = append(steps, step)
		}
	}
	return steps
}
