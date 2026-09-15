package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

const (
	// defaultFanoutMaxConcurrency caps simultaneous target calls when the
	// config names no number.
	defaultFanoutMaxConcurrency = 8

	// defaultFanoutTimeout is the budget for a whole fan-out when the config
	// names none. It must stay well below the server's own write timeout: a
	// fan-out that outlives that resets the connection instead of returning
	// the merged partial answer.
	defaultFanoutTimeout = 10 * time.Second
)

// degradedHeader reports HOW MANY targets did not contribute to a fan-out
// answer, so a caller can tell an incomplete result from a complete one.
//
// A header rather than a body member, for the reason the discovery service
// gives for its own: the v2 on_discover action is additionalProperties:false
// with `catalogs` as its only property, so a `degraded` key inside `message`
// would not be an extension but a response that fails its own schema.
//
// A COUNT AND NOT THE HOSTS. The targets are internal addresses, and this
// adapter is the one a deployment may put an Ingress in front of -- naming them
// would teach any caller the cluster's topology from a single partial failure.
// Which target failed, and why, is in the logs, where the operator is.
const degradedHeader = "X-Beckn-Degraded"

const (
	contextKey  = "context"  // the envelope member every response echoes
	messageKey  = "message"  // the envelope member holding the action
	catalogsKey = "catalogs" // the on_discover action's only member
	limitParam  = "limit"
	offsetParam = "offset"
)

// hopByHopHeaders are connection-scoped and must not be forwarded to a target.
// httputil.ReverseProxy strips these itself; an http.Client.Do does not, so a
// fan-out has to do it.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	// Content-Length is set from the body reader by http.NewRequest; a copied
	// one describes the inbound request and may disagree.
	"Content-Length",
}

// targetResult is one target's answer, or the reason there isn't one.
type targetResult struct {
	status int
	header http.Header
	body   []byte
	err    error
}

// keptResponse is one target's answer after it has been accepted: the envelope
// it sent, and the catalogs already read out of it.
//
// The catalogs are read in collect rather than in the merge so that a body that
// cannot be read is a DEGRADED TARGET like any other failure, instead of a
// merge error that denies the caller every other network's answer. A target
// answering 200 with a broken body is a broken target, not a broken fan-out.
type keptResponse struct {
	body     []byte
	catalogs []json.RawMessage
	// hasCatalogs records that the response carried a message.catalogs member,
	// as opposed to carrying none. The distinction decides two things the
	// merge cannot get right without it: which response may donate the
	// envelope, and whether this action is one a fan-out can merge at all.
	hasCatalogs bool
}

// fanout calls every target of a multi-target route in parallel and answers the
// caller with one merged response.
//
// It exists because the single-target path cannot do this. That path is an
// httputil.ReverseProxy, whose whole contract is that one ResponseWriter
// becomes one upstream's response -- it streams the body straight through and
// has nowhere to hold a second. So a fan-out stops proxying and starts calling:
// it makes its own requests and authors its own response, which means the work
// ReverseProxy did silently (hop-by-hop stripping, X-Forwarded-Host, the host
// rewrite) is done here explicitly.
//
// PARTIAL SUCCESS IS SUCCESS. A target that fails is named in the degraded
// header, and its absence is visible to the caller, but it does not deny them
// the catalogs the other networks returned. Only a total failure is a NACK:
// answering 200 with an empty catalog list when nothing was reached would be
// indistinguishable from networks that genuinely have no matches.
func fanout(ctx *model.StepContext, r *http.Request, w http.ResponseWriter, httpClient *http.Client, responseSteps []definition.ResponseStep, ackSigner *ackSignerStep, cfg FanoutConfig, signNack nackSignerFunc, responseBody *[]byte) {
	targets := ctx.Route.URLs

	query, limit, hasLimit, err := fanoutQuery(r.URL.Query())
	if err != nil {
		signNack(ctx, err)
		*responseBody = sendNack(ctx, w, err)
		return
	}

	results := callTargets(ctx, r, httpClient, targets, query, cfg)

	kept, degraded := collect(ctx, targets, results, responseSteps)
	if len(kept) == 0 {
		err := fmt.Errorf("no target answered: %s", strings.Join(degraded, ", "))
		signNack(ctx, err)
		*responseBody = sendNack(ctx, w, err)
		return
	}

	merged, err := mergeCatalogs(kept, limit, hasLimit)
	if err != nil {
		log.Errorf(ctx.Context, err, "fanout: merge failed: %v", err)
		signNack(ctx, err)
		*responseBody = sendNack(ctx, w, err)
		return
	}

	// The merged body is what the caller receives, so it is what the signature
	// has to cover. The per-response signing the proxy path relies on cannot do
	// that here twice over: it would sign one upstream's body, and it writes the
	// header into THAT upstream's response header, which a fan-out never copies
	// out -- so the answer would go to the caller unsigned. Signing here instead
	// covers the merged bytes and writes to ctx.RespHeader, which is the
	// response writer's own header map.
	if ackSigner != nil {
		ctx.ResponseBody = merged
		if err := ackSigner.RunOnResponse(ctx, nil); err != nil {
			log.Errorf(ctx.Context, err, "fanout: signing the merged response failed: %v", err)
			signNack(ctx, err)
			*responseBody = sendNack(ctx, w, err)
			return
		}
	}

	// Before writeJSONResponse: that writes the status line, and a header set
	// after it is a header nobody receives.
	if len(degraded) > 0 {
		w.Header().Set(degradedHeader, strconv.Itoa(len(degraded)))
	}
	*responseBody = writeJSONResponse(ctx, w, merged)
}

