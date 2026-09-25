package pipeline

import (
	"context"
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

	mu     sync.Mutex
	urls   []string
	bodies [][]byte
}

func publisherAnswering(status string) *fakePublisher { return &fakePublisher{status: status} }

func (f *fakePublisher) Publish(_ context.Context, baseURL string, body []byte) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, baseURL)
	f.bodies = append(f.bodies, body)
	return Outcome{Status: f.status, Reason: "fake"}
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
		URL:            "${inputs.publishUrl}/publish",
		Concurrency:    1,
		Timeout:        "180s",
		Accept:         []string{"ACCEPTED"},
		TreatAsFailure: []string{"PARTIAL", "REJECTED"},
		RefuseWhen:     "collection.stateErrors > 0",
	}
}

func TestPublishCatalogsReportsAnAcceptedCatalogAsPublished(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 0, pub)
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

	result, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 0, pub)
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

	_, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 3, pub)
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

	if _, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 3, pub); err != nil {
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
	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{}, t.TempDir(), publishTestPrefix, 0, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted an empty publish address")
	}
	if !strings.Contains(err.Error(), "publishUrl") || !strings.Contains(err.Error(), "CATALOG_PUBLISH_URL") {
		t.Errorf("error %q names neither the input nor its environment variable", err)
	}
}

func TestPublishCatalogsRejectsASpecThatAcceptsPartial(t *testing.T) {
	pub := publisherAnswering(StatusPublished)

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	spec := goodSpec()
	spec.Accept = []string{"PARTIAL"}

	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 0, pub)
	if err == nil {
		t.Fatal("publishCatalogs honoured a spec accepting PARTIAL it cannot honour")
	}
	if !strings.Contains(err.Error(), "PARTIAL") {
		t.Errorf("error %q does not name the status it cannot accept", err)
	}
	if pub.calls() != 0 {
		t.Errorf("posted %d times despite an unhonourable spec, want 0", pub.calls())
	}
}

func TestPublishCatalogsRejectsASpecThatDoesNotFailOnPartial(t *testing.T) {
	spec := goodSpec()
	spec.TreatAsFailure = []string{"REJECTED"}

	// A real catalogue file and a live server, so the call would otherwise
	// SUCCEED. With an empty directory this test passed even with the
	// judgement check removed -- the publish step would have returned "no
	// catalogue files in ..." and the assertion could not tell the two apart.
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	pub := publisherAnswering(StatusPublished)

	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 0, pub)
	if err == nil {
		t.Fatal("publishCatalogs honoured a spec that does not treat PARTIAL as a failure")
	}
	if !strings.Contains(err.Error(), "PARTIAL") {
		t.Errorf("error %q does not name PARTIAL, so it may be reporting something else entirely", err)
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

	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, 0, publisherAnswering(StatusPublished))
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

	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, 0, publisherAnswering(StatusPublished))
	if err == nil {
		t.Fatal("publishCatalogs accepted a retireOld gated on an undeclared input")
	}
	if !strings.Contains(err.Error(), "retireOld") {
		t.Errorf("error %q does not name the block it refused", err)
	}
}

// TestPublishCatalogsCarriesAnEnabledRetireOld proves the block reaches
// the publisher rather than being parsed and dropped: an enabled retirement
// posts a tombstone for the named catalog alongside the current ones.
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

	result, err := PublishCatalogues(context.Background(), spec, map[string]string{
		"publishUrl": testAdapter,
		"retireOld":  "true",
	}, dir, publishTestPrefix, 0, pub)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("retireOld was declared and enabled, but no retirement was attempted")
	}
	// The catalogue AND the tombstone went through the publisher.
	if pub.calls() != 2 || !strings.Contains(string(pub.bodies[1]), `"isActive":false`) {
		t.Errorf("publisher got %d bodies, want the catalogue then a tombstone", pub.calls())
	}
}

// The file goes to the publisher VERBATIM, with the base address -- the
// publisher appends /publish, so a rendered .../publish would double it.
func TestPublishCatalogsHandsTheFileVerbatimWithTheBaseAddress(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	want, _ := os.ReadFile(filepath.Join(dir, publishTestPrefix+"-MH.json"))
	pub := publisherAnswering(StatusPublished)

	result, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 0, pub)
	if err != nil {
		t.Fatalf("PublishCatalogues: %v", err)
	}
	if pub.urls[0] != testAdapter || string(pub.bodies[0]) != string(want) {
		t.Errorf("publisher got url %q body %s; want %q and the file verbatim", pub.urls[0], pub.bodies[0], testAdapter)
	}
	if got := result.Outcomes[0]; got.CatalogID != "cat-mandi-MH" || got.StateCode != "MH" {
		t.Errorf("outcome = %+v, want the id from the body and the group from the filename", got)
	}
}

// Asked to publish with nothing to publish through is refused, not quietly
// turned into a build-only run.
func TestPublishCatalogsRefusesWithoutAPublisher(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	_, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": testAdapter}, dir, publishTestPrefix, 0, nil)
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
	_, err := PublishCatalogues(context.Background(), spec, map[string]string{}, t.TempDir(), "x", 0, publisherAnswering(StatusPublished))
	if err == nil || !strings.Contains(err.Error(), "EXAMPLE_PUBLISH_URL") {
		t.Errorf("err = %v, want it to name EXAMPLE_PUBLISH_URL", err)
	}
}

// The retirement must not go out when its replacements did not.
//
// retireOld posts a TOMBSTONE: it deactivates the old catalogue, which is how
// that catalogue's resources leave the network. Sending it after a run whose
// new catalogues were all rejected removes the old data and puts nothing in
// its place -- the network is left with neither. The next tick then retries
// and sends the tombstone again.
func TestRetireIsNotSentWhenEveryPublishFailed(t *testing.T) {
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	pub := publisherAnswering(StatusRejected)
	spec := Publish{
		URL:       "${inputs.publishUrl}/publish",
		Accept:    []string{"ACCEPTED"},
		RetireOld: RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old-monolith"},
	}
	resolved := map[string]string{"publishUrl": testAdapter, "retireOld": "true"}

	result, err := PublishCatalogues(context.Background(), spec, resolved, dir, publishTestPrefix, 0, pub)
	if err != nil {
		t.Fatalf("PublishCatalogues: %v", err)
	}
	if result.RetiredOld != nil {
		t.Error("the old catalogue was retired although every replacement was rejected; " +
			"the network is left with neither the old data nor the new")
	}
	// One call for the catalogue, none for the tombstone.
	if got := pub.calls(); got != 1 {
		t.Errorf("publisher saw %d calls, want 1 (the catalogue only, no tombstone)", got)
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
		Accept:    []string{"ACCEPTED"},
		RetireOld: RetireOld{Enabled: "${inputs.retireOld}", CatalogID: "cat-old-monolith"},
	}
	resolved := map[string]string{"publishUrl": testAdapter, "retireOld": "true"}

	result, err := PublishCatalogues(context.Background(), spec, resolved, dir, publishTestPrefix, 0, pub)
	if err != nil {
		t.Fatalf("PublishCatalogues: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("a healthy run did not retire the old catalogue, so the migration never completes")
	}
	if got := pub.calls(); got != 2 {
		t.Errorf("publisher saw %d calls, want 2 (the catalogue and the tombstone)", got)
	}
}
