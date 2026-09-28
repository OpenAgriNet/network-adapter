package sink

// client.go — HTTP transport that POSTs catalog/publish bodies to the
// operator-configured provider adapter /publish endpoint, plus the per-batch
// BatchOutcome and the Rollup helper that collapses those outcomes into one
// SinkOutcome.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// BatchOutcome is the result of pushing one batch of a catalog.
type BatchOutcome struct {
	Acked      bool
	HTTPStatus int
	Reason     string
}

// Client POSTs catalog/publish bodies to the (trusted, operator-configured)
// provider adapter endpoint. No SSRF guard -- the endpoint is config, not
// attacker input.
type Client struct {
	hc *http.Client

	// Log receives one line per push. nil means slog.Default().
	Log *slog.Logger
}

func (c *Client) logger() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// NewClient builds a push transport with the given timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{hc: &http.Client{Timeout: timeout}}
}

// Push POSTs a /publish body. 200 with an ACCEPTED on_publish verdict is an
// ack; anything else is a non-ack with the reason. /publish answers 200 even
// for a catalog it did not accept -- the verdict is in message.results -- so
// the status code alone would read a PARTIAL (indexed with resources missing)
// as success. An answer carrying no results keeps the 200 rule.
func (c *Client) Push(ctx context.Context, endpoint string, body []byte) (BatchOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return BatchOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// One line per call, with the path and never the query.
	began := time.Now()
	fields := []any{"method", http.MethodPost, "path", req.URL.Path, "bytes", len(body)}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.logger().InfoContext(ctx, "publish call", append(fields,
			"status", 0, "duration", time.Since(began).Round(time.Millisecond).String())...)
		return BatchOutcome{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	c.logger().InfoContext(ctx, "publish call", append(fields,
		"status", resp.StatusCode, "duration", time.Since(began).Round(time.Millisecond).String())...)
	out := BatchOutcome{Acked: resp.StatusCode == http.StatusOK, HTTPStatus: resp.StatusCode}
	if !out.Acked {
		out.Reason = strings.TrimSpace(string(respBody))
		return out, nil
	}
	status, reason, readable := verdict(respBody)
	if !readable {
		// A PARTIAL cut off mid-body, or a proxy's HTML page, must not read
		// as a success just because it came back 200.
		out.Acked = false
		out.Reason = "unreadable on_publish answer: " + firstLine(respBody)
		return out, nil
	}
	if status != "" && !strings.EqualFold(status, "ACCEPTED") {
		out.Acked = false
		out.Reason = strings.ToUpper(status)
		if reason != "" {
			out.Reason += ": " + reason
		}
	}
	return out, nil
}

// maxAnswerBytes bounds what is read of an answer: large enough for an
// on_publish listing many per-resource errors -- exactly the PARTIAL case, so
// truncating it would turn a failure into an unreadable body -- and small
// enough that a misbehaving server cannot exhaust memory.
const maxAnswerBytes = 1 << 20

// verdict reads the on_publish results and returns the first one that is not
// ACCEPTED -- or the first result when all are. readable is false only for a
// non-empty body that is not an on_publish answer; an empty body, or one
// carrying no results, is readable with an empty status.
//
// Every result is judged: an answer listing one ACCEPTED and one PARTIAL is a
// PARTIAL, and reading only the first would call it a success.
func verdict(body []byte) (status, reason string, readable bool) {
	var answer struct {
		Message struct {
			Results []struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			} `json:"results"`
		} `json:"message"`
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", "", true
	}
	if json.Unmarshal(body, &answer) != nil {
		return "", "", false
	}
	if len(answer.Message.Results) == 0 {
		return "", "", true
	}
	for _, result := range answer.Message.Results {
		if strings.EqualFold(result.Status, "ACCEPTED") {
			continue
		}
		reason = result.Reason
		if reason == "" && len(result.Errors) > 0 {
			reason = result.Errors[0].Message
		}
		return result.Status, reason, true
	}
	return answer.Message.Results[0].Status, "", true
}

// firstLine trims an answer down to something a log line can carry.
func firstLine(body []byte) string {
	text := strings.TrimSpace(string(body))
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	const limit = 200
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

// Rollup collapses per-batch push outcomes into one crawlmanager.SinkOutcome:
// accepted only if every batch was acked, with the failed batches' reasons
// joined for diagnostics.
func Rollup(outcomes []BatchOutcome) (accepted bool, reason string) {
	var reasons []string
	for _, o := range outcomes {
		if !o.Acked {
			reasons = append(reasons, o.Reason)
		}
	}
	if len(reasons) == 0 {
		return true, ""
	}
	return false, strings.Join(reasons, "; ")
}
