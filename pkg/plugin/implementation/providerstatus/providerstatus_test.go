package providerstatus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

type fakeLister struct {
	ids   []string
	err   error
	calls int
}

func (f *fakeLister) ActiveProviderIDs(ctx context.Context) ([]string, error) {
	f.calls++
	return f.ids, f.err
}

// bareRegistry implements RegistryLookup only, so it cannot list providers.
type bareRegistry struct{}

func (bareRegistry) Lookup(ctx context.Context, req *model.Subscription) ([]model.Subscription, error) {
	return nil, nil
}

// listingRegistry is a RegistryLookup that can also list active providers.
type listingRegistry struct {
	bareRegistry
	*fakeLister
}

func newStep(t *testing.T, l *fakeLister) definition.Step {
	t.Helper()
	s, err := New(listingRegistry{fakeLister: l})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func publishBody(action string, providerIDs ...string) []byte {
	catalogs := make([]string, 0, len(providerIDs))
	for _, id := range providerIDs {
		if id == "" {
			catalogs = append(catalogs, `{"id":"c"}`)
			continue
		}
		catalogs = append(catalogs, fmt.Sprintf(`{"id":"c","provider":{"id":%q}}`, id))
	}
	return []byte(fmt.Sprintf(`{"context":{"action":%q},"message":{"catalogs":[%s]}}`,
		action, strings.Join(catalogs, ",")))
}

func run(s definition.Step, body []byte) error {
	return s.Run(&model.StepContext{Context: context.Background(), Body: body})
}

func TestNewRefusesARegistryThatCannotListProviders(t *testing.T) {
	if _, err := New(bareRegistry{}); err == nil {
		t.Fatal("expected an error for a registry that cannot list providers")
	}
}

func TestPublishAllowsAnActiveProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), publishBody("catalog/publish", "p1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.calls != 1 {
		t.Fatalf("lister calls = %d, want 1", l.calls)
	}
}

func TestPublishRejectsAnUnknownOrInactiveProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	err := run(newStep(t, l), publishBody("catalog/publish", "ghost"))
	var coded *model.CodedErr
	if !errors.As(err, &coded) || coded.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("want a bad-request error, got %v", err)
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name the provider", err)
	}
}

func TestPublishRejectsTheWholeBatchOnOneBadProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	err := run(newStep(t, l), publishBody("catalog/publish", "p1", "ghost"))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want an error naming ghost, got %v", err)
	}
	if l.calls != 1 {
		t.Fatalf("lister calls = %d, want 1", l.calls)
	}
}

func TestPublishRejectsACatalogThatNamesNoProvider(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), publishBody("catalog/publish", "p1", "")); err == nil {
		t.Fatal("expected an error for a catalog with no provider")
	}
}

func TestPublishRejectsAnEmptyCatalogList(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	if err := run(newStep(t, l), publishBody("catalog/publish")); err == nil {
		t.Fatal("expected an error for an empty catalogs list")
	}
}

func TestPublishFailsClosedWhenTheRegistryErrors(t *testing.T) {
	l := &fakeLister{err: errors.New("registry down")}
	if err := run(newStep(t, l), publishBody("catalog/publish", "p1")); err == nil {
		t.Fatal("expected an error when the registry fails")
	}
}

func TestPublishAcceptsTheBarePublishAction(t *testing.T) {
	l := &fakeLister{ids: []string{"p1"}}
	s := newStep(t, l)
	if err := run(s, publishBody("publish", "p1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := run(s, publishBody("publish", "ghost")); err == nil {
		t.Fatal("bare publish must run the gate")
	}
}

func TestOtherActionsPassUntouched(t *testing.T) {
	l := &fakeLister{err: errors.New("registry down")}
	if err := run(newStep(t, l), []byte(`{"context":{"action":"select"},"message":{}}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.calls != 0 {
		t.Fatalf("lister called %d times, want 0", l.calls)
	}
}
