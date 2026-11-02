package envelope

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// maxAnswerBytes caps the answers to calls made here and through Post.
const maxAnswerBytes = 64 << 10

// NewClient is the client for calls made beside the main one: it refuses an
// off-host redirect, as common's does, since they carry the same identifiers.
func NewClient() *http.Client {
	return &http.Client{CheckRedirect: util.RefuseOffHostRedirect}
}

// Post POSTs a sealed body to base+path once, within timeoutMs (0: the
// default). Unreachable or non-2xx: 502. For a capability's own further calls.
func Post(ctx context.Context, client *http.Client, provider, what, base, path string,
	timeoutMs int, sealed []byte) ([]byte, error) {
	return postSealed(ctx, client, provider, what, base, path, model.ActionPlan{TimeoutMs: timeoutMs}, sealed)
}

// postSealed POSTs a sealed body to base+path once, within its own budget.
// Unreachable or non-2xx: 502.
func postSealed(ctx context.Context, client *http.Client, provider, what, base, path string,
	call model.ActionPlan, sealed []byte) ([]byte, error) {
	endpoint, err := util.BuildEndpoint(base, model.ActionPlan{Method: http.MethodPost, Path: path}, nil)
	if err != nil {
		return nil, err
	}
	timeout, _ := util.Budget(call)
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	httpCtx, httpSpan := tracer.Start(attemptCtx, "http "+path)
	defer httpSpan.End()

	req, err := http.NewRequestWithContext(httpCtx, http.MethodPost, endpoint, bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf(ctx, "%s could not be reached for %s: %v", provider, what, err)
		httpSpan.RecordError(err)
		httpSpan.SetStatus(codes.Error, "transport_error")
		return nil, refused(fmt.Errorf("%s did not answer %s", provider, what))
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return nil, refused(fmt.Errorf("%s's answer to %s could not be read", provider, what))
	}
	log.Infof(ctx, "POST %s -> %s, %d bytes", endpoint, resp.Status, len(answer))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Warnf(ctx, "%s returned %s for %s", provider, resp.Status, what)
		httpSpan.SetStatus(codes.Error, resp.Status)
		return nil, refused(fmt.Errorf("%s returned %s for %s", provider, resp.Status, what))
	}
	return answer, nil
}
