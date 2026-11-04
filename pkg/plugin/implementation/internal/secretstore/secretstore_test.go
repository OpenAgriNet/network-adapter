package secretstore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const providersPath = "secret/data/oan/local/providers"

// fakeBao serves AppRole and Kubernetes login and KV v2 reads, counting each.
type fakeBao struct {
	mu        sync.Mutex
	entries   map[string]map[string]string
	tokenTTL  int
	logins    int
	reads     int
	rejectNth int // answer this read with 403, once
	lastLogin map[string]any
	issued    []string
}

func (f *fakeBao) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/login"):
			f.logins++
			_ = json.NewDecoder(r.Body).Decode(&f.lastLogin)
			token := "token-" + string(rune('0'+f.logins))
			f.issued = append(f.issued, token)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{"client_token": token, "lease_duration": f.tokenTTL}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/"+providersPath+"/"):
			f.reads++
			if f.reads == f.rejectNth || !f.knows(r.Header.Get("X-Vault-Token")) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			entry, found := f.entries[strings.TrimPrefix(r.URL.Path, "/v1/"+providersPath+"/")]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"data": entry, "metadata": map[string]any{"version": 3}}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
}

func (f *fakeBao) knows(token string) bool {
	for _, issued := range f.issued {
		if issued == token {
			return true
		}
	}
	return false
}