// fanoutQuery resolves the outbound query string and the page size to apply to
// the merged result.
//
// OFFSET CANNOT BE FANNED OUT. Forwarded as-is it means "skip N" inside EACH
// network independently, so the second page skips N per network, returns rows
// N+1..N+limit from each, and never fetches rows N+1..N*targets of the merged
// ordering. That is not page two of anything, so it is refused rather than
// answered wrongly, and offset is stripped from what the targets are sent.
//
// limit is forwarded unchanged -- each network pages its own retrieval with it
// -- and applied again to the merged list on the way out, so a caller asking
// for 20 gets 20 rather than 20 per network. A caller that sent no limit gets
// no truncation: the default page size belongs to the discovery service, and
// inventing one here would silently drop catalogs.
func fanoutQuery(in url.Values) (url.Values, int, bool, error) {
	out := url.Values{}
	for k, v := range in {
		out[k] = v
	}

	if raw := out.Get(offsetParam); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			return nil, 0, false, model.NewBadReqErr("SCH_INVALID_FORMAT",
				fmt.Errorf("offset is not a whole number"))
		}
		if offset != 0 {
			return nil, 0, false, model.NewBadReqErr("SCH_INVALID_FORMAT",
				fmt.Errorf("offset is not supported when an action is served by several networks: paging past the first page cannot be expressed across them"))
		}
	}
	out.Del(offsetParam)

	raw := out.Get(limitParam)
	if raw == "" {
		return out, 0, false, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return nil, 0, false, model.NewBadReqErr("SCH_INVALID_FORMAT",
			fmt.Errorf("limit is not a whole number"))
	}
	if limit <= 0 {
		// Non-positive means "the service's default" to the discovery service,
		// and this cannot know what that is -- so forward it and do not
		// truncate.
		return out, 0, false, nil
	}
	return out, limit, true, nil
}

// callTargets calls every target in parallel under one deadline and one
// concurrency cap, and returns their answers in target order.
func callTargets(ctx *model.StepContext, r *http.Request, httpClient *http.Client, targets []*url.URL, query url.Values, cfg FanoutConfig) []targetResult {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultFanoutTimeout
	}
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = defaultFanoutMaxConcurrency
	}

	// ONE budget for the fan-out as a whole, not one per target. Per-target
	// timeouts do not compose: with a concurrency cap the targets run in waves,
	// and N waves of a per-target timeout is N times the bound anyone intended.
	// When this expires, in-flight calls return, queued ones never start, and
	// the caller is answered with whatever arrived.
	fanCtx, cancel := context.WithTimeout(ctx.Context, timeout)
	defer cancel()

	// Indexed, never appended: each goroutine owns exactly one cell, so there
	// is no shared write and target order survives for the merge.
	results := make([]targetResult, len(targets))
	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t *url.URL) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-fanCtx.Done():
				results[i].err = fanCtx.Err()
				return
			}
			results[i] = callTarget(fanCtx, ctx, r, httpClient, t, query)
		}(i, t)
	}
	wg.Wait()

	return results
}

