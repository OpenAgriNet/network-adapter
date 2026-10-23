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
)

// maxAnswerBytes caps the OTP verify and follow-up answers.
const maxAnswerBytes = 64 << 10

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

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		log.Warnf(ctx, "%s could not be reached for %s: %v", provider, what, err)
		return nil, refused(fmt.Errorf("%s did not answer %s", provider, what))
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return nil, refused(fmt.Errorf("%s's answer to %s could not be read", provider, what))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Warnf(ctx, "%s returned %s for %s", provider, resp.Status, what)
		return nil, refused(fmt.Errorf("%s returned %s for %s", provider, resp.Status, what))
	}
	return answer, nil
}
