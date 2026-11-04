package common

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

func TestAuthKeysFromRowRenamesRefsToEnvNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth map[string]any
		want map[string]string
	}{
		{"none", map[string]any{"scheme": "none"},
			map[string]string{"authScheme-p": "none"}},
		{"basic", map[string]any{"scheme": "basic",
			"basic": map[string]any{"usernameRef": "U", "passwordRef": "P"}},
			map[string]string{"authScheme-p": "basic", "usernameEnv-p": "U", "passwordEnv-p": "P"}},
		{"header", map[string]any{"scheme": "header",
			"header": map[string]any{"headerName": "Authorization", "headerValueRef": "MAUSAMGRAM_AUTH"}},
			map[string]string{"authScheme-p": "header", "headerName-p": "Authorization",
				"headerValueEnv-p": "MAUSAMGRAM_AUTH"}},
		{"query", map[string]any{"scheme": "query",
			"query": map[string]any{"queryName": "key", "queryValueRef": "K"}},
			map[string]string{"authScheme-p": "query", "queryName-p": "key", "queryValueEnv-p": "K"}},
		{"oauth2", map[string]any{"scheme": "oauth2",
			"oauth2": map[string]any{"tokenUrl": "https://issuer.example/token",
				"clientIdRef": "ID", "clientSecretRef": "SECRET"}},
			map[string]string{"authScheme-p": "oauth2", "tokenUrl-p": "https://issuer.example/token",
				"clientIdEnv-p": "ID", "clientSecretEnv-p": "SECRET"}},
		{"tokenQuery", map[string]any{"scheme": "tokenQuery",
			"tokenQuery": map[string]any{"tokenUrl": "https://agm.example/token",
				"tokenUserField": "access_name", "tokenUserRef": "AGMARKNET_ACCESS_NAME",
				"tokenSecretField": "password", "tokenSecretRef": "AGMARKNET_PASSWORD",
				"tokenResponseField": "token", "queryName": "token", "tokenTtl": "600s"}},
			map[string]string{"authScheme-p": "tokenQuery", "tokenUrl-p": "https://agm.example/token",
				"tokenUserField-p": "access_name", "tokenUserEnv-p": "AGMARKNET_ACCESS_NAME",
				"tokenSecretField-p": "password", "tokenSecretEnv-p": "AGMARKNET_PASSWORD",
				"tokenResponseField-p": "token", "queryName-p": "token", "tokenTtl-p": "600s"}},
		{"tokenHeader", map[string]any{"scheme": "tokenHeader",
			"tokenHeader": map[string]any{"tokenUrl": "https://t.example/token",
				"tokenUserField": "user", "tokenUserRef": "U", "tokenSecretField": "pass",
				"tokenSecretRef": "S", "tokenResponseField": "jwt", "headerName": "X-Token",
				"tokenExpiryField": "exp"}},
			map[string]string{"authScheme-p": "tokenHeader", "tokenUrl-p": "https://t.example/token",
				"tokenUserField-p": "user", "tokenUserEnv-p": "U", "tokenSecretField-p": "pass",
				"tokenSecretEnv-p": "S", "tokenResponseField-p": "jwt", "headerName-p": "X-Token",
				"tokenExpiryField-p": "exp"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := authKeysFromRow("p", tt.auth)
			if err != nil {
				t.Fatalf("authKeysFromRow() returned an unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("keys = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthKeysFromRowIgnoresBlocksOfOtherSchemes(t *testing.T) {
	t.Parallel()

	got, err := authKeysFromRow("p", map[string]any{
		"scheme": "query",
		"query":  map[string]any{"queryName": "key", "queryValueRef": "NEW"},
		"header": map[string]any{"headerName": "Authorization", "headerValueRef": "OLD"},
	})
	if err != nil {
		t.Fatalf("authKeysFromRow() returned an unexpected error: %v", err)
	}
	want := map[string]string{"authScheme-p": "query", "queryName-p": "key", "queryValueEnv-p": "NEW"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want only the query block's %v", got, want)
	}
}

func TestProfileFromRowRefusesWhatTheConfigWouldRefuse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		auth    map[string]any
		mention string
	}{
		{"misspelled field", map[string]any{"scheme": "header", "header": map[string]any{
			"headerName": "A", "headerValueRef": "X", "headerValu": "typo"}}, "headerValu"},
		{"scheme without its block", map[string]any{"scheme": "oauth2"}, "oauth2"},
		{"no scheme", map[string]any{"header": map[string]any{"headerName": "A"}}, "scheme"},
		{"value that is not text", map[string]any{"scheme": "header", "header": map[string]any{
			"headerName": 5, "headerValueRef": "X"}}, "headerName"},
		{"block that is not an object", map[string]any{"scheme": "header", "header": "Authorization"}, "header"},
		{"bad token lifetime", map[string]any{"scheme": "tokenQuery", "tokenQuery": map[string]any{
			"tokenUrl": "https://t.example/token", "tokenUserField": "u", "tokenUserRef": "U",
			"tokenSecretField": "s", "tokenSecretRef": "S", "tokenResponseField": "t",
			"queryName": "token", "tokenTtl": "soon"}}, "tokenTtl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := profileFromRow("mausamgram", tt.auth)
			if err == nil {
				t.Fatal("profileFromRow() accepted a bad auth block")
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %q does not mention %q", err, tt.mention)
			}
		})
	}
}

func rowPlanWithHeaderAuth(baseURL, header, valueEnv string) *model.ProviderRecord {
	plan := testPlan(baseURL, http.MethodGet)
	plan.Auth = map[string]any{"scheme": "header",
		"header": map[string]any{"headerName": header, "headerValueRef": valueEnv}}
	return plan
}

func TestRunUsesTheRowsAuthOverTheConfigs(t *testing.T) {
	t.Setenv("TEST_ROW_AUTH", "row-secret")

	var got string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Row")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer provider.Close()

	registry := &stubRegistry{plan: rowPlanWithHeaderAuth(provider.URL, "X-Row", "TEST_ROW_AUTH")}
	step := newStep(t, registry, &stubMapper{requestResult: []byte(`{}`), responseResult: []byte(`{"ok":true}`)})

	if _, err := runStep(t, step, selectBody); err != nil {
		t.Fatalf("Run() returned an unexpected error: %v", err)
	}
	if got != "row-secret" {
		t.Errorf("provider received X-Row %q, want the row's credential", got)
	}
}

func TestRunRefusesARowWhoseAuthIsBroken(t *testing.T) {
	t.Parallel()

	plan := testPlan("http://127.0.0.1:1", http.MethodGet)
	plan.Auth = map[string]any{"scheme": "oauth2"}
	step := newStep(t, &stubRegistry{plan: plan}, &stubMapper{requestResult: []byte(`{}`)})

	_, err := runStep(t, step, selectBody)
	if err == nil {
		t.Fatal("Run() served a row whose auth block is broken")
	}
	if !strings.Contains(err.Error(), testBindingKey) {
		t.Errorf("error %q does not name the binding", err)
	}
}

func TestAuthForKeepsOneAuthenticatorUntilTheRowChanges(t *testing.T) {
	t.Parallel()

	step := newStep(t, &stubRegistry{}, &stubMapper{})
	plan := rowPlanWithHeaderAuth("http://unused", "X-Row", "A")

	first, err := step.authFor(context.Background(), plan)
	if err != nil {
		t.Fatalf("authFor() returned an unexpected error: %v", err)
	}
	again, err := step.authFor(context.Background(), rowPlanWithHeaderAuth("http://unused", "X-Row", "A"))
	if err != nil {
		t.Fatalf("authFor() returned an unexpected error: %v", err)
	}
	if first != again {
		t.Error("the same row built a second authenticator, losing any token the first held")
	}

	changed, err := step.authFor(context.Background(), rowPlanWithHeaderAuth("http://unused", "X-Row", "B"))
	if err != nil {
		t.Fatalf("authFor() returned an unexpected error: %v", err)
	}
	if changed == first {
		t.Error("a changed row kept the old authenticator")
	}
	if changed.cfg.HeaderValueEnv != "B" {
		t.Errorf("header value env = %q, want the changed row's B", changed.cfg.HeaderValueEnv)
	}
}

func TestAuthForFallsBackToTheConfigWhenTheRowHasNone(t *testing.T) {
	t.Parallel()

	step := newStep(t, &stubRegistry{}, &stubMapper{})
	got, err := step.authFor(context.Background(), testPlan("http://unused", http.MethodGet))
	if err != nil {
		t.Fatalf("authFor() returned an unexpected error: %v", err)
	}
	if got != step.auth[testProvider] {
		t.Error("a row without auth did not use the config's profile")
	}
	if got.cfg.Scheme != util.AuthSchemeNone {
		t.Errorf("scheme = %q, want the config's none", got.cfg.Scheme)
	}
}

func TestRedactionCoversARowsCredential(t *testing.T) {
	t.Setenv("TEST_ROW_RED", "secret-from-the-row")

	step := newStep(t, &stubRegistry{}, &stubMapper{})
	if _, err := step.authFor(context.Background(), rowPlanWithHeaderAuth("http://unused", "X-Row", "TEST_ROW_RED")); err != nil {
		t.Fatalf("authFor() returned an unexpected error: %v", err)
	}

	got := step.redactString("provider said no: secret-from-the-row")
	if strings.Contains(got, "secret-from-the-row") {
		t.Errorf("the row's credential survived redaction: %s", got)
	}
}
