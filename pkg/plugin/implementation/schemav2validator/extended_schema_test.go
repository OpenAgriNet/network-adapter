package schemav2validator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
)

func TestFindReferencedObjects(t *testing.T) {
	tests := []struct {
		name string
		data interface{}
		path string
		want int // number of objects found
	}{
		{
			name: "single domain object",
			data: map[string]interface{}{
				"@context": "https://example.com/schema/DomainType/v1/context.jsonld",
				"@type":    "DomainType",
				"field":    "value",
			},
			path: "message",
			want: 1,
		},
		{
			name: "object with @context and @type is always found",
			data: map[string]interface{}{
				"@context": "https://example.com/schema/core/v2/context.jsonld",
				"@type":    "beckn:Order",
				"field":    "value",
			},
			path: "message",
			want: 1,
		},
		{
			name: "nested domain objects",
			data: map[string]interface{}{
				"order": map[string]interface{}{
					"@context": "https://example.com/schema/core/v2/context.jsonld",
					"@type":    "beckn:Order",
					"orderAttributes": map[string]interface{}{
						"@context": "https://example.com/schema/ChargingSession/v1/context.jsonld",
						"@type":    "ChargingSession",
						"field":    "value",
					},
				},
			},
			path: "message",
			want: 2, // both the outer and nested object are found
		},
		{
			name: "array with domain objects",
			data: map[string]interface{}{
				"items": []interface{}{
					map[string]interface{}{
						"@context": "https://example.com/schema/DomainType/v1/context.jsonld",
						"@type":    "DomainType",
					},
					map[string]interface{}{
						"@context": "https://example.com/schema/AnotherType/v1/context.jsonld",
						"@type":    "AnotherType",
					},
				},
			},
			path: "message",
			want: 2,
		},
		{
			name: "object without @context",
			data: map[string]interface{}{
				"field": "value",
			},
			path: "message",
			want: 0,
		},
		{
			name: "object with @context but no @type",
			data: map[string]interface{}{
				"@context": "https://example.com/schema/DomainType/v1/context.jsonld",
				"field":    "value",
			},
			path: "message",
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findReferencedObjects(tt.data, tt.path)
			assert.Equal(t, tt.want, len(got))
		})
	}
}

func TestTransformContextToSchemaURL(t *testing.T) {
	tests := []struct {
		name       string
		contextURL string
		want       string
	}{
		{
			name:       "standard transformation",
			contextURL: "https://example.com/schema/EvChargingOffer/v1/context.jsonld",
			want:       "https://example.com/schema/EvChargingOffer/v1/attributes.yaml",
		},
		{
			name:       "already attributes.yaml",
			contextURL: "https://example.com/schema/EvChargingOffer/v1/attributes.yaml",
			want:       "https://example.com/schema/EvChargingOffer/v1/attributes.yaml",
		},
		{
			name:       "no context.jsonld in URL",
			contextURL: "https://example.com/schema/EvChargingOffer/v1/",
			want:       "https://example.com/schema/EvChargingOffer/v1/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := transformContextToSchemaURL(tt.contextURL)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHashURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{
			name: "consistent hashing",
			url:  "https://example.com/schema.yaml",
		},
		{
			name: "empty string",
			url:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash1 := hashURL(tt.url)
			hash2 := hashURL(tt.url)

			// Same URL should produce same hash
			assert.Equal(t, hash1, hash2)

			// Hash should be 64 characters (SHA256 hex)
			assert.Equal(t, 64, len(hash1))
		})
	}
}