// callTarget sends the already-validated body to one target and reads its whole
// answer.
//
// The body comes from ctx.Body and not from r.Body: r.Body is a stream that can
// be read once, so the first target would drain it and every other would send
// an empty payload -- a request that looks valid and returns nothing. ctx.Body
// is already in memory, so each target gets its own bytes.Reader over the same
// backing array, which costs an offset rather than a copy.
//
// It is also the right payload: post-validation, post-mediation, and identical
// at every target, which is what lets one signature serve all of them.
func callTarget(fanCtx context.Context, ctx *model.StepContext, r *http.Request, httpClient *http.Client, target *url.URL, query url.Values) targetResult {
	// The target's own configured query survives; the inbound one is laid over
	// it. offset is deleted last and unconditionally, so it cannot re-enter
	// from either side -- it means "skip N in EACH network", which is what
	// fanoutQuery refused the request over in the first place.
	u := *target
	merged := u.Query()
	for key, values := range query {
		merged[key] = values
	}
	merged.Del(offsetParam)
	u.RawQuery = merged.Encode()

	req, err := http.NewRequestWithContext(fanCtx, r.Method, u.String(), bytes.NewReader(ctx.Body))
	if err != nil {
		return targetResult{err: fmt.Errorf("building request for %s: %w", &u, err)}
	}

	// The signature rides along in this copy. signStep signs ctx.Body with no
	// recipient in the signing string, and the body is identical at every
	// target, so one signature is valid at all of them -- no per-target signing
	// and no repeated keyset lookups.
	for name, values := range r.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	// RFC 7230 6.1: the Connection header NAMES further headers that are
	// connection-scoped. httputil.ReverseProxy strips those as well, so a
	// fan-out that dropped only the fixed list below would forward a header
	// the proxy path removes. Done before the fixed list, which includes
	// Connection itself.
	for _, named := range strings.Split(req.Header.Get("Connection"), ",") {
		if name := strings.TrimSpace(named); name != "" {
			req.Header.Del(name)
		}
	}
	for _, h := range hopByHopHeaders {
		req.Header.Del(h)
	}
	req.Header.Set("X-Forwarded-Host", r.Host)
	req.Host = u.Host

	log.Request(fanCtx, req, ctx.Body)

	resp, err := httpClient.Do(req)
	if err != nil {
		return targetResult{err: fmt.Errorf("calling %s: %w", &u, err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return targetResult{err: fmt.Errorf("reading response from %s: %w", &u, err)}
	}
	return targetResult{status: resp.StatusCode, header: resp.Header, body: body}
}

// collect turns the per-target answers into the bodies worth merging and the
// hosts to report degraded.
//
// THE RESPONSE STEPS RUN HERE, SERIALLY, and not inside the goroutines that
// made the calls. ackSignerStep writes to ctx.RespHeader, and ctx.RespHeader is
// the response writer's own header map -- running the steps concurrently is a
// data race on the response being written. The network calls are what is worth
// parallelising; verifying a handful of small bodies afterwards is not.
func collect(ctx *model.StepContext, targets []*url.URL, results []targetResult, responseSteps []definition.ResponseStep) ([]keptResponse, []string) {
	var (
		kept     []keptResponse
		degraded []string
	)
	for i, res := range results {
		host := targets[i].Host
		switch {
		case res.err != nil:
			log.Errorf(ctx.Context, res.err, "fanout: target %s did not answer: %v", host, res.err)
			degraded = append(degraded, host)
			continue
		case res.status < 200 || res.status >= 300:
			log.Warnf(ctx, "fanout: target %s answered %d, excluding it from the merge", host, res.status)
			degraded = append(degraded, host)
			continue
		}

		rctx := &model.ResponseStepContext{
			StatusCode: res.status,
			Header:     res.header,
			Body:       res.body,
		}
		failed := false
		for _, step := range responseSteps {
			// The ack signer signs OUR answer, not an upstream's, so it runs
			// once over the merged body rather than once per response. Every
			// other response step -- signature validation above all -- is about
			// this upstream and belongs here.
			if isAckSigner(step) {
				continue
			}
			if err := step.RunOnResponse(ctx, rctx); err != nil {
				log.Errorf(ctx.Context, err, "fanout: response step rejected target %s: %v", host, err)
				degraded = append(degraded, host)
				failed = true
				break
			}
		}
		if failed {
			continue
		}

		catalogs, present, err := catalogsOf(res.body)
		if err != nil {
			log.Errorf(ctx.Context, err, "fanout: target %s answered %d with a body that cannot be read: %v", host, res.status, err)
			degraded = append(degraded, host)
			continue
		}
		kept = append(kept, keptResponse{body: res.body, catalogs: catalogs, hasCatalogs: present})
	}
	return kept, degraded
}

// isAckSigner reports whether a response step is the ack signer, seeing through
// the telemetry wrapper the handler puts around configured steps.
func isAckSigner(step definition.ResponseStep) bool {
	var inner any = step
	if instrumented, ok := step.(*InstrumentedResponseStep); ok {
		inner = instrumented.step
	}
	_, ok := inner.(*ackSignerStep)
	return ok
}

// mergeCatalogs folds several on_discover envelopes into one.
//
// The FIRST successful response supplies the envelope and its context is kept
// whole. Every network echoes the transaction and message ids of the request
// that was fanned out to all of them, so one echo is as good as another, and
// building a context here would mean this handler learning a protocol it
// otherwise only forwards.
func mergeCatalogs(kept []keptResponse, limit int, hasLimit bool) ([]byte, error) {
	// The donor is the first response that actually carried catalogs, not
	// simply the first kept one. Two things ride on that.
	//
	// A response with no catalogs member is not a discovery answer, and if
	// NOTHING carried one this is not a discovery action: the rule has been
	// pointed at several targets for an action whose replies cannot be merged.
	// Writing an empty catalogs array into, say, an ACK would produce a body
	// that fails its own schema -- the v2 actions are additionalProperties:
	// false -- and it would be signed over those bytes. Refused loudly instead.
	//
	// And a response can be HTTP 200 while carrying a Beckn error envelope.
	// Such a response is kept (it is not a transport failure) but must not
	// donate the envelope, or an unrelated network's error member would ride
	// out alongside everyone else's catalogs.
	donor := -1
	for i, k := range kept {
		if k.hasCatalogs {
			donor = i
			break
		}
	}
	if donor < 0 {
		return nil, fmt.Errorf("no response carried %q: fan-out merges catalogs, so a rule with several targets is only meaningful for an action whose replies carry them", catalogsKey)
	}

	var donorEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(kept[donor].body, &donorEnvelope); err != nil {
		return nil, fmt.Errorf("donor response is not a JSON object: %w", err)
	}

	message := map[string]json.RawMessage{}
	if raw, ok := donorEnvelope[messageKey]; ok {
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, fmt.Errorf("donor response has a non-object %q: %w", messageKey, err)
		}
	}

	// Rebuilt rather than edited in place: only context and message belong in
	// the answer, so anything else the donor carried does not travel.
	envelope := map[string]json.RawMessage{}
	if raw, ok := donorEnvelope[contextKey]; ok {
		envelope[contextKey] = raw
	}

	perNetwork := make([][]json.RawMessage, 0, len(kept))
	for _, k := range kept {
		perNetwork = append(perNetwork, k.catalogs)
	}

	merged := dedupe(interleave(perNetwork))
	if hasLimit && len(merged) > limit {
		merged = merged[:limit]
	}
	if merged == nil {
		merged = []json.RawMessage{}
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encoding merged catalogs: %w", err)
	}
	message[catalogsKey] = encoded

	encodedMessage, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encoding merged message: %w", err)
	}
	envelope[messageKey] = encodedMessage

	return json.Marshal(envelope)
}

