package AgricultureFacility_test

// schemacache_test.go fetches the AgricultureFacility v0.1 schema pack from
// where it is actually published, and caches it under testdata/schema-cache.
//
// This package used to carry the nine schemas the pack reaches as one vendored
// 700-line multi-document YAML file, with a hand-written table naming the URI
// each document had to be registered under. Two problems with that. It was a
// transcription of the pack that could disagree with the pack and only a manual
// refresh would notice. And the base URI the table registered the OAN packs
// under -- https://schemas.openagrinet.global/... -- has no DNS record at all,
// so the one address the tests presented as the schema's identity was an
// address nothing serves.
//
// Now the validator resolves the pack's $refs itself and this loader answers
// them: once over the network, then from disk. So the base URIs are the real,
// dereferenceable ones,
//
//	https://raw.githubusercontent.com/OpenAgriNet/network-specs/<commit>/schema/AgricultureFacility/v0.1
//
// the relative refs the pack writes ("../../AgricultureResource/v0.1/...")
// resolve against them with no table to keep in step, and the beckn.io
// schemas -- which redirect to schema.nfh.global, and which the two sides
// disagree about the naming of -- are simply loaded under whichever name the
// ref that reached them was written with.
//
// The cache is not committed; see testdata/.gitignore. A cold run needs the
// network, and every run after it is offline and deterministic. With an empty
// cache AND no network the schema tests SKIP rather than fail, because they
// cannot be performed at all on that machine -- what they cover that does not
// need the pack is covered offline in mappings_test.go.
//
// To refresh against upstream, empty the cache and run the tests:
//
//	rm -rf pkg/plugin/implementation/AgricultureFacility/testdata/schema-cache
//	go test ./pkg/plugin/implementation/AgricultureFacility/

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// The published pack. Pinned to a COMMIT, not the schema-packs-v0.1 branch --
// a suite whose verdict depends on the day it ran is not a conformance suite,
// and a branch name in a raw.githubusercontent.com URL is not a pin at all:
// it serves whatever that branch's tip currently is.
//
// That distinction is not academic. commit b76c9ad8a5 on schema-packs-v0.1,
// titled "docs #5: generate composed schema reference pages," silently
// dropped the `not: anyOf: [...]` clause that makes an OnDemand resource
// reject facilityType/location/address/services/capacity/publicContact/
// website/source/lastUpdatedAt -- exactly the constraint
// TestTheRequestResourceSatisfiesOnDemandMode exists to verify, and exactly
// the constraint this plugin's whole design (see the package doc and
// README's "Where the query lives") depends on. A branch pin would have
// picked that up silently on the next cold cache; this pin does not move
// until a human reads the upstream diff and updates it.
//
// a39d2f3723 is the last commit before that regression. Bump this after
// confirming the successor commit still carries the `not:` clause -- diff it
// against https://github.com/OpenAgriNet/network-specs/commits/schema-packs-v0.1
// -- rather than moving straight to the branch tip.
const (
	packBase = "https://raw.githubusercontent.com/OpenAgriNet/network-specs/a39d2f372349401df5e4f5efd9e8b6992ae60d46/schema"

	facilityPackURL = packBase + "/AgricultureFacility/v0.1/attributes.yaml"
	resourcePackURL = packBase + "/AgricultureResource/v0.1/attributes.yaml"

	// The pack's own examples, published alongside it.
	packExampleBase = packBase + "/AgricultureFacility/v0.1/examples"
)

// schemaCacheDir mirrors each fetched URL as host/path, so what is on disk says
// where it came from without an index to consult.
const schemaCacheDir = "testdata/schema-cache"

// fetchTimeout is per document. Generous, because a cold cache pulls a dozen
// of them.
const fetchTimeout = 30 * time.Second

// maxSchemaSize caps what is read from a response. The largest of these
// documents is a few kilobytes; anything near this is not a schema.
const maxSchemaSize = 4 << 20

// packLoader answers the validator's $ref lookups, from the cache when it can
// and from the network when it must.
//
// It records what it loaded so a test can assert the schema was compiled out of
// the pack rather than out of whatever else happened to be cached, and records
// the first fetch failure so an offline machine can be told apart from a broken
// schema.
type packLoader struct {
	mu       sync.Mutex
	loaded   map[string]bool
	fetchErr error
}

func newPackLoader() *packLoader {
	return &packLoader{loaded: map[string]bool{}}
}

// Load satisfies jsonschema.URLLoader.
func (l *packLoader) Load(url string) (any, error) {
	raw, err := l.text(url)
	if err != nil {
		return nil, err
	}
	return decodeSchema(url, raw)
}