func TestIsValidSchemaPath(t *testing.T) {
	tests := []struct {
		name       string
		schemaPath string
		want       bool
	}{
		{
			name:       "http URL",
			schemaPath: "http://example.com/schema.yaml",
			want:       true,
		},
		{
			name:       "https URL",
			schemaPath: "https://example.com/schema.yaml",
			want:       true,
		},
		{
			name:       "file URL",
			schemaPath: "file:///path/to/schema.yaml",
			want:       true,
		},
		{
			name:       "local path",
			schemaPath: "/path/to/schema.yaml",
			want:       true,
		},
		{
			name:       "relative path",
			schemaPath: "./schema.yaml",
			want:       true,
		},
		{
			name:       "empty path",
			schemaPath: "",
			want:       true, // url.Parse("") succeeds, returns empty scheme
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidSchemaPath(tt.schemaPath)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewSchemaCache(t *testing.T) {
	tests := []struct {
		name    string
		maxSize int
	}{
		{
			name:    "default size",
			maxSize: 100,
		},
		{
			name:    "custom size",
			maxSize: 50,
		},
		{
			name:    "zero size",
			maxSize: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newSchemaCache(tt.maxSize)

			assert.NotNil(t, cache)
			assert.Equal(t, tt.maxSize, cache.maxSize)
			assert.NotNil(t, cache.schemas)
			assert.Equal(t, 0, len(cache.schemas))
			assert.NotNil(t, cache.rawSchemas)
			assert.Equal(t, 0, len(cache.rawSchemas))
		})
	}
}

func TestSchemaCache_GetSet(t *testing.T) {
	cache := newSchemaCache(10)

	// Create a simple schema doc
	doc := &openapi3.T{
		OpenAPI: "3.1.0",
	}

	urlHash := hashURL("https://example.com/schema.yaml")
	ttl := 1 * time.Hour

	// Test Set
	cache.set(urlHash, doc, ttl)

	// Test Get - should find it
	retrieved, found := cache.get(urlHash)
	assert.True(t, found)
	assert.Equal(t, doc, retrieved)

	// Test Get - non-existent key
	_, found = cache.get("non-existent-hash")
	assert.False(t, found)
}

func TestSchemaCache_LRUEviction(t *testing.T) {
	cache := newSchemaCache(2) // Small cache for testing

	doc1 := &openapi3.T{OpenAPI: "3.1.0"}
	doc2 := &openapi3.T{OpenAPI: "3.1.1"}
	doc3 := &openapi3.T{OpenAPI: "3.1.2"}

	ttl := 1 * time.Hour

	// Add first two items
	cache.set("hash1", doc1, ttl)
	cache.set("hash2", doc2, ttl)

	// Access first item to make it more recent
	cache.get("hash1")

	// Add third item - should evict hash2 (least recently used)
	cache.set("hash3", doc3, ttl)

	// Verify hash1 and hash3 exist, hash2 was evicted
	_, found1 := cache.get("hash1")
	_, found2 := cache.get("hash2")
	_, found3 := cache.get("hash3")

	assert.True(t, found1, "hash1 should exist (recently accessed)")
	assert.False(t, found2, "hash2 should be evicted (LRU)")
	assert.True(t, found3, "hash3 should exist (just added)")
}

func TestSchemaCache_TTLExpiry(t *testing.T) {
	cache := newSchemaCache(10)

	doc := &openapi3.T{OpenAPI: "3.1.0"}
	urlHash := "test-hash"

	// Set with very short TTL
	cache.set(urlHash, doc, 1*time.Millisecond)

	// Should be found immediately
	_, found := cache.get(urlHash)
	assert.True(t, found)

	// Wait for expiry
	time.Sleep(10 * time.Millisecond)

	// Should not be found after expiry
	_, found = cache.get(urlHash)
	assert.False(t, found)
}

func TestSchemaCache_CleanupExpired(t *testing.T) {
	cache := newSchemaCache(10)

	doc := &openapi3.T{OpenAPI: "3.1.0"}

	// Add items with short TTL
	cache.set("hash1", doc, 1*time.Millisecond)
	cache.set("hash2", doc, 1*time.Millisecond)
	cache.set("hash3", doc, 1*time.Hour) // This one won't expire

	// Wait for expiry
	time.Sleep(10 * time.Millisecond)

	// Cleanup expired
	count := cache.cleanupExpired()

	// Should have cleaned up 2 expired items
	assert.Equal(t, 2, count)

	// Verify only hash3 remains
	cache.mu.RLock()
	assert.Equal(t, 1, len(cache.schemas))
	_, exists := cache.schemas["hash3"]
	assert.True(t, exists)
	cache.mu.RUnlock()
}

func TestIsAllowedDomain(t *testing.T) {
	tests := []struct {
		name           string
		schemaURL      string
		allowedDomains []string
		want           bool
	}{
		{
			name:           "domain in whitelist",
			schemaURL:      "https://raw.githubusercontent.com/beckn/schema.yaml",
			allowedDomains: []string{"raw.githubusercontent.com", "schemas.beckn.org"},
			want:           true,
		},
		{
			name:           "domain not in whitelist",
			schemaURL:      "https://malicious.com/schema.yaml",
			allowedDomains: []string{"raw.githubusercontent.com", "schemas.beckn.org"},
			want:           false,
		},
		{
			name:           "exact domain match",
			schemaURL:      "https://raw.githubusercontent.com/beckn/schema.yaml",
			allowedDomains: []string{"raw.githubusercontent.com"},
			want:           true,
		},
		{
			name:           "legitimate subdomain passes",
			schemaURL:      "https://raw.githubusercontent.com/beckn/schema.yaml",
			allowedDomains: []string{"githubusercontent.com"},
			want:           true,
		},
		{
			name:           "prefix bypass blocked - evil-beckn.io vs beckn.io",
			schemaURL:      "https://evil-beckn.io/schema.yaml",
			allowedDomains: []string{"beckn.io"},
			want:           false,
		},
		{
			name:           "suffix bypass blocked - beckn.io.evil.com vs beckn.io",
			schemaURL:      "https://beckn.io.evil.com/schema.yaml",
			allowedDomains: []string{"beckn.io"},
			want:           false,
		},
		{
			name:           "substring bypass blocked - notraw.githubusercontent.com vs raw.githubusercontent.com",
			schemaURL:      "https://notraw.githubusercontent.com/schema.yaml",
			allowedDomains: []string{"raw.githubusercontent.com"},
			want:           false,
		},
		{
			name:           "allowlist entry with leading/trailing spaces",
			schemaURL:      "https://raw.githubusercontent.com/beckn/schema.yaml",
			allowedDomains: []string{"  raw.githubusercontent.com  "},
			want:           true,
		},
		{
			name:           "fragment injection bypass attempt",
			schemaURL:      "file:///dev/zero#raw.githubusercontent.com",
			allowedDomains: []string{"raw.githubusercontent.com"},
			want:           false,
		},
		{
			name:           "no host (file://) does not match even with non-empty allowlist",
			schemaURL:      "file:///etc/passwd",
			allowedDomains: []string{"raw.githubusercontent.com"},
			want:           false,
		},
		{
			name:           "empty allowlist allows any domain",
			schemaURL:      "https://any.domain.example.com/schema.yaml",
			allowedDomains: []string{},
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.schemaURL)
			assert.NoError(t, err)
			got := isAllowedDomain(u, tt.allowedDomains)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFindReferencedObjects_PathBuilding(t *testing.T) {
	data := map[string]interface{}{
		"order": map[string]interface{}{
			"beckn:orderItems": []interface{}{
				map[string]interface{}{
					"beckn:acceptedOffer": map[string]interface{}{
						"beckn:offerAttributes": map[string]interface{}{
							"@context": "https://example.com/schema/ChargingOffer/v1/context.jsonld",
							"@type":    "ChargingOffer",
						},
					},
				},
			},
		},
	}

	objects := findReferencedObjects(data, "message")

	assert.Equal(t, 1, len(objects))
	assert.Equal(t, "message.order.beckn:orderItems[0].beckn:acceptedOffer.beckn:offerAttributes", objects[0].Path)
	assert.Equal(t, "ChargingOffer", objects[0].Type)
}

// Integration tests for the 4 remaining functions

func TestLoadSchemaFromPath_LocalFile(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	tmpFile, err := os.CreateTemp("", "test-schema-*.yaml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      properties:
        field1:
          type: string`

	_, err = tmpFile.Write([]byte(schemaContent))
	assert.NoError(t, err)
	tmpFile.Close()

	// localSchema=false means the location came from a payload's @context --
	// the only way the production caller passes it. A local file is not
	// something the network may ask this process to open, so it is refused.
	doc, err := cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, false)
	if err == nil {
		t.Fatal("a payload-directed load opened a local file")
	}
	assert.Contains(t, err.Error(), "only http and https are read")
	assert.Nil(t, doc)

	// localSchema=true is an operator naming a path in the adapter's own
	// config, which is the one case where opening a file is the intent.
	doc, err = cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Equal(t, "3.1.0", doc.OpenAPI)
}

func TestLoadSchemaFromPath_CacheHit(t *testing.T) {
	// A temp file is just the fixture here, so this loads in operator mode:
	// a payload-directed load refuses local files by design.
	cache := newSchemaCache(10)
	ctx := context.Background()

	tmpFile, err := os.CreateTemp("", "test-schema-*.yaml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0`

	tmpFile.Write([]byte(schemaContent))
	tmpFile.Close()

	doc1, err := cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)

	doc2, err := cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)

	assert.Equal(t, doc1, doc2)
}

func TestLoadSchemaFromPath_InvalidPath(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	_, err := cache.loadSchemaFromPath(ctx, "/nonexistent/schema.yaml", 1*time.Hour, 30*time.Second, false)
	assert.Error(t, err)
}

func TestFindSchemaByType_DirectMatch(t *testing.T) {
	// A temp file is just the fixture here, so this loads in operator mode:
	// a payload-directed load refuses local files by design.
	cache := newSchemaCache(10)
	ctx := context.Background()

	tmpFile, err := os.CreateTemp("", "test-schema-*.yaml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      properties:
        field1:
          type: string`

	tmpFile.Write([]byte(schemaContent))
	tmpFile.Close()

	doc, err := cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)

	schema, err := findSchemaByType(ctx, doc, "TestType")
	assert.NoError(t, err)
	assert.NotNil(t, schema)
}

func TestFindSchemaByType_NotFound(t *testing.T) {
	// A temp file is just the fixture here, so this loads in operator mode:
	// a payload-directed load refuses local files by design.
	cache := newSchemaCache(10)
	ctx := context.Background()

	tmpFile, err := os.CreateTemp("", "test-schema-*.yaml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object`

	tmpFile.Write([]byte(schemaContent))
	tmpFile.Close()

	doc, err := cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)

	_, err = findSchemaByType(ctx, doc, "NonExistentType")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no schema found")
}

func TestValidateReferencedObject_Valid(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      additionalProperties: false
      x-jsonld:
        "@context": ./context.jsonld
        "@type": TestType
      properties:
        field1:
          type: string
      required:
        - field1`

	ctxURL := serveTempSchema(t, schemaContent)

	obj := referencedObject{
		Path:    "message.test",
		Context: ctxURL,
		Type:    "TestType",
		Data: map[string]interface{}{
			"@context": ctxURL,
			"@type":    "TestType",
			"field1":   "value1",
		},
	}

	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, nil, false)
	assert.NoError(t, err)
}

func TestValidateReferencedObject_Invalid(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      additionalProperties: false
      x-jsonld:
        "@context": ./context.jsonld
        "@type": TestType
      properties:
        field1:
          type: string
      required:
        - field1`

	ctxURL := serveTempSchema(t, schemaContent)

	obj := referencedObject{
		Path:    "message.test",
		Context: ctxURL,
		Type:    "TestType",
		Data: map[string]interface{}{
			"@context": ctxURL,
			"@type":    "TestType",
		},
	}

	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, nil, false)
	assert.Error(t, err)

	schemaErrors := []model.Error{}
	(&schemav2Validator{}).extractSchemaErrors(err, &schemaErrors)
	if len(schemaErrors) == 0 || schemaErrors[0].Code != "SCH_REQUIRED_FIELD_MISSING" {
		t.Errorf("Code = %+v, want SCH_REQUIRED_FIELD_MISSING", schemaErrors)
	}
}

func TestValidateReferencedObject_DomainNotAllowed(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	obj := referencedObject{
		Path:    "message.test",
		Context: "https://malicious.com/schema.yaml",
		Type:    "TestType",
		Data:    map[string]interface{}{},
	}

	allowedDomains := []string{"trusted.com"}

	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, allowedDomains, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "domain not allowed")

	becknErr, ok := err.(*model.Error)
	if !ok || becknErr.Code != "SCH_INVALID_JSONLD_CONTEXT" {
		t.Errorf("err = %+v (%T), want *model.Error with Code=SCH_INVALID_JSONLD_CONTEXT", err, err)
	}
}

func TestValidateReferencedObject_EntityTypeNotFound(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object`

	ctxURL := serveTempSchema(t, schemaContent)

	obj := referencedObject{
		Path:    "message.test",
		Context: ctxURL,
		Type:    "NonExistentType",
		Data: map[string]interface{}{
			"@context": ctxURL,
			"@type":    "NonExistentType",
		},
	}

	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, nil, false)
	assert.Error(t, err)

	becknErr, ok := err.(*model.Error)
	if !ok || becknErr.Code != "SCH_INVALID_ENTITY_TYPE" {
		t.Errorf("err = %+v (%T), want *model.Error with Code=SCH_INVALID_ENTITY_TYPE", err, err)
	}
	if becknErr.Details == nil || becknErr.Details.Path != obj.Path {
		t.Errorf("Details.Path = %+v, want %q", becknErr.Details, obj.Path)
	}
	if becknErr.Unwrap() == nil {
		t.Error("expected Unwrap() to reach the underlying findSchemaByType error, got nil")
	}
}

func TestValidateReferencedObject_SchemaLoadFailure(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	obj := referencedObject{
		Path:    "message.test",
		Context: "/nonexistent/schema.yaml",
		Type:    "TestType",
		Data:    map[string]interface{}{},
	}

	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, nil, false)
	assert.Error(t, err)

	becknErr, ok := err.(*model.Error)
	if !ok || becknErr.Code != "SCH_SCHEMA_ADAPTATION_FAILED" {
		t.Errorf("err = %+v (%T), want *model.Error with Code=SCH_SCHEMA_ADAPTATION_FAILED", err, err)
	}
	if becknErr.Details == nil || becknErr.Details.Path != obj.Path {
		t.Errorf("Details.Path = %+v, want %q", becknErr.Details, obj.Path)
	}
	if becknErr.Unwrap() == nil {
		t.Error("expected Unwrap() to reach the underlying loadSchemaFromPath error, got nil")
	}
}

func TestValidateReferencedObject_NonHttpSchemeRejectedWhenAllowlistSet(t *testing.T) {
	tests := []struct {
		name    string
		context string
	}{
		{
			name:    "file scheme with fragment bypass",
			context: "file:///dev/zero#raw.githubusercontent.com",
		},
		{
			name:    "file scheme local path",
			context: "file:///etc/passwd",
		},
		{
			// host IS in allowlist — scheme check must fire before domain check
			name:    "gopher scheme with allowlisted host",
			context: "gopher://raw.githubusercontent.com/schema.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newSchemaCache(10)
			ctx := context.Background()

			obj := referencedObject{
				Path:    "message.test",
				Context: tt.context,
				Type:    "Dos",
				Data:    map[string]interface{}{},
			}

			allowedDomains := []string{"raw.githubusercontent.com"}

			err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, allowedDomains, false)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "invalid scheme in @context")

			becknErr, ok := err.(*model.Error)
			if !ok || becknErr.Code != "SCH_INVALID_JSONLD_CONTEXT" {
				t.Errorf("err = %+v (%T), want *model.Error with Code=SCH_INVALID_JSONLD_CONTEXT", err, err)
			}
		})
	}
}

func TestValidateReferencedObject_EmptyAllowlistSkipsDomainCheck(t *testing.T) {
	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      additionalProperties: false
      x-jsonld:
        "@context": ./context.jsonld
        "@type": TestType
      properties:
        field1:
          type: string
      required:
        - field1`

	tests := []struct {
		name           string
		allowedDomains []string
	}{
		{name: "file scheme allowed when no allowlist (nil)", allowedDomains: nil},
		{name: "file scheme allowed when no allowlist (empty)", allowedDomains: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile, err := os.CreateTemp("", "test-schema-*.yaml")
			assert.NoError(t, err)
			defer os.Remove(tmpFile.Name())
			tmpFile.Write([]byte(schemaContent))
			tmpFile.Close()

			cache := newSchemaCache(10)
			ctx := context.Background()

			// Use file:// scheme — would be rejected by scheme check if allowlist were set.
			// It is still refused, one layer down: the reader takes http and
			// https only. What an empty allowlist skips is the HOST check.
			obj := referencedObject{
				Path:    "message.test",
				Context: "file://" + tmpFile.Name(),
				Type:    "TestType",
				Data:    map[string]interface{}{"field1": "value1"},
			}

			err = cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, tt.allowedDomains, false)
			// So: no domain error and no @context scheme error, which is what
			// an empty allowlist means. Not "anything is readable".
			if err != nil {
				assert.NotContains(t, err.Error(), "domain not allowed")
				assert.NotContains(t, err.Error(), "invalid scheme in @context")
			}
		})
	}
}

