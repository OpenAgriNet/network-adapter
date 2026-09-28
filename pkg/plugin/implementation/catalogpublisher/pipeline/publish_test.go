package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// publishTestPrefix is the filename prefix the pipeline's build step writes
// (build.output.filenamePrefix in the YAML), so the tests exercise the same
// naming convention a real run produces.
const publishTestPrefix = "mandi"

// writeCatalog writes one catalog file named <prefix>-<STATE>.json, shaped
// like a real publish body so the publish step can read its catalog id back.
func writeCatalog(t *testing.T, dir, state string) {
	t.Helper()
	id := "cat-mandi-" + state
	body := `{"context":{"action":"catalog/publish"},"message":{"catalogs":[{"id":"` + id +
		`","isActive":true,"resources":[{"id":"res:mandi:1"}]}],` +
		`"publishDirectives":[{"catalogId":"` + id + `"}]}}`
	path := filepath.Join(dir, publishTestPrefix+"-"+state+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fakePublisher stands in for the crawler's sink: it answers every body with
// one status and keeps what it was sent, so a test can assert what reached it
// -- or that nothing did. The HTTP call and the judgement of a real answer are
// the sink's, and tested there; what is tested here is the publish step's own
// rules.
type fakePublisher struct {
	status string

	mu      sync.Mutex
	urls    []string
	bodies  [][]byte
	retires []retireCall
}

// retireCall is one Retire the publish step asked for.
type retireCall struct{ baseURL, catalogID, descriptorName string }

func publisherAnswering(status string) *fakePublisher { return &fakePublisher{status: status} }

func (f *fakePublisher) Publish(_ context.Context, baseURL string, body []byte) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, baseURL)
	f.bodies = append(f.bodies, body)
	return Outcome{Status: f.status, Reason: "fake"}
}

func (f *fakePublisher) Retire(_ context.Context, baseURL string, r Retirement) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retires = append(f.retires, retireCall{baseURL, r.CatalogID, r.DescriptorName})
	return Outcome{CatalogID: r.CatalogID, Status: f.status, Reason: "fake"}
}

func (f *fakePublisher) retired() []retireCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]retireCall(nil), f.retires...)
}

func (f *fakePublisher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// testAdapter is the base address the tests hand the publish step.
const testAdapter = "http://adapter.test"

// goodSpec is the publish block the pipeline actually declares.
func goodSpec() Publish {
	return Publish{
		URL:        "${inputs.publishUrl}/publish",
		RefuseWhen: "collection.groupErrors > 0",
	}
}

func TestPublishCatalogsReportsAnAcceptedCatalogAsPublished(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}

	if pub.calls() != 1 {
		t.Errorf("posted %d times, want 1", pub.calls())
	}
	if len(result.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want 1", result.Outcomes)
	}
	if result.Outcomes[0].Status != StatusPublished {
		t.Errorf("status = %q, want %q", result.Outcomes[0].Status, StatusPublished)
	}
	if result.HasFailures() {
		t.Error("HasFailures is true after an ACCEPTED result")
	}
}

func TestPublishCatalogsTreatsPartialAsAFailure(t *testing.T) {
	// A PARTIAL is a catalog indexed with resources missing -- 273 markets
	// published as 128 findable ones -- so it must never read as success.
	pub := publisherAnswering(StatusRejected)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if !result.HasFailures() {
		t.Fatalf("HasFailures is false after a PARTIAL: %+v", result.Outcomes)
	}
	if result.Outcomes[0].Status != StatusRejected {
		t.Errorf("status = %q, want %q", result.Outcomes[0].Status, StatusRejected)
	}
}

func TestPublishCatalogsRefusesAPartialCollection(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, map[string]int{"groupErrors": 3}, pub)
	if err == nil {
		t.Fatal("publishCatalogs published a collection with 3 failed states")
	}
	if pub.calls() != 0 {
		t.Errorf("posted %d times after refusing, want 0", pub.calls())
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q does not say how many states failed", err)
	}
}

