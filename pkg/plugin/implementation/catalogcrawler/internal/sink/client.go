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
type Client struct{ hc *http.Client }

// NewClient builds a push transport with the given timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{hc: &http.Client{Timeout: timeout}}
}

// Push POSTs a /publish body. 200 with an ACCEPTED on_publish verdict is an
// ack; anything else is a non-ack with the reason. /publish answers 200 even
// for a catalogue it did not accept -- the verdict is in message.results -- so
// the status code alone would read a PARTIAL (indexed with resources missing)
// as success. An answer carrying no results keeps the 200 rule.
func (c *Client) Push(ctx context.Context, endpoint string, body []byte) (BatchOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return BatchOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return BatchOutcome{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
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

// verdict reads the first on_publish result's status and reason. readable is
// false only for a non-empty body that is not an on_publish answer; an empty
// body, or one carrying no results, is readable with an empty status.
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
	first := answer.Message.Results[0]
	reason = first.Reason
	if reason == "" && len(first.Errors) > 0 {
		reason = first.Errors[0].Message
	}
	return first.Status, reason, true
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
