// Package registrycache keeps the registry's active records in the cache, one
// list per entity, loaded up front and reloaded each TTL.
package registrycache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// maxResponseBytes caps a registry answer; a larger one is refused.
const maxResponseBytes = util.DefaultMaxResponseBytes

// ErrNotCached reports that an entity's list is not in the cache.
var ErrNotCached = errors.New("registry list not cached")

// Config configures a Store.
type Config struct {
	// URL is the registry base including the API version, e.g. http://registry:8081/api/v1.
	URL string
	// TTL is how long an entity's active records stay cached. Zero disables caching.
	TTL time.Duration
	// Preload names the entities loaded up front, e.g. Participant.
	Preload []string
}

// Store reads registry records through the cache.
type Store struct {
	url     string
	ttl     time.Duration
	check   time.Duration
	preload []string
	// stale marks a preload entity to reload at the next check.
	stale map[string]*atomic.Bool
	cache definition.Cache
	http  *http.Client
}

// New returns a Store. cache may be nil, which disables caching.
func New(cache definition.Cache, cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, errors.New("registrycache: url is required")
	}
	stale := make(map[string]*atomic.Bool, len(cfg.Preload))
	for _, entity := range cfg.Preload {
		stale[entity] = new(atomic.Bool)
	}
	return &Store{
		url:     strings.TrimSuffix(cfg.URL, "/"),
		ttl:     cfg.TTL,
		check:   time.Minute,
		preload: cfg.Preload,
		stale:   stale,
		cache:   cache,
		http:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Refresh reloads entity's list at the next check. Only the background load
// writes the list.
func (s *Store) Refresh(entity string) {
	if flag, ok := s.stale[entity]; ok {
		flag.Store(true)
	}
}

// Start loads each preload entity now and every TTL until stopped. Each minute
// it also reloads a failed, missing or refreshed entity.
func (s *Store) Start(ctx context.Context) (stop func()) {
	if !s.caching() || len(s.preload) == 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	for _, entity := range s.preload {
		go func() {
			var due time.Time // zero: load now
			for {
				refresh := s.stale[entity].Swap(false)
				if refresh || !time.Now().Before(due) || s.missing(ctx, entity) {
					// Reload a check before the list expires.
					start := time.Now()
					if s.load(ctx, entity) {
						due = start.Add(max(s.ttl-s.check, s.check))
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.check):
				}
			}
		}()
	}
	return cancel
}

// missing reports whether entity's list is absent. An unreadable cache is not
// missing: reloading would not refill it.
func (s *Store) missing(ctx context.Context, entity string) bool {
	raw, err := s.cache.Get(ctx, s.listKey(entity))
	return err == nil && raw == ""
}

// load caches entity's active records, reporting whether the registry answered.
func (s *Store) load(ctx context.Context, entity string) bool {
	records, err := s.search(ctx, entity, map[string]any{})
	if err != nil {
		log.Warnf(ctx, "Failed to preload registry entity %s, retrying in %s: %v", entity, s.check, err)
		return false
	}
	all := activeOf(records)
	s.storeList(ctx, entity, all)
	log.Infof(ctx, "Preloaded %d active of %d %s records", len(all), len(records), entity)
	return true
}

// List returns the cached active records of entity whose fields match (equal,
// or contained in a list field), or ErrNotCached. It never asks the registry.
func (s *Store) List(ctx context.Context, entity string, match map[string]string) ([]json.RawMessage, error) {
	all, err := s.cached(ctx, entity)
	if err != nil {
		return nil, err
	}
	return matching(all, match), nil
}

// cached returns entity's cached active records.
func (s *Store) cached(ctx context.Context, entity string) ([]json.RawMessage, error) {
	if !s.caching() {
		return nil, ErrNotCached
	}
	raw, err := s.cache.Get(ctx, s.listKey(entity))
	if err != nil {
		return nil, err
	}
	var all []json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &all) != nil {
		return nil, ErrNotCached
	}
	return all, nil
}

func (s *Store) storeList(ctx context.Context, entity string, all []json.RawMessage) {
	if !s.caching() {
		return
	}
	raw, err := json.Marshal(all)
	if err == nil {
		err = s.cache.Set(ctx, s.listKey(entity), string(raw), s.ttl)
	}
	if err != nil {
		log.Warnf(ctx, "Failed to cache registry list %s: %v", s.listKey(entity), err)
	}
}

// listKey includes the registry URL, so registries sharing a Redis keep apart.
func (s *Store) listKey(entity string) string {
	return fmt.Sprintf("registry_%s_%s_all", s.url, entity)
}

func (s *Store) caching() bool {
	return s.cache != nil && s.ttl > 0
}

// search asks the registry for entity's records; empty filters match all.
func (s *Store) search(ctx context.Context, entity string, filters map[string]any) ([]json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"filters": filters})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/%s/search", s.url, entity), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry search failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("registry search failed: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("registry answer exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry search failed with status %s", resp.Status)
	}

	return decode(data)
}

// decode accepts both shapes the registry answers with: a bare array, or a
// {"data": [...]} envelope.
func decode(data []byte) ([]json.RawMessage, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err == nil {
		return records, nil
	}
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("unreadable registry answer: %w", err)
	}
	return envelope.Data, nil
}

// activeOf returns the active records, never nil, so an empty list caches as [].
func activeOf(records []json.RawMessage) []json.RawMessage {
	all := []json.RawMessage{}
	for _, record := range records {
		if active(record) {
			all = append(all, record)
		}
	}
	return all
}

func matching(records []json.RawMessage, match map[string]string) []json.RawMessage {
	var found []json.RawMessage
	for _, record := range records {
		if matches(record, match) {
			found = append(found, record)
		}
	}
	return found
}

// matches reports whether every field of match equals the record's field, or
// is contained in it when the field is a list of strings.
func matches(record json.RawMessage, match map[string]string) bool {
	if len(match) == 0 {
		return true
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(record, &r) != nil {
		return false
	}
	for field, want := range match {
		var value string
		if json.Unmarshal(r[field], &value) == nil {
			if value != want {
				return false
			}
			continue
		}
		var values []string
		if json.Unmarshal(r[field], &values) != nil || !slices.Contains(values, want) {
			return false
		}
	}
	return true
}

func active(record json.RawMessage) bool {
	var r struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(record, &r) == nil && strings.EqualFold(r.Status, "active")
}