func TestPublishCatalogsPublishesWhenRefuseWhenIsNotDeclared(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	spec := goodSpec()
	spec.RefuseWhen = ""

	if _, err := PublishCatalogs(context.Background(), spec,
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, map[string]int{"groupErrors": 3}, pub); err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if pub.calls() != 1 {
		t.Errorf("posted %d times, want 1", pub.calls())
	}
}

func TestPublishCatalogsNeedsAPublishURL(t *testing.T) {
	// The hint the run fills in from the pipeline's own publishUrl input.
	spec := goodSpec()
	spec.AddressHint = publishAddressHintFor(Input{Flag: "publish-url", Env: "CATALOG_PUBLISH_URL"})
	_, err := PublishCatalogs(context.Background(), spec,
		map[string]string{}, t.TempDir(), publishTestPrefix, nil, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted an empty publish address")
	}
	if !strings.Contains(err.Error(), "publishUrl") || !strings.Contains(err.Error(), "CATALOG_PUBLISH_URL") {
		t.Errorf("error %q names neither the input nor its environment variable", err)
	}
}

// TestPublishCatalogsRejectsAPublishURLItWouldIgnore: the address actually
// used comes from inputs.publishUrl, so a publish.url pointing anywhere else
// would be silently ignored -- an operator's edit taking no effect, with no
// message saying so.
func TestPublishCatalogsRejectsAPublishURLItWouldIgnore(t *testing.T) {
	spec := goodSpec()
	spec.URL = "https://somewhere.else.test/publish"

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogs(context.Background(), spec,
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, nil, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted a publish.url it does not honour")
	}
	if !strings.Contains(err.Error(), "somewhere.else.test") {
		t.Errorf("error %q does not quote the ignored url", err)
	}
}

// TestPublishCatalogsRefusesAnUnresolvableRetireOld: the file's retireOld
// block is gated on ${inputs.retireOld}, an input the file never declares.
// Reading that as "off" would silently skip a retirement somebody wrote the
// block specifically to get.
func TestPublishCatalogsRefusesAnUnresolvableRetireOld(t *testing.T) {
	spec := goodSpec()
	spec.RetireOld = RetireOld{
		Enabled:        "${inputs.retireOld}",
		CatalogID:      "cat-agmarknet-mandi-prices",
		DescriptorName: "Retired: superseded by the per-state market catalogs",
	}

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogs(context.Background(), spec,
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, nil, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted a retireOld gated on an undeclared input")
	}
	if !strings.Contains(err.Error(), "retireOld") {
		t.Errorf("error %q does not name the block it refused", err)
	}
}

// TestPublishCatalogsCarriesAnEnabledRetireOld proves the block reaches
// the publisher rather than being parsed and dropped: an enabled retirement
// retires the named catalog alongside publishing the current ones.
func TestPublishCatalogsCarriesAnEnabledRetireOld(t *testing.T) {
	spec := goodSpec()
	spec.RetireOld = RetireOld{
		Enabled:        "${inputs.retireOld}",
		CatalogID:      "cat-agmarknet-mandi-prices",
		DescriptorName: "Retired: superseded by the per-state market catalogs",
	}

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	pub := publisherAnswering(StatusPublished)

	result, err := PublishCatalogs(context.Background(), spec, map[string]string{
		"publishUrl": testAdapter,
		"retireOld":  "true",
	}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("retireOld was declared and enabled, but no retirement was attempted")
	}
	// The catalog was published and the old one retired, both through the
	// publisher -- which builds the retirement body, identity and all.
	want := retireCall{testAdapter, "cat-agmarknet-mandi-prices", "Retired: superseded by the per-state market catalogs"}
	if pub.calls() != 1 || len(pub.retired()) != 1 || pub.retired()[0] != want {
		t.Errorf("publisher got %d bodies and retires %+v; want the catalog, then Retire(%+v)",
			pub.calls(), pub.retired(), want)
	}
	if result.RetiredOld.CatalogID != want.catalogID {
		t.Errorf("RetiredOld = %+v, want catalogId %s", result.RetiredOld, want.catalogID)
	}
}