func (f *fakeBao) counts() (logins, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, f.reads
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newFake(t *testing.T) (*fakeBao, string) {
	t.Helper()
	fake := &fakeBao{tokenTTL: 3600, entries: map[string]map[string]string{
		"mausamgram": {"MAUSAMGRAM_AUTH": "tok-1"},
	}}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func newStore(t *testing.T, url string, c *clock, login Login) *Store {
	t.Helper()
	store, err := New(Config{URL: url, Path: providersPath, Login: login, CacheTTL: time.Minute, now: c.Now})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return store
}

func appRole() Login { return AppRole{RoleID: "role-1", SecretID: "secret-1"} }

func TestValueLogsInWithAppRoleAndReadsTheKey(t *testing.T) {
	fake, url := newFake(t)
	store := newStore(t, url, &clock{now: time.Now()}, appRole())

	got, err := store.Value(context.Background(), "mausamgram", "MAUSAMGRAM_AUTH")
	if err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if got != "tok-1" {
		t.Errorf("Value() = %q, want tok-1", got)
	}
	if fake.lastLogin["role_id"] != "role-1" || fake.lastLogin["secret_id"] != "secret-1" {
		t.Errorf("login sent %v, want the AppRole ids", fake.lastLogin)
	}
}

func TestValueLogsInWithTheKubernetesTokenFile(t *testing.T) {
	fake, url := newFake(t)
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("pod-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newStore(t, url, &clock{now: time.Now()}, Kubernetes{Role: "network-adapter", TokenFile: jwt})

	if _, err := store.Value(context.Background(), "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if fake.lastLogin["role"] != "network-adapter" || fake.lastLogin["jwt"] != "pod-jwt" {
		t.Errorf("login sent %v, want the role and the file's token", fake.lastLogin)
	}
}

func TestValueIsServedFromMemoryUntilItsTimeIsUp(t *testing.T) {
	fake, url := newFake(t)
	c := &clock{now: time.Now()}
	store := newStore(t, url, c, appRole())
	ctx := context.Background()

	for range 3 {
		if _, err := store.Value(ctx, "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
			t.Fatalf("Value() = %v", err)
		}
	}
	if _, reads := fake.counts(); reads != 1 {
		t.Errorf("read OpenBao %d times within the cache time, want 1", reads)
	}

	fake.mu.Lock()
	fake.entries["mausamgram"]["MAUSAMGRAM_AUTH"] = "tok-2"
	fake.mu.Unlock()
	c.advance(time.Minute + time.Second)
	got, err := store.Value(ctx, "mausamgram", "MAUSAMGRAM_AUTH")
	if err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if got != "tok-2" {
		t.Errorf("Value() after the cache time = %q, want the rotated tok-2", got)
	}
}

func TestValueReusesItsLoginUntilTheTokenNearlyExpires(t *testing.T) {
	fake, url := newFake(t)
	fake.tokenTTL = 120
	c := &clock{now: time.Now()}
	store := newStore(t, url, c, appRole())
	ctx := context.Background()

	if _, err := store.Value(ctx, "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
		t.Fatalf("Value() = %v", err)
	}
	c.advance(61 * time.Second)
	if _, err := store.Value(ctx, "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if logins, _ := fake.counts(); logins != 1 {
		t.Errorf("logged in %d times while the token was fresh, want 1", logins)
	}

	c.advance(61 * time.Second)
	if _, err := store.Value(ctx, "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if logins, _ := fake.counts(); logins != 2 {
		t.Errorf("logged in %d times after the token's lifetime, want 2", logins)
	}
}

func TestValueLogsInAgainOnceWhenARead403s(t *testing.T) {
	fake, url := newFake(t)
	fake.rejectNth = 1
	store := newStore(t, url, &clock{now: time.Now()}, appRole())

	got, err := store.Value(context.Background(), "mausamgram", "MAUSAMGRAM_AUTH")
	if err != nil {
		t.Fatalf("Value() = %v", err)
	}
	if got != "tok-1" {
		t.Errorf("Value() = %q, want tok-1", got)
	}
	if logins, reads := fake.counts(); logins != 2 || reads != 2 {
		t.Errorf("logins %d, reads %d; want one fresh login and one retried read", logins, reads)
	}
}

func TestValueReportsWhatIsMissingWithoutAnyValue(t *testing.T) {
	_, url := newFake(t)
	store := newStore(t, url, &clock{now: time.Now()}, appRole())
	ctx := context.Background()

	_, err := store.Value(ctx, "skymet", "SKYMET_AUTH")
	if err == nil || !strings.Contains(err.Error(), "skymet") {
		t.Errorf("Value() for an absent entry = %v, want an error naming the entry", err)
	}

	_, err = store.Value(ctx, "mausamgram", "MAUSAMGRAM_OTHER")
	if err == nil || !strings.Contains(err.Error(), "MAUSAMGRAM_OTHER") {
		t.Errorf("Value() for an absent key = %v, want an error naming the key", err)
	}
	if err != nil && strings.Contains(err.Error(), "tok-1") {
		t.Errorf("error %q carries another key's value", err)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]Config{
		"no url":             {Path: providersPath, Login: appRole()},
		"no path":            {URL: "http://bao:8200", Login: appRole()},
		"no login":           {URL: "http://bao:8200", Path: providersPath},
		"approle half":       {URL: "http://bao:8200", Path: providersPath, Login: AppRole{RoleID: "r"}},
		"kubernetes no role": {URL: "http://bao:8200", Path: providersPath, Login: Kubernetes{TokenFile: "/x"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(cfg); err == nil {
				t.Error("New() accepted an incomplete config")
			}
		})
	}
}

func TestHeldReturnsOnlyWhatIsAlreadyInMemory(t *testing.T) {
	fake, url := newFake(t)
	c := &clock{now: time.Now()}
	store := newStore(t, url, c, appRole())

	if _, held := store.Held("mausamgram", "MAUSAMGRAM_AUTH"); held {
		t.Error("Held() reported a value before anything was read")
	}
	if logins, reads := fake.counts(); logins != 0 || reads != 0 {
		t.Errorf("Held() called the store: %d logins, %d reads", logins, reads)
	}

	if _, err := store.Value(context.Background(), "mausamgram", "MAUSAMGRAM_AUTH"); err != nil {
		t.Fatalf("Value() = %v", err)
	}
	c.advance(time.Hour)
	got, held := store.Held("mausamgram", "MAUSAMGRAM_AUTH")
	if !held || got != "tok-1" {
		t.Errorf("Held() = %q, %v after expiry; want the last value read, kept for redaction", got, held)
	}
}