// interleave takes one catalog from each network in turn until all are drained.
//
// Straight concatenation would order the merged list by position in the routing
// config, so with a limit the first network fills the page and the last may
// never be seen at all -- a caller asking for 20 results from four networks
// would get twenty from the first one. Round-robin gives each network
// proportional presence in whatever the caller actually sees. It is not a
// relevance ranking: nothing in the on_discover envelope carries a score to
// rank by, and inventing an order that looks like relevance would be worse than
// an order that plainly is not.
func interleave(perNetwork [][]json.RawMessage) []json.RawMessage {
	total := 0
	longest := 0
	for _, catalogs := range perNetwork {
		total += len(catalogs)
		if len(catalogs) > longest {
			longest = len(catalogs)
		}
	}

	out := make([]json.RawMessage, 0, total)
	for i := 0; i < longest; i++ {
		for _, catalogs := range perNetwork {
			if i < len(catalogs) {
				out = append(out, catalogs[i])
			}
		}
	}
	return out
}

// dedupe drops repeats of a catalog id, keeping the first occurrence.
//
// Two networks can carry the same catalog -- a provider on both, or one network
// relaying another's. A catalog with no readable id is kept rather than
// dropped: an id is how duplicates are recognised, and its absence means
// unknown, not duplicate.
func dedupe(catalogs []json.RawMessage) []json.RawMessage {
	seen := make(map[string]bool, len(catalogs))
	out := catalogs[:0]
	for _, c := range catalogs {
		id := catalogID(c)
		if id == "" {
			out = append(out, c)
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, c)
	}
	return out
}

// catalogID reads a catalog's id without decoding the rest of it.
func catalogID(catalog json.RawMessage) string {
	var fields struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(catalog, &fields); err != nil {
		return ""
	}
	return fields.ID
}

// catalogsOf pulls one response's catalogs out without decoding the catalogs
// themselves: they are re-emitted byte for byte, so a member this build does
// not know about is not dropped on the way through.
//
// A response carrying no catalogs contributes nothing rather than failing the
// merge -- a network with no matches is a valid answer, not an error.
func catalogsOf(body []byte) ([]json.RawMessage, bool, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("not a JSON object: %w", err)
	}
	raw, ok := envelope[messageKey]
	if !ok {
		return nil, false, nil
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, false, fmt.Errorf("non-object %q: %w", messageKey, err)
	}
	rawCatalogs, ok := message[catalogsKey]
	if !ok {
		return nil, false, nil
	}
	var catalogs []json.RawMessage
	if err := json.Unmarshal(rawCatalogs, &catalogs); err != nil {
		return nil, true, fmt.Errorf("non-array %q: %w", catalogsKey, err)
	}
	return catalogs, true, nil
}