func TestValidateExtendedSchemas_NoObjects(t *testing.T) {
	v := &schemav2Validator{
		config: &Config{
			EnableExtendedSchema: true,
			ExtendedSchemaConfig: ExtendedSchemaConfig{},
		},
		schemaCache: newSchemaCache(10),
	}

	ctx := context.Background()
	body := map[string]interface{}{
		"message": map[string]interface{}{
			"field": "value",
		},
	}

	err := v.validateExtendedSchemas(ctx, body)
	assert.NoError(t, err)
}

func TestValidateExtendedSchemas_MissingMessage(t *testing.T) {
	v := &schemav2Validator{
		config: &Config{
			EnableExtendedSchema: true,
		},
		schemaCache: newSchemaCache(10),
	}

	ctx := context.Background()
	body := map[string]interface{}{
		"context": map[string]interface{}{},
	}

	err := v.validateExtendedSchemas(ctx, body)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing 'message' field")
}

// TestValidateExtendedSchemas_DomainNotAllowed_PropagatesCode drives a real
// domain-object validation failure through the full production path
// (validateExtendedSchemas -> validateReferencedObject -> extractSchemaErrors'
// *model.Error passthrough branch -> prefixSchemaErrorPaths), confirming the
// classified code and path both survive end-to-end — not just at the unit
// level of validateReferencedObject or extractSchemaErrors individually.
func TestValidateExtendedSchemas_DomainNotAllowed_PropagatesCode(t *testing.T) {
	v := &schemav2Validator{
		config: &Config{
			EnableExtendedSchema: true,
			ExtendedSchemaConfig: ExtendedSchemaConfig{
				AllowedDomains: []string{"trusted.com"},
			},
		},
		schemaCache: newSchemaCache(10),
	}

	ctx := context.Background()
	body := map[string]interface{}{
		"message": map[string]interface{}{
			"order": map[string]interface{}{
				"@context": "https://malicious.com/schema.yaml",
				"@type":    "SomeType",
			},
		},
	}

	err := v.validateExtendedSchemas(ctx, body)
	if err == nil {
		t.Fatal("expected an error")
	}

	var schemaErr *model.SchemaValidationErr
	if !errors.As(err, &schemaErr) {
		t.Fatalf("expected *model.SchemaValidationErr, got %T: %v", err, err)
	}
	if len(schemaErr.Errors) != 1 || schemaErr.Errors[0].Code != "SCH_INVALID_JSONLD_CONTEXT" {
		t.Errorf("Errors = %+v, want one entry with Code=SCH_INVALID_JSONLD_CONTEXT", schemaErr.Errors)
	}
	if schemaErr.Errors[0].Details == nil || schemaErr.Errors[0].Details.Path == "" {
		t.Errorf("Details = %+v, want a non-empty Path from prefixSchemaErrorPaths", schemaErr.Errors[0].Details)
	}
}

