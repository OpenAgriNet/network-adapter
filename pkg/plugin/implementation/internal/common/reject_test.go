// reject: a mapper that can refuse a provider's 200.
package common

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// rejectingMapper is stubMapper plus Reject, recording what Reject was given.
type rejectingMapper struct {
	*stubMapper
	rejectErr   error
	rejectInput any
}

func (m *rejectingMapper) Reject(_ context.Context, _ string, input any) error {
	m.rejectInput = input
	return m.rejectErr
}

// assertStatus fails unless err is a CodedErr carrying status.
func assertStatus(t *testing.T, err error, status int) {
	t.Helper()
	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != status {
		t.Fatalf("error = %v, want a coded %d", err, status)
	}
}

// rejectStep is a step whose provider answers body with a 200.
func rejectStep(t *testing.T, mapper definition.Mapper, body string) *Step {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(provider.Close)
	return newStep(t, &stubRegistry{plan: testPlan(provider.URL, http.MethodPost)}, mapper)
}

func TestReject_MapperRefuses_ReturnsItsErrorAndSkipsResponseMapping(t *testing.T) {
	stub := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)}
	mapper := &rejectingMapper{stubMapper: stub,
		rejectErr: model.NewCodedErr(http.StatusBadGateway, "NET_DOWNSTREAM_UNAVAILABLE", errors.New("refused"))}

	stepCtx, err := runStep(t, rejectStep(t, mapper, `{"responseCode":0,"responseMessage":"no"}`), selectBody)
	assertStatus(t, err, http.StatusBadGateway)
	if stub.responseInput != nil {
		t.Error("the response half ran on a refused answer")
	}
	if len(stepCtx.ResponseBody) != 0 {
		t.Errorf("a refused answer produced a response body: %s", stepCtx.ResponseBody)
	}
}

func TestReject_MapperAccepts_MapsResponse(t *testing.T) {
	stub := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)}
	mapper := &rejectingMapper{stubMapper: stub}

	stepCtx, err := runStep(t, rejectStep(t, mapper, `{"responseCode":1}`), selectBody)
	if err != nil {
		t.Fatal(err)
	}
	if string(stepCtx.ResponseBody) != `{"a":1}` {
		t.Errorf("response = %s, want the mapped answer", stepCtx.ResponseBody)
	}
	input, _ := mapper.rejectInput.(map[string]any)
	if input["beckn"] == nil || input["response"].(map[string]any)["responseCode"] != 1.0 {
		t.Errorf("Reject was given %v, want the beckn request and the decoded answer", mapper.rejectInput)
	}
}

func TestReject_MapperWithoutReject_MapsResponseUnchanged(t *testing.T) {
	stub := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"a":1}`)}

	stepCtx, err := runStep(t, rejectStep(t, stub, `{"responseCode":0}`), selectBody)
	if err != nil {
		t.Fatalf("a mapper with no Reject refused: %v", err)
	}
	if !strings.Contains(string(stepCtx.ResponseBody), `"a":1`) {
		t.Errorf("response = %s, want the mapped answer", stepCtx.ResponseBody)
	}
}
