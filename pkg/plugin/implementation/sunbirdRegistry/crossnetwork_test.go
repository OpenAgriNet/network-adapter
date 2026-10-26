package sunbirdRegistry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

const testAgreedPack = "https://x/MandiPrice/v0.1/attributes.yaml"
const testAgreedContext = "https://x/MandiPrice/v0.1/context.jsonld"

// admittedRecord is a network we admitted, in the given directions. A pack list
// is required on a real record, so each direction carries one.
func admittedRecord(agreed federationRole) participant {
	record := activeRecord()
	record.Role = roleList{roleNetwork}
	record.FederationRole = agreed
	return record
}

func agreedPacks() *federationDirection {
	return &federationDirection{SchemaPacks: []string{testAgreedPack}}
}

// admittingClient answers every search with one network admitted in the given
// directions.
func admittingClient(t *testing.T, role federationRole) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, admittedRecord(role)))
	}))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL, nil)
}

// discoverFor asks as a discover naming the given capabilities.
func discoverFor(t *testing.T, c *Client, contexts ...string) error {
	t.Helper()
	return c.CheckCrossNetworkRequester(context.Background(), testParticipantID, discoverAction, contexts)
}

func TestACallerWeAgreedToGiveToIsAllowed(t *testing.T) {
	t.Parallel()

	client := admittingClient(t, federationRole{Requester: agreedPacks()})
	if err := discoverFor(t, client, testAgreedContext); err != nil {
		t.Fatalf("a requester asking within the agreement must be allowed: %v", err)
	}
}

func TestACallerWeOnlyAgreedToReadFromIsRefused(t *testing.T) {
	t.Parallel()

	client := admittingClient(t, federationRole{Source: agreedPacks()})
	err := discoverFor(t, client, testAgreedContext)
	if err == nil {
		t.Fatal("a network we only read from must not be able to read from us")
	}
	if !strings.Contains(err.Error(), "not a network we agreed to give to") {
		t.Errorf("the refusal must name the direction: %v", err)
	}
}

func TestARecordWithNoAgreementIsLeftAlone(t *testing.T) {
	t.Parallel()

	// An ordinary participant: a provider, or our own deployment. It carries no
	// federationRole, so no federation rule applies to it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, activeRecord()))
	}))
	t.Cleanup(srv.Close)

	if err := discoverFor(t, newTestClient(t, srv.URL, nil), testAgreedContext); err != nil {
		t.Fatalf("a participant we never admitted must be left alone: %v", err)
	}
}

func TestACallerWeNeverAdmittedIsLeftAlone(t *testing.T) {
	t.Parallel()

	// The registry answered, and the answer is that we hold no record for them.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t))
	}))
	t.Cleanup(srv.Close)

	if err := discoverFor(t, newTestClient(t, srv.URL, nil), testAgreedContext); err != nil {
		t.Fatalf("an unknown caller is judged by signature validation, not here: %v", err)
	}
}

func TestADiscoverNamingNoCapabilityIsRefused(t *testing.T) {
	t.Parallel()

	client := admittingClient(t, federationRole{Requester: agreedPacks()})
	err := discoverFor(t, client)
	if err == nil {
		t.Fatal("naming no capability asks for everything, which no agreement covers")
	}
	if !strings.Contains(err.Error(), "named no capability") {
		t.Errorf("the refusal must say what was missing: %v", err)
	}
}

func TestADiscoverAskingOutsideTheAgreementIsRefused(t *testing.T) {
	t.Parallel()

	client := admittingClient(t, federationRole{Requester: agreedPacks()})
	err := discoverFor(t, client, "https://x/WeatherObservation/v0.1/context.jsonld")
	if err == nil {
		t.Fatal("a capability outside the agreement must be refused")
	}
	if !strings.Contains(err.Error(), "did not agree to give") {
		t.Errorf("the refusal must not name what we hold: %v", err)
	}
}

func TestOnlyADiscoverIsCheckedAgainstTheAgreedCapabilities(t *testing.T) {
	t.Parallel()

	// A select carries no schemaContext, so the direction is the whole check.
	client := admittingClient(t, federationRole{Requester: agreedPacks()})
	if err := client.CheckCrossNetworkRequester(context.Background(), testParticipantID, "select", nil); err != nil {
		t.Fatalf("a select names no capability, so it must pass on direction alone: %v", err)
	}
}

func TestASelectFromANetworkWeOnlyReadFromIsStillRefused(t *testing.T) {
	t.Parallel()

	client := admittingClient(t, federationRole{Source: agreedPacks()})
	if err := client.CheckCrossNetworkRequester(context.Background(), testParticipantID, "select", nil); err == nil {
		t.Fatal("the direction is checked for every action, not only discover")
	}
}

func TestACallerNamingNobodyIsLeftAlone(t *testing.T) {
	t.Parallel()

	client := admittingClient(t, federationRole{Requester: agreedPacks()})
	if err := client.CheckCrossNetworkRequester(context.Background(), "", discoverAction, nil); err != nil {
		t.Fatalf("an empty caller is refused by the step, not judged here: %v", err)
	}
}

func TestARegistryThatCannotBeAskedRefuses(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	err := discoverFor(t, newTestClient(t, srv.URL, nil), testAgreedContext)
	if err == nil {
		t.Fatal("a registry we could not ask must refuse rather than allow")
	}
}

func TestARefusalIsTheCallersFaultNotOurs(t *testing.T) {
	t.Parallel()

	// A refused caller is known and well formed -- they asked for something we
	// did not agree to give. Unclassified it would surface as a 500, which
	// blames this adapter and tells them nothing they can act on.
	client := admittingClient(t, federationRole{Source: agreedPacks()})

	var coded *model.CodedErr
	if err := discoverFor(t, client, testAgreedContext); !errors.As(err, &coded) {
		t.Fatalf("a refusal must carry a status, got %T: %v", err, err)
	}
	if coded.HTTPStatus() != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", coded.HTTPStatus())
	}
}

// A half-filled admission record is still an admission. The usability tests the
// crawler applies -- a network role, a discover address -- say whether we can
// CALL a network, and have no bearing on whether we agreed to answer it.
func TestARecordMissingTheNetworkRoleIsStillJudged(t *testing.T) {
	t.Parallel()

	record := admittedRecord(federationRole{Source: agreedPacks()})
	record.Role = roleList{"provider"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, record))
	}))
	t.Cleanup(srv.Close)

	err := discoverFor(t, newTestClient(t, srv.URL, nil), testAgreedContext)
	if err == nil {
		t.Fatal("a network we only read from must be refused whatever else its record says")
	}
	if !strings.Contains(err.Error(), "not a network we agreed to give to") {
		t.Errorf("the refusal must name the direction: %v", err)
	}
}

func TestARecordWithNoDiscoverAddressIsStillJudged(t *testing.T) {
	t.Parallel()

	record := admittedRecord(federationRole{Source: agreedPacks()})
	record.BaseURL = ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, recordJSON(t, record))
	}))
	t.Cleanup(srv.Close)

	if err := discoverFor(t, newTestClient(t, srv.URL, nil), testAgreedContext); err == nil {
		t.Fatal("an address we cannot call them at does not make them one we may answer")
	}
}
