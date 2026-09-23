package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/catalogpublish"
)

// publishTestPrefix is the filename prefix the pipeline's build step writes
// (build.output.filenamePrefix in the YAML), so the tests exercise the same
// naming convention a real run produces.
const publishTestPrefix = "mandi"

// writeCatalog writes one catalog file named <prefix>-<STATE>.json, shaped
// like a real publish body so catalogpublish can read its catalog id back.
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

// answerServer answers every POST with a single result carrying status, and
// counts the calls so a test can assert the server was never reached.
func answerServer(t *testing.T, status string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// The pipeline's declared url already carries /publish; passing it
		// through untrimmed would land on /publish/publish.
		if r.URL.Path != "/publish" {
			t.Errorf("posted to %q, want /publish", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"context": map[string]any{"action": "catalog/on_publish"},
			"message": map[string]any{"results": []any{
				map[string]any{"catalogId": "cat-mandi-MH", "status": status},
			}},
		})
	}))
	return server, &calls
}

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
	server, calls := answerServer(t, "ACCEPTED")
	defer server.Close()

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": server.URL}, dir, publishTestPrefix, 0)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}

	if *calls != 1 {
		t.Errorf("posted %d times, want 1", *calls)
	}
	if len(result.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want 1", result.Outcomes)
	}
	if result.Outcomes[0].Status != catalogpublish.StatusPublished {
		t.Errorf("status = %q, want %q", result.Outcomes[0].Status, catalogpublish.StatusPublished)
	}
	if result.HasFailures() {
		t.Error("HasFailures is true after an ACCEPTED result")
	}
}

func TestPublishCatalogsTreatsPartialAsAFailure(t *testing.T) {
	// A PARTIAL is a catalog indexed with resources missing -- 273 markets
	// published as 128 findable ones -- so it must never read as success.
	server, _ := answerServer(t, "PARTIAL")
	defer server.Close()

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	result, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": server.URL}, dir, publishTestPrefix, 0)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if !result.HasFailures() {
		t.Fatalf("HasFailures is false after a PARTIAL: %+v", result.Outcomes)
	}
	if result.Outcomes[0].Status != catalogpublish.StatusRejected {
		t.Errorf("status = %q, want %q", result.Outcomes[0].Status, catalogpublish.StatusRejected)
	}
}

func TestPublishCatalogsRefusesAPartialCollection(t *testing.T) {
	server, calls := answerServer(t, "ACCEPTED")
	defer server.Close()

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	_, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{"publishUrl": server.URL}, dir, publishTestPrefix, 3)
	if err == nil {
		t.Fatal("publishCatalogs published a collection with 3 failed states")
	}
	if *calls != 0 {
		t.Errorf("posted %d times after refusing, want 0", *calls)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("error %q does not say how many states failed", err)
	}
}

func TestPublishCatalogsPublishesWhenRefuseWhenIsNotDeclared(t *testing.T) {
	server, calls := answerServer(t, "ACCEPTED")
	defer server.Close()

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	spec := goodSpec()
	spec.RefuseWhen = ""

	if _, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": server.URL}, dir, publishTestPrefix, 3); err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if *calls != 1 {
		t.Errorf("posted %d times, want 1", *calls)
	}
}

func TestPublishCatalogsNeedsAPublishURL(t *testing.T) {
	_, err := PublishCatalogues(context.Background(), goodSpec(),
		map[string]string{}, t.TempDir(), publishTestPrefix, 0)
	if err == nil {
		t.Fatal("publishCatalogs accepted an empty publish address")
	}
	if !strings.Contains(err.Error(), "publishUrl") || !strings.Contains(err.Error(), "MANDI_PUBLISH_URL") {
		t.Errorf("error %q names neither the input nor its environment variable", err)
	}
}

func TestPublishCatalogsRejectsASpecThatAcceptsPartial(t *testing.T) {
	server, calls := answerServer(t, "ACCEPTED")
	defer server.Close()

	dir := t.TempDir()
	writeCatalog(t, dir, "MH")

	spec := goodSpec()
	spec.Accept = []string{"PARTIAL"}

	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": server.URL}, dir, publishTestPrefix, 0)
	if err == nil {
		t.Fatal("publishCatalogs honoured a spec accepting PARTIAL it cannot honour")
	}
	if !strings.Contains(err.Error(), "PARTIAL") {
		t.Errorf("error %q does not name the status it cannot accept", err)
	}
	if *calls != 0 {
		t.Errorf("posted %d times despite an unhonourable spec, want 0", *calls)
	}
}

func TestPublishCatalogsRejectsASpecThatDoesNotFailOnPartial(t *testing.T) {
	spec := goodSpec()
	spec.TreatAsFailure = []string{"REJECTED"}

	// A real catalogue file and a live server, so the call would otherwise
	// SUCCEED. With an empty directory this test passed even with the
	// judgement check removed -- catalogpublish would have returned "no
	// catalog files in ..." and the assertion could not tell the two apart.
	dir := t.TempDir()
	writeCatalog(t, dir, "MH")
	server, _ := answerServer(t, "ACCEPTED")

	_, err := PublishCatalogues(context.Background(), spec,
		map[string]string{"publishUrl": server.URL}, dir, publishTestPrefix, 0)
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
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, 0)
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
		map[string]string{"publishUrl": "http://127.0.0.1:1"}, dir, publishTestPrefix, 0)
	if err == nil {
		t.Fatal("publishCatalogs accepted a retireOld gated on an undeclared input")
	}
	if !strings.Contains(err.Error(), "retireOld") {
		t.Errorf("error %q does not name the block it refused", err)
	}
}

// TestPublishCatalogsCarriesAnEnabledRetireOld proves the block reaches
// catalogpublish rather than being parsed and dropped: an enabled retirement
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
	server, _ := answerServer(t, "ACCEPTED")

	result, err := PublishCatalogues(context.Background(), spec, map[string]string{
		"publishUrl": server.URL,
		"retireOld":  "true",
	}, dir, publishTestPrefix, 0)
	if err != nil {
		t.Fatalf("publishCatalogs: %v", err)
	}
	if result.RetiredOld == nil {
		t.Fatal("retireOld was declared and enabled, but no retirement was attempted")
	}
}