// The file goes to the publisher VERBATIM, with the base address -- the
// publisher appends /publish, so a rendered .../publish would double it.
func TestPublishCatalogsHandsTheFileVerbatimWithTheBaseAddress(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	want, _ := os.ReadFile(filepath.Join(dir, publishTestPrefix+"-MH.json"))
	pub := publisherAnswering(StatusPublished)

	result, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("PublishCatalogs: %v", err)
	}
	if pub.urls[0] != testAdapter || string(pub.bodies[0]) != string(want) {
		t.Errorf("publisher got url %q body %s; want %q and the file verbatim", pub.urls[0], pub.bodies[0], testAdapter)
	}
	if got := result.Outcomes[0]; got.CatalogID != "cat-mandi-MH" || got.Group != "MH" {
		t.Errorf("outcome = %+v, want the id from the body and the group from the filename", got)
	}
}

// Asked to publish with nothing to publish through is refused, not quietly
// turned into a build-only run.
func TestPublishCatalogsRefusesWithoutAPublisher(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	_, err := PublishCatalogs(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "publisher") {
		t.Fatalf("err = %v, want a refusal naming the missing publisher", err)
	}
}

// The "no publish address" error names the flag and env the pipeline FILE
// declares, not a name fixed in Go: a hardcoded hint once told a pipeline to
// set a variable it never read.
func TestPublishAddressHintNamesThePipelinesOwnInput(t *testing.T) {
	hint := publishAddressHintFor(Input{Flag: "send-to", Env: "EXAMPLE_PUBLISH_URL"})
	if !strings.Contains(hint, "EXAMPLE_PUBLISH_URL") || !strings.Contains(hint, "--send-to") {
		t.Errorf("hint %q does not name the declared flag and env", hint)
	}

	spec := goodSpec()
	spec.AddressHint = hint
	_, err := PublishCatalogs(context.Background(), spec, map[string]string{}, t.TempDir(), "x", nil, publisherAnswering(StatusPublished))
	if err == nil || !strings.Contains(err.Error(), "EXAMPLE_PUBLISH_URL") {
		t.Errorf("err = %v, want it to name EXAMPLE_PUBLISH_URL", err)
	}
}

// The retirement must not go out when its replacements did not.
//
// retireOld posts a TOMBSTONE: it deactivates the old catalog, which is how
// that catalog's resources leave the network. Sending it after a run whose
// new catalogs were all rejected removes the old data and puts nothing in
// its place -- the network is left with neither. The next tick then retries
// and sends the tombstone again.
func TestRetireIsNotSentWhenEveryPublishFailed(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	pub := publisherAnswering(StatusRejected)
	spec := Publish{
		URL:       "${inputs.publishUrl}/publish",
		RetireOld: RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old-monolith"},
	}
	resolved := map[string]string{"publishUrl": testAdapter, "retireOld": "true"}

	result, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("PublishCatalogs: %v", err)
	}
	if result.RetiredOld != nil {
		t.Error("the old catalog was retired although every replacement was rejected; " +
			"the network is left with neither the old data nor the new")
	}
	// One call for the catalog, no retirement.
	if got := pub.calls(); got != 1 || len(pub.retired()) != 0 {
		t.Errorf("publisher saw %d publishes and %d retires, want 1 and 0", got, len(pub.retired()))
	}
}

// And it MUST still go out on a healthy run, or the migration never completes
// and the check above is just breakage.
func TestRetireIsSentWhenEveryPublishSucceeded(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	pub := publisherAnswering(StatusPublished)
	spec := Publish{
		URL:       "${inputs.publishUrl}/publish",
		RetireOld: RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old-monolith"},
	}
	resolved := map[string]string{"publishUrl": testAdapter, "retireOld": "true"}

	result, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix, nil, pub)
	if err != nil {
		t.Fatalf("PublishCatalogs: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("a healthy run did not retire the old catalog, so the migration never completes")
	}
	if got := pub.calls(); got != 1 || len(pub.retired()) != 1 {
		t.Errorf("publisher saw %d publishes and %d retires, want 1 and 1", got, len(pub.retired()))
	}
}

