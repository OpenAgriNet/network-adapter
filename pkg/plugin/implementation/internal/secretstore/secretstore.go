// Package secretstore reads provider secrets from OpenBao, holding each answer
// in memory for a while so a rotated secret is picked up without a restart.
package secretstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

// DefaultCacheTTL is how long a read entry is reused when the config sets none.
const DefaultCacheTTL = 5 * time.Minute

// maxRefreshSkew is the most a login is renewed ahead of its expiry, so a token
// never dies between being checked and being used.
const maxRefreshSkew = time.Minute

// requestTimeout bounds one call to the secret store.
const requestTimeout = 10 * time.Second

// ErrNotFound means the store has no entry by that name.
var ErrNotFound = errors.New("no such entry in the secret store")

// Login is how the store proves who this adapter is.
type Login interface {
	name() string
	path() string
	body() (map[string]any, error)
	validate() error
}

// AppRole logs in with a role id and secret id, for deployments outside Kubernetes.
type AppRole struct {
	RoleID, SecretID string
	Mount            string // where AppRole is enabled; default "approle"
}

func (a AppRole) name() string { return "AppRole" }
func (a AppRole) path() string { return "auth/" + mountOr(a.Mount, "approle") + "/login" }
func (a AppRole) body() (map[string]any, error) {
	return map[string]any{"role_id": a.RoleID, "secret_id": a.SecretID}, nil
}
func (a AppRole) validate() error {
	if a.RoleID == "" || a.SecretID == "" {
		return errors.New("AppRole login needs both a role id and a secret id")
	}
	return nil
}

// Kubernetes logs in with the pod's service-account token, read on each login
// because Kubernetes rotates it.
type Kubernetes struct {
	Role, TokenFile string
	Mount           string // where Kubernetes login is enabled; default "kubernetes"
}

func (k Kubernetes) name() string { return "Kubernetes" }
func (k Kubernetes) path() string { return "auth/" + mountOr(k.Mount, "kubernetes") + "/login" }
func (k Kubernetes) body() (map[string]any, error) {
	jwt, err := os.ReadFile(k.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading the service-account token: %w", err)
	}
	return map[string]any{"role": k.Role, "jwt": strings.TrimSpace(string(jwt))}, nil
}
func (k Kubernetes) validate() error {
	if k.Role == "" || k.TokenFile == "" {
		return errors.New("Kubernetes login needs a role and a token file")
	}
	return nil
}

func mountOr(mount, fallback string) string {
	if mount = strings.Trim(mount, "/"); mount != "" {
		return mount
	}
	return fallback
}

// Config says where the secrets are and how to log in.
type Config struct {
	URL      string // e.g. http://openbao:8200
	Path     string // KV v2 data path holding one entry per provider
	Login    Login
	CacheTTL time.Duration

	HTTPClient *http.Client
	now        func() time.Time
}

// Store reads entries from the secret store. Safe for concurrent use.
type Store struct {
	cfg Config

	mu         sync.Mutex
	token      string
	tokenUntil time.Time // zero: the token does not expire
	entries    map[string]cachedEntry
}

type cachedEntry struct {
	values map[string]string
	until  time.Time
}

// New checks cfg and fills in its defaults. Nothing is called until Value.
func New(cfg Config) (*Store, error) {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	cfg.Path = strings.Trim(cfg.Path, "/")
	switch {
	case cfg.URL == "":
		return nil, errors.New("secret store: a URL is required")
	case cfg.Path == "":
		return nil, errors.New("secret store: the path holding provider entries is required")
	case cfg.Login == nil:
		return nil, errors.New("secret store: a login method is required")
	}
	if err := cfg.Login.validate(); err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Store{cfg: cfg, entries: map[string]cachedEntry{}}, nil
}