func TestPrefixSchemaErrorPaths(t *testing.T) {
	tests := []struct {
		name         string
		schemaErrors []model.Error
		objPath      string
		want         []model.Error
	}{
		{
			name:         "no existing details gets object path",
			schemaErrors: []model.Error{{Message: "m1"}},
			objPath:      "message.order",
			want: []model.Error{
				{Message: "m1", Details: &model.ErrorDetails{Path: "message.order"}},
			},
		},
		{
			name: "existing path is prefixed with object path",
			schemaErrors: []model.Error{
				{Message: "m1", Details: &model.ErrorDetails{Path: "items[0].id"}},
			},
			objPath: "message.order",
			want: []model.Error{
				{Message: "m1", Details: &model.ErrorDetails{Path: "message.order.items[0].id"}},
			},
		},
		{
			name: "existing cause is preserved after prefixing",
			schemaErrors: []model.Error{
				{
					Message: "m1",
					Details: &model.ErrorDetails{
						Path:  "items[0].id",
						Cause: &model.Error{Code: "NET_DOWNSTREAM_UNAVAILABLE", Message: "registry lookup failed"},
					},
				},
			},
			objPath: "message.order",
			want: []model.Error{
				{
					Message: "m1",
					Details: &model.ErrorDetails{
						Path:  "message.order.items[0].id",
						Cause: &model.Error{Code: "NET_DOWNSTREAM_UNAVAILABLE", Message: "registry lookup failed"},
					},
				},
			},
		},
		{
			name: "existing details with no path yet gets object path, cause still preserved",
			schemaErrors: []model.Error{
				{
					Message: "m1",
					Details: &model.ErrorDetails{
						Cause: &model.Error{Code: "NET_DOWNSTREAM_UNAVAILABLE", Message: "registry lookup failed"},
					},
				},
			},
			objPath: "message.order",
			want: []model.Error{
				{
					Message: "m1",
					Details: &model.ErrorDetails{
						Path:  "message.order",
						Cause: &model.Error{Code: "NET_DOWNSTREAM_UNAVAILABLE", Message: "registry lookup failed"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefixSchemaErrorPaths(tt.schemaErrors, tt.objPath)
			if !reflect.DeepEqual(tt.schemaErrors, tt.want) {
				t.Errorf("prefixSchemaErrorPaths() = %+v, want %+v", tt.schemaErrors, tt.want)
			}
		})
	}
}

func TestIsSchemaVersionSegment(t *testing.T) {
	tests := []struct {
		name string
		seg  string
		want bool
	}{
		{name: "v1", seg: "v1", want: true},
		{name: "v2.1", seg: "v2.1", want: true},
		{name: "v1.2.3", seg: "v1.2.3", want: true},
		{name: "V2 uppercase", seg: "V2", want: true},
		{name: "1.0 no prefix", seg: "1.0", want: true},
		{name: "bare v", seg: "v", want: false},
		{name: "type name", seg: "JobType", want: false},
		{name: "empty", seg: "", want: false},
		{name: "v1beta", seg: "v1beta", want: false},
		{name: "plain word", seg: "main", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isSchemaVersionSegment(tt.seg))
		})
	}
}

func TestExtractRelativeSchemaPath(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{
			name:   "URL with /schema/ marker and version",
			rawURL: "https://example.com/schema/JobType/v2.1/attributes.yaml",
			want:   "JobType/attributes.yaml",
		},
		{
			name:   "namespaced URL extracts last non-version segment",
			rawURL: "https://example.com/schema/common/CodedValue/v2.1/attributes.yaml",
			want:   "CodedValue/attributes.yaml",
		},
		{
			name:   "GitHub raw URL without /schema/ marker",
			rawURL: "https://raw.githubusercontent.com/org/repo/main/hiring-jobs/HiringJobResource/v2.1/attributes.yaml",
			want:   "HiringJobResource/attributes.yaml",
		},
		{
			name:   "context.jsonld URL",
			rawURL: "https://example.com/schema/ChargingSession/v1/context.jsonld",
			want:   "ChargingSession/attributes.yaml",
		},
		{
			name:   "no version segment",
			rawURL: "https://example.com/schema/JobType/attributes.yaml",
			want:   "JobType/attributes.yaml",
		},
		{
			name:   "bare key no scheme",
			rawURL: "JobType/attributes.yaml",
			want:   "JobType/attributes.yaml",
		},
		{
			name:   "relative path with ../ segments",
			rawURL: "../../../../common/VerificationSummary/v2.1/attributes.yaml",
			want:   "VerificationSummary/attributes.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, extractRelativeSchemaPath(u))
		})
	}
}

