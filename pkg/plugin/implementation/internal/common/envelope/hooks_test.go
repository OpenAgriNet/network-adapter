package envelope

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// testHooks stand in for a capability's: the challenge sits where the
// grievance packs put it, and "yes" is a "True" flag. Enough for this
// package's tests; the real ones are the Grievance package's.
var testHooks = Hooks{
	Challenge: func(exchange common.Exchange) (map[string]any, error) {
		holder := testHolder(exchange.Beckn)
		kind, id := "", ""
		if v, _ := holder["enrolmentId"].(string); v != "" {
			kind, id = "Ben_id", v
		} else if applicant, ok := holder["applicant"].(map[string]any); ok {
			if v, _ := applicant["mobile"].(string); v != "" {
				kind, id = "Mobile", v
			}
		}
		challenge, _ := holder["challenge"].(map[string]any)
		response, _ := challenge["response"].(map[string]any)
		otp, _ := response["value"].(string)
		if kind == "" || otp == "" {
			return nil, model.NewBadReqErr("SCH_REQUIRED_FIELD_MISSING", errors.New("no identifier or OTP"))
		}
		return map[string]any{"Types": kind, "Values": id, "OTP": otp}, nil
	},
	Succeeded: func(plain []byte) bool {
		var answer map[string]any
		if json.Unmarshal(plain, &answer) != nil {
			return false
		}
		for _, key := range []string{"Rsponce", "Responce", "Status", "status"} {
			if value, ok := answer[key].(string); ok {
				return strings.EqualFold(strings.TrimSpace(value), "true")
			}
		}
		return false
	},
}

// testHolder is support's channel or the commitment's attributes.
func testHolder(beckn any) map[string]any {
	payload, _ := beckn.(map[string]any)
	message, _ := payload["message"].(map[string]any)
	if support, ok := message["support"].(map[string]any); ok {
		if channels, ok := support["channels"].([]any); ok && len(channels) > 0 {
			holder, _ := channels[0].(map[string]any)
			return holder
		}
		return nil
	}
	contract, _ := message["contract"].(map[string]any)
	if commitments, ok := contract["commitments"].([]any); ok && len(commitments) > 0 {
		commitment, _ := commitments[0].(map[string]any)
		holder, _ := commitment["commitmentAttributes"].(map[string]any)
		return holder
	}
	return nil
}
