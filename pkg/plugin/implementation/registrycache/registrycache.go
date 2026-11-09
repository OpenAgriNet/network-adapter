// Package registrycache keeps the registry's active records in the cache, one
// list per entity. It loads every active record of the configured entities up
// front, reloads them each TTL, and falls back to the registry for anything
// not in the cache.
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
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// maxResponseBytes caps a registry answer; a larger one is refused.
const maxResponseBytes = util.DefaultMaxResponseBytes

// ErrNotFound reports that no active record, or more than one, matched.
var ErrNotFound = definition.ErrRegistryRecordNotFound

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
	cache   definition.Cache
	http    *http.Client
}

// New returns a Store. cache may be nil, which disables caching.
func New(cache definition.Cache, cfg Config) (*Store, error) {
	if cfg.URL == "" {
		return nil, errors.New("registrycache: url is required")
	}
	return &Store{
		url:     strings.TrimSuffix(cfg.URL, "/"),
		ttl:     cfg.TTL,
		check:   time.Minute,
		preload: cfg.Preload,
		cache:   cache,
		http:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Start loads each preload entity now and again every TTL, in the background,
// until the returned stop is called. Every minute it also reloads an entity
// whose load failed, or whose list is gone from the cache (expired while the
// host slept, or the cache was restarted). A failed load keeps the list
// already cached; reads fall back to the registry meanwhile.
func (s *Store) Start(ctx context.Context) (stop func()) {
	if !s.caching() || len(s.preload) == 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	for _, entity := range s.preload {
		go func() {
			var due time.Time // zero: load now
			for {
				if !time.Now().Before(due) || s.missing(ctx, entity) {
					// Timed from the start of the load, so the next one lands
					// before the list it cached expires.
					start := time.Now()
					if s.load(ctx, entity) {
						due = start.Add(s.ttl)
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

// missing reports whether entity's list is absent from the cache. A cache it
// could not read is not reported missing: reloading would not refill it, and
// would only send every check to the registry.
func (s *Store) missing(ctx context.Context, entity string) bool {
	raw, err := s.cache.Get(ctx, listKey(entity))
	return err == nil && raw == ""
}

// load reads every record of entity and caches the active ones as its list,
// reporting whether the registry answered.
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

// Get returns the one active record of entity whose field equals value (or, for
// a list field, contains it), exactly as the registry sent it. It is found in
// the entity's cached list; a record the list lacks is asked of the registry
// and, when active, added to the list. No match, an inactive record, or more
// than one match is ErrNotFound.
func (s *Store) Get(ctx context.Context, entity, field, value string) (json.RawMessage, error) {
	match := map[string]string{field: value}
	all, cached, err := s.activeRecords(ctx, entity)
	if err != nil {
		return nil, err
	}
	found := matching(all, match)
	if len(found) == 0 && cached {
		// Not in the cached list: it may have joined since the last load.
		records, err := s.search(ctx, entity, map[string]any{field: map[string]string{"eq": value}})
		if err != nil {
			return nil, err
		}
		if found = matching(activeOf(records), match); len(found) == 1 {
			s.storeList(ctx, entity, append(all, found[0]))
		}
	}
	if len(found) != 1 {
		return nil, ErrNotFound
	}
	return found[0], nil
}

// List returns every active record of entity whose fields match, each exactly
// as the registry sent it. A field matches when it equals the value or, for a
// list field, contains it; an empty match returns every active record. The
// entity's list is served from the cache when present, and otherwise read
// from the registry and cached.
func (s *Store) List(ctx context.Context, entity string, match map[string]string) ([]json.RawMessage, error) {
	all, _, err := s.activeRecords(ctx, entity)
	if err != nil {
		return nil, err
	}
	return matching(all, match), nil
}

// activeRecords returns entity's active records and whether they came from the
// cache; on a miss they are read from the registry and cached.
func (s *Store) activeRecords(ctx context.Context, entity string) ([]json.RawMessage, bool, error) {
	if s.caching() {
		if raw, err := s.cache.Get(ctx, listKey(entity)); err == nil && raw != "" {
			var all []json.RawMessage
			if json.Unmarshal([]byte(raw), &all) == nil {
				return all, true, nil
			}
		}
	}
	records, err := s.search(ctx, entity, map[string]any{})
	if err != nil {
		return nil, false, err
	}
	all := activeOf(records)
	s.storeList(ctx, entity, all)
	return all, false, nil
}

func (s *Store) storeList(ctx context.Context, entity string, all []json.RawMessage) {
	if !s.caching() {
		return
	}
	raw, err := json.Marshal(all)
	if err == nil {
		err = s.cache.Set(ctx, listKey(entity), string(raw), s.ttl)
	}
	if err != nil {
		log.Warnf(ctx, "Failed to cache registry list %s: %v", listKey(entity), err)
	}
}

func listKey(entity string) string {
	return fmt.Sprintf("registry_%s_all", entity)
}

func (s *Store) caching() bool {
	return s.cache != nil && s.ttl > 0
}

// search asks the registry for the records of entity matching filters. An
// empty filters object matches every record.
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
