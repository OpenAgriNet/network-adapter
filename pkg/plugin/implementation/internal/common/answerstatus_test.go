package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

const noStockBody = `{"statusCode":409,"message":"Data Not Found","data":[]}`

func answerStatusStep(t *testing.T, status int, answerStatuses []int, mapper *stubMapper) *Step {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, noStockBody)
	}))
	t.Cleanup(upstream.Close)

	return newStep(t, &stubRegistry{plan: testPlan(upstream.URL, http.MethodPost)}, mapper,
		func(c *Config) {
			c.setProviderAuth(AuthProfile{Scheme: util.AuthSchemeNone, AnswerStatuses: answerStatuses})
		})
}

func TestRunMapsAConfiguredAnswerStatus(t *testing.T) {
	t.Parallel()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"mapped":true}`)}
	step := answerStatusStep(t, http.StatusConflict, []int{http.StatusConflict}, mapper)

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	input, ok := mapper.responseInput.(map[string]any)
	if !ok {
		t.Fatalf("response mapping did not run; input = %T", mapper.responseInput)
	}
	response, _ := input["response"].(map[string]any)
	if response["message"] != "Data Not Found" {
		t.Errorf("response mapping saw %v, want the provider's 409 body", input["response"])
	}
}

func TestRunStillFailsAStatusThatIsNotConfigured(t *testing.T) {
	t.Parallel()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	step := answerStatusStep(t, http.StatusConflict, nil, mapper)

	_, err := runStep(t, step, selectBody)
	assertStatus(t, err, http.StatusBadGateway)
	if mapper.responseInput != nil {
		t.Error("response mapping ran for a status nobody configured as an answer")
	}
}

func TestRunStillFailsAnotherStatusWhenOneIsConfigured(t *testing.T) {
	t.Parallel()

	mapper := &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{}`)}
	step := answerStatusStep(t, http.StatusNotFound, []int{http.StatusConflict}, mapper)

	_, err := runStep(t, step, selectBody)
	assertStatus(t, err, http.StatusBadGateway)
	if mapper.responseInput != nil {
		t.Error("response mapping ran for a 404 when only 409 is an answer")
	}
}

func TestParseProviderAuthReadsAnswerStatuses(t *testing.T) {
	t.Parallel()

	profiles, err := ParseProviderAuth(map[string]string{
		"authScheme-sathi":     "none",
		"answerStatuses-sathi": "409, 404",
	})
	if err != nil {
		t.Fatalf("ParseProviderAuth() error: %v", err)
	}
	if err := profiles["sathi"].validate(); err != nil {
		t.Fatalf("validate() error: %v", err)
	}
	if got := profiles["sathi"].AnswerStatuses; !reflect.DeepEqual(got, []int{409, 404}) {
		t.Errorf("AnswerStatuses = %v, want [409 404]", got)
	}
}

func TestNewRefusesAnAnswerStatusOutsideFourHundreds(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"conflict", "200", "503", "409,"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			profile := AuthProfile{Provider: testProvider, Scheme: util.AuthSchemeNone, AnswerStatusesRaw: raw}
			cfg := &Config{BindingKeys: []string{testBindingKey}, AuthByProvider: map[string]*AuthProfile{testProvider: &profile}}
			_, _, err := New(context.Background(), &stubRegistry{}, &stubMapper{}, nil, cfg)
			if err == nil {
				t.Fatalf("expected answerStatuses %q to be refused", raw)
			}
			if !strings.Contains(err.Error(), "answerStatuses") {
				t.Errorf("error %q should name answerStatuses", err)
			}
		})
	}
}