// Value is key in entry. Errors name the entry and key, never a value.
func (s *Store) Value(ctx context.Context, entry, key string) (string, error) {
	values, err := s.entry(ctx, entry)
	if err != nil {
		return "", err
	}
	value, found := values[key]
	if !found {
		return "", fmt.Errorf("secret store entry %s has no key %s", entry, key)
	}
	return value, nil
}

// Held is the last value read for key, even past its cache time, without
// calling the store: for hiding secrets in error messages.
func (s *Store) Held(entry, key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, found := s.entries[entry].values[key]
	return value, found
}

func (s *Store) entry(ctx context.Context, entry string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached, found := s.entries[entry]; found && s.cfg.now().Before(cached.until) {
		return cached.values, nil
	}
	values, err := s.read(ctx, entry)
	if err != nil {
		return nil, err
	}
	s.entries[entry] = cachedEntry{values: values, until: s.cfg.now().Add(s.cfg.CacheTTL)}
	log.Infof(ctx, "Read secret store entry %s: %d key(s), reused for %s", entry, len(values), s.cfg.CacheTTL)
	return values, nil
}

// read fetches entry, logging in again once if the store refuses the token: it
// may have been revoked or expired early.
func (s *Store) read(ctx context.Context, entry string) (map[string]string, error) {
	if err := s.loginIfNeeded(ctx); err != nil {
		return nil, err
	}
	values, status, err := s.get(ctx, entry)
	if status == http.StatusForbidden {
		log.Warnf(ctx, "Secret store refused the token reading %s; logging in again", entry)
		s.token = ""
		if err := s.loginIfNeeded(ctx); err != nil {
			return nil, err
		}
		values, status, err = s.get(ctx, entry)
	}
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, entry)
	case status != http.StatusOK:
		return nil, fmt.Errorf("secret store answered HTTP %d reading %s", status, entry)
	}
	return values, nil
}

func (s *Store) get(ctx context.Context, entry string) (map[string]string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/%s/%s", s.cfg.URL, s.cfg.Path, url.PathEscape(entry)), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Vault-Token", s.token)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s from the secret store: %w", entry, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}

	var answer struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("secret store answer for %s is not readable: %w", entry, err)
	}
	return textValues(entry, answer.Data.Data)
}

func textValues(entry string, raw map[string]any) (map[string]string, int, error) {
	values := make(map[string]string, len(raw))
	for key, value := range raw {
		text, isText := value.(string)
		if !isText {
			return nil, http.StatusOK, fmt.Errorf("secret store entry %s: key %s is not text", entry, key)
		}
		values[key] = text
	}
	return values, http.StatusOK, nil
}

func (s *Store) loginIfNeeded(ctx context.Context) error {
	now := s.cfg.now()
	if s.token != "" && (s.tokenUntil.IsZero() || now.Before(s.tokenUntil)) {
		return nil
	}
	body, err := s.cfg.Login.body()
	if err != nil {
		return fmt.Errorf("secret store %s login: %w", s.cfg.Login.name(), err)
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/%s", s.cfg.URL, s.cfg.Login.path()), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("secret store %s login: %w", s.cfg.Login.name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("secret store refused the %s login: HTTP %d", s.cfg.Login.name(), resp.StatusCode)
	}

	var answer struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil || answer.Auth.ClientToken == "" {
		return fmt.Errorf("secret store %s login answered without a token", s.cfg.Login.name())
	}
	s.token = answer.Auth.ClientToken
	s.tokenUntil = renewAt(now, time.Duration(answer.Auth.LeaseDuration)*time.Second)
	log.Infof(ctx, "Logged in to the secret store with %s, token valid for %s",
		s.cfg.Login.name(), time.Duration(answer.Auth.LeaseDuration)*time.Second)
	return nil
}

// renewAt is when to log in again: a little before lease ends. Zero lease: never.
func renewAt(now time.Time, lease time.Duration) time.Time {
	if lease <= 0 {
		return time.Time{}
	}
	return now.Add(lease - min(maxRefreshSkew, lease/10))
}