func TestRefuseWhenAcceptsAThreshold(t *testing.T) {
	counter, threshold, ok := refuseWhenRule("collection.groupErrors > 3")
	if !ok || counter != "groupErrors" || threshold != 3 {
		t.Fatalf("refuseWhenRule = %q, %d, %v; want groupErrors, 3, true", counter, threshold, ok)
	}
	if _, _, ok := refuseWhenRule("collection.groupErrors >= 3"); ok {
		t.Fatal(">= was accepted; only > N is understood")
	}
}

// A pipeline that tolerates three failed groups publishes with three and
// refuses with four.
func TestPublishRefusesOnlyAboveTheThreshold(t *testing.T) {
	spec := goodSpec()
	spec.RefuseWhen = "collection.groupErrors > 3"
	resolved := map[string]string{"publishUrl": testAdapter}

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	if _, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix,
		map[string]int{"groupErrors": 3}, publisherAnswering(StatusPublished)); err != nil {
		t.Fatalf("three failed groups under `> 3` were refused: %v", err)
	}

	pub := publisherAnswering(StatusPublished)
	_, err := PublishCatalogs(context.Background(), spec, resolved, dir, publishTestPrefix,
		map[string]int{"groupErrors": 4}, pub)
	if err == nil || !strings.Contains(err.Error(), "refusing to publish") {
		t.Fatalf("four failed groups under `> 3`: err = %v, want a refusal", err)
	}
	if pub.calls() != 0 {
		t.Errorf("a refused collection still posted %d times", pub.calls())
	}
}

// The retireOld block is resolved into everything the retirement envelope
// carries: its defaults where the file is silent, its ${inputs.*} references
// resolved, so the deactivation reaches the same audience, under the same
// schema, as the catalogs that supersede it.
func TestRetirementResolvesTheWholeBlock(t *testing.T) {
	resolved := map[string]string{"retireOld": "true", "networkId": "oan-prod"}
	got, err := retirement(RetireOld{
		Enabled:        "${inputs.retireOld}",
		CatalogID:      "cat-old",
		DescriptorName: "Retired",
		VisibleTo:      []string{"${inputs.networkId}"},
		SchemaTypes:    []string{"https://schema.example/MandiPrice/context.jsonld"},
	}, resolved)
	if err != nil {
		t.Fatalf("retirement: %v", err)
	}
	want := Retirement{
		CatalogID: "cat-old", DescriptorName: "Retired",
		CatalogType: "REGULAR", UpdateMode: "MERGE",
		VisibleTo:   []string{"oan-prod"},
		SchemaTypes: []string{"https://schema.example/MandiPrice/context.jsonld"},
	}
	if got == nil || fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Fatalf("retirement = %+v, want %+v", got, want)
	}

	full, err := retirement(RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old",
		UpdateMode: "FULL", CatalogType: "MASTER"}, resolved)
	if err != nil || full.UpdateMode != "FULL" || full.CatalogType != "MASTER" {
		t.Fatalf("declared updateMode/catalogType: %+v, %v; want them passed through", full, err)
	}
}

// A retirement is a deactivation; a block saying isActive: true, or naming an
// input that does not resolve, is refused rather than quietly ignored.
func TestRetirementRefusesWhatItCannotHonour(t *testing.T) {
	resolved := map[string]string{"retireOld": "true"}
	yes := true
	for name, block := range map[string]RetireOld{
		"isActive true":         {Enabled: "${inputs.retireOld}", CatalogID: "c", IsActive: &yes},
		"an unresolved input":   {Enabled: "${inputs.retireOld}", CatalogID: "c", VisibleTo: []string{"${inputs.nope}"}},
		"an unknown updateMode": {Enabled: "${inputs.retireOld}", CatalogID: "c", UpdateMode: "REPLACE"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := retirement(block, resolved); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}
