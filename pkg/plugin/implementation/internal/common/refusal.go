// A mapping refusing the call: either half may answer with the reserved
// _error field instead of a document.
package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// errorField is the reserved field a mapping half returns instead of a
// document when the call must be refused. Reserved like _local, so it cannot
// collide with a field a provider or the network defines.
//
// On the request half it refuses before the provider is called, with a code
// the mapping names -- required checks cannot name one. On the response half
// it answers what the provider's reply means, such as nothing on file.
const errorField = "_error"

// refusal is what a mapping writes under _error. Status absent means 400.
type refusal struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// refusalIn returns the error a mapped document carries under _error, or nil
// when it carries none.
func refusalIn(mapped []byte) error {
	var document map[string]json.RawMessage
	if json.Unmarshal(mapped, &document) != nil {
		// Not an object, so not a refusal. Whether it is a usable document is
		// the caller's question.
		return nil
	}
	raw, refused := document[errorField]
	if !refused {
		return nil
	}
	var r refusal
	if err := json.Unmarshal(raw, &r); err != nil || r.Message == "" {
		return fmt.Errorf("a mapping refused the call with an unreadable %s; it needs status, code and message", errorField)
	}
	return r.err()
}

// err classifies a refusal as the mapping asked. 202 is not a fault but an
// answer with nothing in it, so it is an ACK carrying the reason.
func (r refusal) err() error {
	switch r.Status {
	case 0, http.StatusBadRequest:
		return model.NewBadReqErr(r.Code, errors.New(r.Message))
	case http.StatusAccepted:
		return model.NewAckNoCallbackErr(model.StatusACK, &model.Error{Code: r.Code, Message: r.Message})
	default:
		return model.NewCodedErr(r.Status, r.Code, errors.New(r.Message))
	}
}
