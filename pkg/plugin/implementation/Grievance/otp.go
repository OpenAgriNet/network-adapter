package Grievance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// challengeMethod is the only challenge a caller may answer: an OTP by SMS.
const challengeMethod = "SMS_OTP"

// identityType is how the OTP service is told the identifier is a registration
// number. A grievance is filed against a registration, so it is never a mobile
// or an Aadhaar number here; nothing is inferred from the value's shape.
const identityType = "Ben_id"

// maxVerifyAnswerBytes caps what is read from the verify call: its answer is a
// flag and a sentence.
const maxVerifyAnswerBytes = 64 << 10

// otpVerify spends the farmer's OTP on the OTP realm before an action that
// carries one -- the lodge or the read -- is sent.
//
// It is one call the registry has no action for: a precondition of support
// and status, not an action of its own. It goes to the host the OTP realm's
// action names on the registry (init's), at verifyPath, through the same codec
// and token, so the OTP realm's host lives in one place.
//
// The OTP goes here and nowhere else: it is not in the lodge or the read, it is
// not logged, and it is not echoed.
type otpVerify struct {
	provider   string
	path       string
	actions    map[string]bool
	hostAction string
	codec      *cbcCodec
	client     *http.Client
}

func newOTPVerify(provider, path string, actions map[string]bool, hostAction string, codec *cbcCodec) *otpVerify {
	return &otpVerify{provider: provider, path: path, actions: actions, hostAction: hostAction,
		codec: codec, client: &http.Client{}}
}

// check verifies the exchange's challenge, refusing the call when it fails.
//
//   - challenge or identifier missing: 400 SCH_REQUIRED_FIELD_MISSING. The
//     request half refuses these first; this is the guard behind it.
//   - a method other than SMS_OTP: 400 SCH_INVALID_FORMAT.
//   - the OTP realm cannot be reached or answers non-2xx: 502.
//   - anything but a "True" success flag: 400 BIZ_GENERIC_ERROR -- the farmer's
//     OTP is wrong, expired or already used, and the call it guards is not made.
//     Not a 401: that means a Beckn signature failed to verify.
func (v *otpVerify) check(ctx context.Context, exchange common.Exchange) error {
	identity, method, otp := challengeIn(exchange)
	if identity == "" || otp == "" {
		return model.NewBadReqErr("SCH_REQUIRED_FIELD_MISSING", errors.New(
			"this call needs the registration number and challenge.value, the OTP the farmer received"))
	}
	if method != challengeMethod {
		return model.NewBadReqErr("SCH_INVALID_FORMAT", fmt.Errorf(
			"challenge.method must be %s", challengeMethod))
	}

	body, err := json.Marshal(map[string]string{"Types": identityType, "Values": identity, "OTP": otp})
	if err != nil {
		return err
	}
	sealed, open, err := v.codec.seal(ctx, body)
	if err != nil {
		return err
	}

	answer, err := v.post(ctx, exchange, sealed)
	if err != nil {
		return err
	}
	plain, err := open(ctx, answer)
	if err != nil {
		return err
	}
	if !succeeded(plain) {
		// The portal's own words stay out of the reply and the log: its
		// message can quote the farmer's details back.
		log.Warnf(ctx, "%s refused the OTP", v.provider)
		return model.NewCodedErr(http.StatusBadRequest, "BIZ_GENERIC_ERROR",
			errors.New("the OTP is wrong or has expired"))
	}
	return nil
}

// post sends the sealed verify request to the OTP realm's host.
func (v *otpVerify) post(ctx context.Context, exchange common.Exchange, sealed []byte) ([]byte, error) {
	if exchange.Plan == nil {
		return nil, errors.New("no call plan to find the OTP service on")
	}
	call := exchange.Plan.Actions[v.hostAction]
	base := exchange.Plan.BaseURLFor(v.hostAction)
	endpoint, err := util.BuildEndpoint(base, model.ActionPlan{Method: http.MethodPost, Path: v.path}, nil)
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
	resp, err := v.client.Do(req)
	if err != nil {
		log.Warnf(ctx, "%s's OTP service could not be reached for verification: %v", v.provider, err)
		return nil, refused(fmt.Errorf("%s's OTP service did not answer", v.provider))
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxVerifyAnswerBytes))
	if err != nil {
		return nil, refused(fmt.Errorf("%s's OTP service answer could not be read", v.provider))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		log.Warnf(ctx, "%s's OTP service returned %s for verification", v.provider, resp.Status)
		return nil, refused(fmt.Errorf("%s's OTP service returned %s", v.provider, resp.Status))
	}
	return answer, nil
}

// challengeIn reads the registration number and the challenge from where each
// action carries them: on support's channel, with the registration in
// orderId; on any contract action, in commitmentAttributes.
func challengeIn(exchange common.Exchange) (identity, method, otp string) {
	payload, _ := exchange.Beckn.(map[string]any)
	message, _ := payload["message"].(map[string]any)

	var holder map[string]any
	if support, isSupport := message["support"].(map[string]any); isSupport {
		identity = text(support["orderId"])
		if channels, ok := support["channels"].([]any); ok && len(channels) > 0 {
			holder, _ = channels[0].(map[string]any)
		}
	} else if contract, isContract := message["contract"].(map[string]any); isContract {
		if commitments, ok := contract["commitments"].([]any); ok && len(commitments) > 0 {
			commitment, _ := commitments[0].(map[string]any)
			holder, _ = commitment["commitmentAttributes"].(map[string]any)
			identity = text(holder["enrolmentId"])
		}
	}
	if challenge, ok := holder["challenge"].(map[string]any); ok {
		method, otp = text(challenge["method"]), text(challenge["value"])
	}
	return identity, method, otp
}

// succeeded reads the OTP realm's success flag. "True" is the only success:
// a missing flag, an empty answer or any other value fails closed.
func succeeded(plain []byte) bool {
	var answer map[string]any
	if json.Unmarshal(plain, &answer) != nil {
		return false
	}
	for _, key := range []string{"Rsponce", "Responce", "Status", "status"} {
		if value, ok := answer[key]; ok {
			return strings.EqualFold(text(value), "true")
		}
	}
	return false
}

// text is a JSON value as a trimmed string, or empty when it is not one.
func text(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}