// text returns a document's bytes verbatim, which is what the governed-field
// check needs -- it reads the pack as YAML text rather than as a compiled
// schema.
func (l *packLoader) text(url string) ([]byte, error) {
	cached, err := cachePath(url)
	if err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(cached); err == nil {
		l.markLoaded(url)
		return raw, nil
	}

	raw, err := fetchSchema(url)
	if err != nil {
		l.mu.Lock()
		if l.fetchErr == nil {
			l.fetchErr = err
		}
		l.mu.Unlock()
		return nil, err
	}
	if err := writeCache(cached, raw); err != nil {
		return nil, err
	}
	l.markLoaded(url)
	return raw, nil
}

func (l *packLoader) markLoaded(url string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loaded[url] = true
}

func (l *packLoader) sawLoaded(url string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loaded[url]
}

// skipIfOffline turns "the pack could not be reached and was not cached" into a
// skip, and leaves every other failure to the caller -- including a reachable
// server that answered with something other than 200, which is not offline,
// it is a finding.
//
// The distinction is the point. A schema that no longer compiles is a finding; a
// laptop on a train is not. A 404 for a pack that moved or was deleted is a
// finding too, not a train.
func (l *packLoader) skipIfOffline(t *testing.T, cause error) {
	t.Helper()

	l.mu.Lock()
	fetchErr := l.fetchErr
	l.mu.Unlock()
	if fetchErr == nil {
		return
	}
	var status httpStatusError
	if errors.As(fetchErr, &status) {
		return
	}
	t.Skipf("the schema pack is neither cached under %s nor reachable, so conformance "+
		"cannot be checked here -- run once with network access to prime the cache.\n"+
		"fetch: %v\nfailure: %v", schemaCacheDir, fetchErr, cause)
}

// decodeSchema converts one fetched document into the shape the validator
// takes.
//
// The packs are published as YAML and the beckn examples as JSON. YAML parses
// both, since JSON is a subset of it, and the round trip through JSON is a
// format conversion and nothing more: no key is renamed and no value is
// touched. It is there so numbers and keys arrive as the validator's own types
// rather than as yaml's.
func decodeSchema(url string, raw []byte) (any, error) {
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%s is not YAML or JSON: %w", url, err)
	}
	asJSON, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("%s could not be converted to JSON: %w", url, err)
	}
	return jsonschema.UnmarshalJSON(strings.NewReader(string(asJSON)))
}

// cachePath maps a URL to its place in the cache, and refuses anything that
// would write outside it.
func cachePath(rawURL string) (string, error) {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("could not parse %s: %w", rawURL, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("refusing to load %s: the pack is fetched over https only", rawURL)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("refusing to load %s: no host", rawURL)
	}
	clean := path.Clean("/" + parsed.Path)
	if strings.Contains(clean, "..") || strings.ContainsAny(parsed.Host, `/\`) {
		return "", fmt.Errorf("refusing to cache %s: the path escapes %s", rawURL, schemaCacheDir)
	}
	return filepath.Join(schemaCacheDir, parsed.Host, filepath.FromSlash(clean)), nil
}

// httpStatusError marks a fetch that reached the server and got back
// something other than 200 -- a reachable pack that 404s or 500s, not an
// unreachable one. skipIfOffline uses the type to tell the two apart.
type httpStatusError struct {
	url    string
	status string
}

func (e httpStatusError) Error() string {
	return fmt.Sprintf("GET %s: %s", e.url, e.status)
}

// fetchSchema retrieves one document. Redirects are followed -- schema.beckn.io
// 301s to schema.nfh.global -- but the URL the ref was written with is what the
// document is cached and registered under, which is what makes the refs
// resolve.
func fetchSchema(url string) ([]byte, error) {
	client := &http.Client{Timeout: fetchTimeout}
	response, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, httpStatusError{url: url, status: response.Status}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSchemaSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	if len(raw) > maxSchemaSize {
		return nil, fmt.Errorf("%s is larger than %d bytes, which no schema in this pack is",
			url, maxSchemaSize)
	}
	return raw, nil
}

// writeCache writes through a temporary file in the same directory, so an
// interrupted run leaves no half-written document behind for the next one to
// parse.
func writeCache(cached string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		return fmt.Errorf("could not create %s: %w", filepath.Dir(cached), err)
	}
	temp, err := os.CreateTemp(filepath.Dir(cached), "."+filepath.Base(cached)+".*")
	if err != nil {
		return fmt.Errorf("could not stage %s: %w", cached, err)
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("could not write %s: %w", temp.Name(), err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("could not close %s: %w", temp.Name(), err)
	}
	if err := os.Rename(temp.Name(), cached); err != nil {
		return fmt.Errorf("could not move %s into place: %w", cached, err)
	}
	return nil
}