func TestRawSchemaKey(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		want string
	}{
		{
			name: "2-part path unchanged",
			rel:  "JobType/attributes.yaml",
			want: "JobType/attributes.yaml",
		},
		{
			name: "3-part path strips version segment",
			rel:  "JobType/v2.1/attributes.yaml",
			want: "JobType/attributes.yaml",
		},
		{
			name: "3-part path with v1.0",
			rel:  "AnotherType/v1.0/attributes.yaml",
			want: "AnotherType/attributes.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, rawSchemaKey(tt.rel))
		})
	}
}

func TestRawSchemaKey_ConsistentWithExtractRelativeSchemaPath(t *testing.T) {
	cases := []struct {
		fileRel   string
		schemaURL string
	}{
		{
			fileRel:   "JobType/v2.1/attributes.yaml",
			schemaURL: "https://example.com/schema/JobType/v2.1/attributes.yaml",
		},
		{
			fileRel:   "HiringJobResource/v2.1/attributes.yaml",
			schemaURL: "https://raw.githubusercontent.com/org/repo/main/hiring-jobs/HiringJobResource/v2.1/attributes.yaml",
		},
	}
	for _, c := range cases {
		fileKey := rawSchemaKey(c.fileRel)
		u, _ := url.Parse(c.schemaURL)
		urlKey := extractRelativeSchemaPath(u)
		assert.Equal(t, fileKey, urlKey, "key mismatch for %s", c.fileRel)
	}
}

func TestPreloadSchemasToCache(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "schema-test-*")
	assert.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	schemaContent := []byte(`openapi: 3.1.0
info:
  title: Test
  version: 1.0.0`)

	os.MkdirAll(filepath.Join(tmpDir, "TypeA"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "TypeA", "attributes.yaml"), schemaContent, 0644)

	os.MkdirAll(filepath.Join(tmpDir, "TypeB"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "TypeB", "attributes.yaml"), schemaContent, 0644)

	cache := newSchemaCache(10)
	ctx := context.Background()

	err = preloadSchemasToCache(ctx, cache, tmpDir)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(cache.rawSchemas))
	assert.Contains(t, cache.rawSchemas, "TypeA/attributes.yaml")
	assert.Contains(t, cache.rawSchemas, "TypeB/attributes.yaml")
}

func TestPreloadSchemasToCache_InvalidDir(t *testing.T) {
	cache := newSchemaCache(10)
	err := preloadSchemasToCache(context.Background(), cache, "/nonexistent/dir")
	assert.Error(t, err)
}

