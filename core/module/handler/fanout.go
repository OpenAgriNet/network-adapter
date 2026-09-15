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
	defaultFanoutMaxConcurrency = 8
	defaultFanoutTimeout        = 10 * time.Second
	degradedHeader              = "X-Beckn-Degraded"
)

const (
	contextKey  = "context"
	messageKey  = "message"
	catalogsKey = "catalogs"
	limitParam  = "limit"
	offsetParam = "offset"
)

// hopByHopHeaders are connection-scoped and must not be forwarded to a target.
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
	"Content-Length",
}

// targetResult is one target's answer, or the reason there isn't one.
type targetResult struct {
	status int
	header http.Header
	body   []byte
	err    error
}

type keptResponse struct {
	body        []byte
	catalogs    []json.RawMessage
	hasCatalogs bool
}

// fanout calls every target of a multi-target route in parallel and answers with one merged response.
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

// fanoutQuery validates query parameters, stripping offset and extracting limit.
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
		return out, 0, false, nil
	}
	return out, limit, true, nil
}

// callTargets invokes all targets concurrently subject to timeout and concurrency limits.
func callTargets(ctx *model.StepContext, r *http.Request, httpClient *http.Client, targets []*url.URL, query url.Values, cfg FanoutConfig) []targetResult {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultFanoutTimeout
	}
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = defaultFanoutMaxConcurrency
	}

	fanCtx, cancel := context.WithTimeout(ctx.Context, timeout)
	defer cancel()

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

// callTarget forwards the payload to a single target URL.
func callTarget(fanCtx context.Context, ctx *model.StepContext, r *http.Request, httpClient *http.Client, target *url.URL, query url.Values) targetResult {
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

	for name, values := range r.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
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

// collect filters target results and runs response validation steps.
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

// isAckSigner checks if a response step is an ack signer.
func isAckSigner(step definition.ResponseStep) bool {
	var inner any = step
	if instrumented, ok := step.(*InstrumentedResponseStep); ok {
		inner = instrumented.step
	}
	_, ok := inner.(*ackSignerStep)
	return ok
}

// mergeCatalogs merges catalog payloads from kept responses into a single response envelope.
func mergeCatalogs(kept []keptResponse, limit int, hasLimit bool) ([]byte, error) {
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

// interleave round-robins catalogs across networks for balanced result representation.
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

// dedupe drops duplicate catalogs by ID, preserving order of first appearance.
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

// catalogID extracts the catalog ID from a raw JSON catalog object.
func catalogID(catalog json.RawMessage) string {
	var fields struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(catalog, &fields); err != nil {
		return ""
	}
	return fields.ID
}

// catalogsOf extracts catalog array items from a response envelope body.
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
