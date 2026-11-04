package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openBaoAnswering serves one login and one provider entry, and records the
// login body it was sent.
func openBaoAnswering(t *testing.T, gotLogin *map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/login"):
			_ = json.NewDecoder(r.Body).Decode(gotLogin)
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "t", "lease_duration": 3600}})
		case r.URL.Path == "/v1/secret/data/oan/local/providers/mausamgram":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"MAUSAMGRAM_AUTH": "from-bao"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func storeSettings(url string, extra map[string]string) map[string]string {
	settings := map[string]string{"secretStoreUrl": url, "secretStorePath": "secret/data/oan/local/providers"}
	for k, v := range extra {
		settings[k] = v
	}
	return settings
}

func TestSecretStoreFromIsNoneWithoutAURL(t *testing.T) {
	t.Parallel()

	store, err := SecretStoreFrom(map[string]string{"bindingKeys": testBindingKey})
	if err != nil || store != nil {
		t.Errorf("SecretStoreFrom() = %v, %v; want no store and no error", store, err)
	}
}

func TestSecretStoreFromLogsInWithAppRoleIdsFromTheNamedVariables(t *testing.T) {
	t.Setenv("TEST_BAO_ROLE_ID", "role-1")
	t.Setenv("TEST_BAO_SECRET_ID", "secret-1")
	var login map[string]any
	url := openBaoAnswering(t, &login)

	store, err := SecretStoreFrom(storeSettings(url, map[string]string{"secretStoreLogin": "approle",
		"secretStoreRoleIdEnv": "TEST_BAO_ROLE_ID", "secretStoreSecretIdEnv": "TEST_BAO_SECRET_ID"}))
	if err != nil {
		t.Fatalf("SecretStoreFrom() = %v", err)
	}
	got, err := store.Value(context.Background(), "mausamgram", "MAUSAMGRAM_AUTH")
	if err != nil || got != "from-bao" {
		t.Fatalf("Value() = %q, %v; want from-bao", got, err)
	}
	if login["role_id"] != "role-1" || login["secret_id"] != "secret-1" {
		t.Errorf("login sent %v, want the ids from the named variables", login)
	}
}

func TestSecretStoreFromLogsInWithTheKubernetesRole(t *testing.T) {
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("pod-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	var login map[string]any
	url := openBaoAnswering(t, &login)

	store, err := SecretStoreFrom(storeSettings(url, map[string]string{"secretStoreLogin": "kubernetes",
		"secretStoreRole": "network-adapter", "secretStoreTokenFile": jwt}))
	if err != nil {
		t.Fatalf("SecretStoreFrom() = %v", err)
	}
	if _, err := store.Value(context.Background(), "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if login["role"] != "network-adapter" || login["jwt"] != "pod-jwt" {
		t.Errorf("login sent %v, want the role and the file's token", login)
	}
}

func TestSecretStoreFromRefusesAnIncompleteSetup(t *testing.T) {
	t.Setenv("TEST_BAO_ROLE_ID", "role-1")
	t.Setenv("TEST_BAO_EMPTY", "")
	approle := map[string]string{"secretStoreLogin": "approle",
		"secretStoreRoleIdEnv": "TEST_BAO_ROLE_ID", "secretStoreSecretIdEnv": "TEST_BAO_EMPTY"}

	tests := []struct {
		name     string
		settings map[string]string
		mention  string
	}{
		{"no login", storeSettings("http://bao:8200", nil), "secretStoreLogin"},
		{"unknown login", storeSettings("http://bao:8200", map[string]string{"secretStoreLogin": "ldap"}), "ldap"},
		{"no path", map[string]string{"secretStoreUrl": "http://bao:8200", "secretStoreLogin": "kubernetes",
			"secretStoreRole": "r"}, "path"},
		{"approle variable not named", storeSettings("http://bao:8200", map[string]string{"secretStoreLogin": "approle",
			"secretStoreRoleIdEnv": "TEST_BAO_ROLE_ID"}), "secretStoreSecretIdEnv"},
		{"approle variable empty", storeSettings("http://bao:8200", approle), "TEST_BAO_EMPTY"},
		{"kubernetes without a role", storeSettings("http://bao:8200", map[string]string{"secretStoreLogin": "kubernetes"}), "role"},
		{"bad cache time", storeSettings("http://bao:8200", map[string]string{"secretStoreLogin": "kubernetes",
			"secretStoreRole": "r", "secretStoreCacheTtl": "soon"}), "secretStoreCacheTtl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SecretStoreFrom(tt.settings)
			if err == nil {
				t.Fatal("SecretStoreFrom() accepted an incomplete setup")
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %q does not mention %q", err, tt.mention)
			}
		})
	}
}

func TestParseStepConfigBuildsTheSecretStore(t *testing.T) {
	t.Parallel()

	cfg, err := ParseStepConfig(map[string]string{"bindingKeys": testBindingKey,
		"secretStoreUrl": "http://bao:8200", "secretStorePath": "secret/data/oan/local/providers",
		"secretStoreLogin": "kubernetes", "secretStoreRole": "network-adapter"})
	if err != nil {
		t.Fatalf("ParseStepConfig() = %v", err)
	}
	if cfg.SecretStore == nil {
		t.Error("ParseStepConfig() left SecretStore unset")
	}
}