func TestLoadSchemaFromPath_RawSchemasHit(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object`

	cache.rawSchemas["TestType/attributes.yaml"] = []byte(schemaContent)

	doc, err := cache.loadSchemaFromPath(ctx, "TestType/attributes.yaml", 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)
	assert.NotNil(t, doc)
	assert.Equal(t, "3.1.0", doc.OpenAPI)
}

func TestLoadSchemaFromPath_LRUHit(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	expected := &openapi3.T{OpenAPI: "3.1.0"}
	cache.set(hashURL("TestType/attributes.yaml"), expected, 1*time.Hour)

	// localSchema=false skips rawSchemas step, goes straight to LRU
	doc, err := cache.loadSchemaFromPath(ctx, "TestType/attributes.yaml", 1*time.Hour, 30*time.Second, false)
	assert.NoError(t, err)
	assert.Equal(t, expected, doc)
}

func TestLoadSchemaFromPath_LocalMissFallsBackToFile(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	tmpFile, err := os.CreateTemp("", "test-schema-*.yaml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	tmpFile.Write([]byte(`openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0`))
	tmpFile.Close()

	// rawSchemas empty, localSchema=true — local miss, falls through to file load
	doc, err := cache.loadSchemaFromPath(ctx, tmpFile.Name(), 1*time.Hour, 30*time.Second, true)
	assert.NoError(t, err)
	assert.NotNil(t, doc)
}

func TestValidateReferencedObject_LocalSchemaHit(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	cache.rawSchemas["TestType/attributes.yaml"] = []byte(`openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      properties:
        field1:
          type: string`)

	obj := referencedObject{
		Path: "message.test",
		Type: "TestType",
		Data: map[string]interface{}{
			"@type":  "TestType",
			"field1": "value1",
		},
	}

	// localSchema=true, no @context — relies entirely on rawSchemas lookup by @type
	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, nil, true)
	assert.NoError(t, err)
}

func TestValidateReferencedObject_LocalMissFallsBackToContext(t *testing.T) {
	cache := newSchemaCache(10)
	ctx := context.Background()

	ctxURL := serveTempSchema(t, `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      properties:
        field1:
          type: string`)

	obj := referencedObject{
		Path:    "message.test",
		Context: ctxURL,
		Type:    "TestType",
		Data: map[string]interface{}{
			"@context": ctxURL,
			"@type":    "TestType",
			"field1":   "value1",
		},
	}

	// rawSchemas empty, localSchema=true — local miss, falls back to fetching the @context
	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, nil, true)
	assert.NoError(t, err)
}

func TestValidateReferencedObject_AllowlistedHttpsURLPasses(t *testing.T) {
	schemaContent := `openapi: 3.1.0
info:
  title: Test Schema
  version: 1.0.0
components:
  schemas:
    TestType:
      type: object
      additionalProperties: false
      x-jsonld:
        "@context": ./context.jsonld
        "@type": TestType
      properties:
        field1:
          type: string
      required:
        - field1`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(schemaContent))
	}))
	defer server.Close()

	// Extract just the host (e.g. "127.0.0.1:PORT") and add it to the allowlist.
	serverURL, _ := url.Parse(server.URL)
	allowedDomains := []string{serverURL.Host}

	cache := newSchemaCache(10)
	ctx := context.Background()

	obj := referencedObject{
		Path:    "message.test",
		Context: server.URL + "/schema/context.jsonld",
		Type:    "TestType",
		Data:    map[string]interface{}{"field1": "value1"},
	}

	err := cache.validateReferencedObject(ctx, obj, 1*time.Hour, 30*time.Second, allowedDomains, false)
	assert.NoError(t, err, "valid http URL with allowlisted host should pass")
}

func TestLoadSchemaFromPath_TTLExpiry_FetchesFresh(t *testing.T) {
	var serveV2 atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveV2.Load() {
			w.Write([]byte("openapi: 3.1.0\ninfo:\n  title: Schema v2\n  version: 2.0.0\n"))
		} else {
			w.Write([]byte("openapi: 3.1.0\ninfo:\n  title: Schema v1\n  version: 1.0.0\n"))
		}
	}))
	defer server.Close()

	cache := newSchemaCache(10)
	ctx := context.Background()

	// Load v1 with a 1ms TTL so the LRU entry expires almost immediately.
	doc1, err := cache.loadSchemaFromPath(ctx, server.URL, 1*time.Millisecond, 30*time.Second, false)
	assert.NoError(t, err)
	assert.Equal(t, "Schema v1", doc1.Info.Title)

	// Wait for the LRU entry to expire, then switch the server to v2.
	time.Sleep(10 * time.Millisecond)
	serveV2.Store(true)

	// Re-load — LRU miss (expired), freshReadFromURI fetches from the server and gets v2.
	doc2, err := cache.loadSchemaFromPath(ctx, server.URL, 1*time.Hour, 30*time.Second, false)
	assert.NoError(t, err)
	assert.Equal(t, "Schema v2", doc2.Info.Title, "expected v2 after TTL expiry — global URIMapCache not bypassed")
}

// packStyleSchema mirrors how the capability schema packs are shaped: the capability
// declares @type one level down in allOf and lists it as required, and nothing
// closes the object with additionalProperties:false.
const packStyleSchema = `openapi: 3.1.0
info:
  title: Pack Style
  version: 1.0.0
components:
  schemas:
    WeatherObservation:
      type: object
      x-jsonld:
        "@context": https://schemas.example.org/schema/WeatherObservation/v0.1/context.jsonld
        "@type": openagrinet:WeatherObservation
      allOf:
        - type: object
          required:
            - informationMode
          properties:
            informationMode:
              type: string
              enum: [OnDemand, Direct]
        - type: object
          required:
            - "@type"
          properties:
            "@type":
              type: string
              const: openagrinet:WeatherObservation`

// serveTempSchema serves content over http and returns a URL usable as an
// @context. Served rather than written to disk because a payload-directed load
// reads http and https only -- and because fetching is what production does.
func serveTempSchema(t *testing.T, content string) string {
	t.Helper()
	return serveSchema(t, content).URL + "/context.jsonld"
}

// A pack that requires @type must receive it. This is the case that could not
// validate while both JSON-LD keys were removed unconditionally: the payload
// carries @type, the schema requires it, and stripping it produced a spurious
// "@type is required".
func TestValidateReferencedObject_PackStyleKeepsAtType(t *testing.T) {
	cache := newSchemaCache(10)
	path := serveTempSchema(t, packStyleSchema)

	obj := referencedObject{
		Path:    "message.catalogs[0].resources[0].resourceAttributes",
		Context: path,
		Type:    "openagrinet:WeatherObservation",
		Data: map[string]interface{}{
			"@context":        "https://schemas.example.org/schema/WeatherObservation/v0.1/context.jsonld",
			"@type":           "openagrinet:WeatherObservation",
			"informationMode": "OnDemand",
		},
	}

	err := cache.validateReferencedObject(context.Background(), obj, 1*time.Hour, 30*time.Second, nil, false)
	assert.NoError(t, err)
}

// packStyleTypeListSchema mirrors how the packs really declare @type: a oneOf
// whose first branch is the canonical string and whose second is a list
// carrying that type alongside provider-defined ones, which must not take the
// openagrinet: prefix.
const packStyleTypeListSchema = `openapi: 3.1.0
info:
  title: Pack Style With Type List
  version: 1.0.0
components:
  schemas:
    WeatherObservation:
      type: object
      x-jsonld:
        "@context": https://schemas.example.org/schema/WeatherObservation/v0.1/context.jsonld
        "@type": openagrinet:WeatherObservation
      allOf:
        - type: object
          required:
            - informationMode
          properties:
            informationMode:
              type: string
              enum: [OnDemand, Direct]
        - type: object
          required:
            - "@type"
          properties:
            "@type":
              oneOf:
                - type: string
                  const: openagrinet:WeatherObservation
                - type: array
                  minItems: 2
                  uniqueItems: true
                  contains:
                    const: openagrinet:WeatherObservation
                  items:
                    oneOf:
                      - const: openagrinet:WeatherObservation
                      - type: string
                        minLength: 1
                        not:
                          pattern: "^openagrinet:"`

// resourceBody wraps resourceAttributes the way a payload carries them, so
// discovery runs over the same shape production sees.
func resourceBody(ctxURL string, atType interface{}, informationMode string) map[string]interface{} {
	attrs := map[string]interface{}{"@context": ctxURL, "@type": atType}
	if informationMode != "" {
		attrs["informationMode"] = informationMode
	}
	return map[string]interface{}{
		"message": map[string]interface{}{
			"catalogs": []interface{}{
				map[string]interface{}{"resources": []interface{}{
					map[string]interface{}{"resourceAttributes": attrs},
				}},
			},
		},
	}
}

// theObjectIn runs the production discovery over a body and returns the single
// domain object in it. Tests go through this rather than building a
// referencedObject by hand: Context, Type and Data all come off one map there,
// so a hand-built object can assert a state the real path cannot produce.
func theObjectIn(t *testing.T, body map[string]interface{}) referencedObject {
	t.Helper()
	objects := findReferencedObjects(body["message"], "message")
	if len(objects) != 1 {
		t.Fatalf("expected exactly one domain object from discovery, got %d", len(objects))
	}
	return objects[0]
}

// The pack allows @type to be a list, and reading only the string form meant
// such an object matched nothing, was dropped before validation, and the layer
// reported a pass over a payload it had not looked at.
func TestValidateReferencedObject_AcceptsAndChecksATypeList(t *testing.T) {
	ctxURL := serveTempSchema(t, packStyleTypeListSchema)
	const canonical = "openagrinet:WeatherObservation"

	for _, tt := range []struct {
		name       string
		atType     interface{}
		wantErr    bool
		wantErrHas string
	}{
		{
			name:   "the canonical type alone, as a string",
			atType: canonical,
		},
		{
			name:   "the canonical type beside a provider type",
			atType: []interface{}{canonical, "vendor:GriddedForecast"},
		},
		{
			name:   "provider type first -- the document decides which entry names the capability",
			atType: []interface{}{"vendor:GriddedForecast", canonical},
		},
		{
			// A real payload-level rejection, and one that only bites because
			// @type is kept in the data rather than stripped: the list branch
			// forbids a second openagrinet: type.
			name:    "a second openagrinet type, which the pack forbids",
			atType:  []interface{}{canonical, "openagrinet:MandiPrice"},
			wantErr: true,
		},
		{
			// The string branch does not match a list and the list branch
			// requires two entries, so neither is satisfied.
			name:    "a single-entry list, which satisfies neither branch",
			atType:  []interface{}{canonical},
			wantErr: true,
		},
		{
			name:       "a list naming no type the document declares",
			atType:     []interface{}{"vendor:One", "vendor:Two"},
			wantErr:    true,
			wantErrHas: "no schema found",
		},
		{
			name:       "@type present but not a type name",
			atType:     42,
			wantErr:    true,
			wantErrHas: "not a type name",
		},
		{
			name:       "@type an empty list",
			atType:     []interface{}{},
			wantErr:    true,
			wantErrHas: "not a type name",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := theObjectIn(t, resourceBody(ctxURL, tt.atType, "OnDemand"))
			err := newSchemaCache(10).validateReferencedObject(
				context.Background(), obj, 1*time.Hour, 30*time.Second, nil, false)

			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			if err == nil {
				t.Fatal("expected a rejection; a skipped object is reported as valid")
			}
			if tt.wantErrHas != "" {
				assert.Contains(t, err.Error(), tt.wantErrHas)
			}
		})
	}
}

// An object claiming a type this validator cannot read must be rejected, not
// passed over. Skipping is what let unvalidated resourceAttributes through.
func TestFindReferencedObjects_TypeShapes(t *testing.T) {
	const ctxURL = "https://schemas.example.org/schema/WeatherObservation/v0.1/context.jsonld"

	for _, tt := range []struct {
		name      string
		attrs     map[string]interface{}
		wantFound bool
		wantTypes []string
		wantCode  string
	}{
		{
			name:      "string @type",
			attrs:     map[string]interface{}{"@context": ctxURL, "@type": "openagrinet:WeatherObservation"},
			wantFound: true,
			wantTypes: []string{"openagrinet:WeatherObservation"},
		},
		{
			name:      "list @type keeps every entry, in payload order",
			attrs:     map[string]interface{}{"@context": ctxURL, "@type": []interface{}{"a", "b"}},
			wantFound: true,
			wantTypes: []string{"a", "b"},
		},
		{
			name:      "list @context takes the first string, since only a URL locates a schema",
			attrs:     map[string]interface{}{"@context": []interface{}{ctxURL, map[string]interface{}{"inline": "term"}}, "@type": "T"},
			wantFound: true,
			wantTypes: []string{"T"},
		},
		{
			name:      "inline-object @context names no document to fetch",
			attrs:     map[string]interface{}{"@context": map[string]interface{}{"inline": "term"}, "@type": "T"},
			wantFound: true,
			wantCode:  "SCH_INVALID_JSONLD_CONTEXT",
		},
		{
			name:      "@type a number",
			attrs:     map[string]interface{}{"@context": ctxURL, "@type": 42},
			wantFound: true,
			wantCode:  "SCH_INVALID_ENTITY_TYPE",
		},
		{
			// No claim about which schema applies, so there is nothing to
			// validate against. Passed over, as before.
			name:      "@context with no @type at all",
			attrs:     map[string]interface{}{"@context": ctxURL, "field": "value"},
			wantFound: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			objects := findReferencedObjects(map[string]interface{}{"resourceAttributes": tt.attrs}, "message")
			if !tt.wantFound {
				assert.Empty(t, objects)
				return
			}
			if len(objects) != 1 {
				t.Fatalf("expected one object, got %d", len(objects))
			}
			obj := objects[0]

			if tt.wantCode != "" {
				if obj.Unusable == nil {
					t.Fatal("expected the object to be marked unusable, so it is rejected rather than skipped")
				}
				becknErr, ok := obj.Unusable.(*model.Error)
				if !ok {
					t.Fatalf("Unusable = %T, want *model.Error", obj.Unusable)
				}
				assert.Equal(t, tt.wantCode, becknErr.Code)

				// and it must actually reject when validated
				err := newSchemaCache(10).validateReferencedObject(
					context.Background(), obj, 1*time.Hour, 30*time.Second, nil, false)
				assert.Error(t, err)
				return
			}

			assert.Nil(t, obj.Unusable)
			assert.Equal(t, tt.wantTypes, obj.Types)
			assert.Equal(t, tt.wantTypes[0], obj.Type)
			assert.Equal(t, ctxURL, obj.Context)
		})
	}
}

func TestStripUnaccountedJSONLDKeys(t *testing.T) {
	declaresType := &openapi3.SchemaRef{Value: &openapi3.Schema{
		AllOf: openapi3.SchemaRefs{
			{Value: &openapi3.Schema{
				Required:   []string{"@type"},
				Properties: openapi3.Schemas{"@type": {Value: &openapi3.Schema{}}},
			}},
		},
	}}
	declaresNeither := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Properties: openapi3.Schemas{"field1": {Value: &openapi3.Schema{}}},
	}}
	declaresBoth := &openapi3.SchemaRef{Value: &openapi3.Schema{
		Properties: openapi3.Schemas{
			"@context": {Value: &openapi3.Schema{}},
			"@type":    {Value: &openapi3.Schema{}},
		},
	}}

	data := map[string]interface{}{
		"@context": "https://example.com/context.jsonld",
		"@type":    "openagrinet:WeatherObservation",
		"field1":   "value1",
	}

	tests := []struct {
		name   string
		schema *openapi3.SchemaRef
		want   []string
	}{
		{"pack declares @type, so only @context goes", declaresType, []string{"@type", "field1"}},
		{"schema declares neither, so both go", declaresNeither, []string{"field1"}},
		{"schema declares both, so neither goes", declaresBoth, []string{"@context", "@type", "field1"}},
		{"nil schema is treated as declaring nothing", nil, []string{"field1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripUnaccountedJSONLDKeys(tt.schema, data)
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			assert.Equal(t, tt.want, keys)
			// the input must not be mutated -- obj.Data is shared with the caller
			assert.Len(t, data, 3)
		})
	}
}

func TestSchemaDeclaresProperty(t *testing.T) {
	leaf := func(required ...string) *openapi3.SchemaRef {
		return &openapi3.SchemaRef{Value: &openapi3.Schema{Required: required}}
	}

	cyclic := &openapi3.SchemaRef{Value: &openapi3.Schema{}}
	cyclic.Value.AllOf = openapi3.SchemaRefs{cyclic}

	tests := []struct {
		name   string
		schema *openapi3.SchemaRef
		want   bool
	}{
		{"nil ref", nil, false},
		{"nil value", &openapi3.SchemaRef{}, false},
		{"declared directly as a property", &openapi3.SchemaRef{Value: &openapi3.Schema{
			Properties: openapi3.Schemas{"@type": {Value: &openapi3.Schema{}}},
		}}, true},
		{"required directly", leaf("@type"), true},
		{"required inside allOf", &openapi3.SchemaRef{Value: &openapi3.Schema{
			AllOf: openapi3.SchemaRefs{leaf("other"), leaf("@type")},
		}}, true},
		{"required inside anyOf", &openapi3.SchemaRef{Value: &openapi3.Schema{
			AnyOf: openapi3.SchemaRefs{leaf("@type")},
		}}, true},
		{"required inside oneOf", &openapi3.SchemaRef{Value: &openapi3.Schema{
			OneOf: openapi3.SchemaRefs{leaf("@type")},
		}}, true},
		{"required inside then", &openapi3.SchemaRef{Value: &openapi3.Schema{
			Then: leaf("@type"),
		}}, true},
		{"required inside else", &openapi3.SchemaRef{Value: &openapi3.Schema{
			Else: leaf("@type"),
		}}, true},
		// naming a property under "not" forbids it, so it must not count as declared
		{"named under not does not count", &openapi3.SchemaRef{Value: &openapi3.Schema{
			Not: leaf("@type"),
		}}, false},
		// "if" only selects a branch; it does not permit the property
		{"named under if does not count", &openapi3.SchemaRef{Value: &openapi3.Schema{
			If: leaf("@type"),
		}}, false},
		{"absent everywhere", &openapi3.SchemaRef{Value: &openapi3.Schema{
			AllOf: openapi3.SchemaRefs{leaf("informationMode")},
		}}, false},
		{"self-referencing schema terminates", cyclic, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schemaDeclaresProperty(tt.schema, "@type", map[*openapi3.Schema]bool{})
			assert.Equal(t, tt.want, got)
		})
	}
}

// A payload chooses the @context, so it chooses every document the loader then
// reads to resolve that document's $refs. The allowlist is consulted once, on
// the entry URL; these tests cover what happens after it.

const entrySchemaRefTemplate = `openapi: 3.1.0
info:
  title: entry
  version: "1"
paths: {}
components:
  schemas:
    TestType:
      type: object
      properties:
        field1:
          $ref: "REF_TARGET#/components/schemas/Borrowed"
`

const borrowedSchema = `openapi: 3.1.0
info:
  title: borrowed
  version: "1"
paths: {}
components:
  schemas:
    Borrowed:
      type: string
`

// serveSchema returns an https-less test server answering every path with body.
func serveSchema(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestValidateReferencedObject_RefusesARefThatWouldReadTheDisk(t *testing.T) {
	// A real file, so a successful read would be indistinguishable from a
	// legitimate schema and the test could not tell the two apart.
	onDisk := filepath.Join(t.TempDir(), "borrowed.yaml")
	if err := os.WriteFile(onDisk, []byte(borrowedSchema), 0o600); err != nil {
		t.Fatalf("failed to write the file under test: %v", err)
	}

	for _, tt := range []struct {
		name string
		ref  string
	}{
		{name: "file scheme", ref: "file://" + onDisk},
		{name: "bare path, which parses with no scheme at all", ref: onDisk},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := serveSchema(t, strings.Replace(entrySchemaRefTemplate, "REF_TARGET", tt.ref, 1))
			host, err := url.Parse(entry.URL)
			if err != nil {
				t.Fatalf("failed to parse the test server URL: %v", err)
			}

			cache := newSchemaCache(10)
			obj := referencedObject{
				Path:    "message.test",
				Context: entry.URL + "/context.jsonld",
				Type:    "TestType",
				Data:    map[string]interface{}{"field1": "value1"},
			}

			err = cache.validateReferencedObject(context.Background(), obj,
				1*time.Hour, 30*time.Second, []string{host.Host}, false)

			// The entry document is allowlisted and https, so nothing before
			// the $ref refuses this. Only the reader can.
			if err == nil {
				t.Fatal("the $ref was read, so a payload can name any file on disk")
			}
			assert.Contains(t, err.Error(), "refusing to read schema from")
		})
	}
}

// The packs pull 15 documents across 3 hosts -- the one the allowlist names
// plus two external spec hosts the packs $ref into -- so a $ref to a host
// outside the allowlist is the normal case, not the attack. This pins that:
// applying isAllowedDomain to $refs as well would need all three hosts named
// in the allowlist first, and would otherwise stop every pack loading.
// Deliberate, not missed.
func TestValidateReferencedObject_AllowsARefToAHostOutsideTheAllowlist(t *testing.T) {
	borrowed := serveSchema(t, borrowedSchema)
	entry := serveSchema(t, strings.Replace(entrySchemaRefTemplate, "REF_TARGET", borrowed.URL+"/borrowed.yaml", 1))

	entryHost, err := url.Parse(entry.URL)
	if err != nil {
		t.Fatalf("failed to parse the test server URL: %v", err)
	}
	borrowedHost, err := url.Parse(borrowed.URL)
	if err != nil {
		t.Fatalf("failed to parse the test server URL: %v", err)
	}
	if entryHost.Port() == borrowedHost.Port() {
		t.Fatal("the two servers must differ, or this proves nothing")
	}

	cache := newSchemaCache(10)
	obj := referencedObject{
		Path:    "message.test",
		Context: entry.URL + "/context.jsonld",
		Type:    "TestType",
		Data:    map[string]interface{}{"field1": "value1"},
	}

	// Only the entry host is allowlisted; the $ref host is not.
	if err := cache.validateReferencedObject(context.Background(), obj,
		1*time.Hour, 30*time.Second, []string{entryHost.Host}, false); err != nil {
		t.Fatalf("a cross-host $ref must still resolve, or no pack can load: %v", err)
	}
}

func TestPayloadDirectedReader(t *testing.T) {
	onDisk := filepath.Join(t.TempDir(), "schema.yaml")
	if err := os.WriteFile(onDisk, []byte(borrowedSchema), 0o600); err != nil {
		t.Fatalf("failed to write the file under test: %v", err)
	}

	for _, tt := range []struct {
		name    string
		raw     string
		refused bool
	}{
		{name: "file scheme", raw: "file://" + onDisk, refused: true},
		{name: "bare path", raw: onDisk, refused: true},
		{name: "a scheme nobody serves schemas over", raw: "gopher://example.test/schema.yaml", refused: true},
		{name: "http is read", raw: "", refused: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.raw
			if raw == "" {
				raw = serveSchema(t, borrowedSchema).URL + "/schema.yaml"
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("failed to parse %q: %v", raw, err)
			}

			data, err := payloadDirectedReader(openapi3.NewLoader(), u)
			if tt.refused {
				if err == nil {
					t.Fatalf("%q was read, and must not have been", raw)
				}
				assert.Contains(t, err.Error(), "only http and https are read")
				// The point is that nothing was read, not merely that it errored.
				assert.Empty(t, data)
				return
			}
			assert.NoError(t, err)
			assert.Contains(t, string(data), "Borrowed")
		})
	}
}
