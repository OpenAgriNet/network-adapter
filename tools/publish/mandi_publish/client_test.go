package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenReturnsTheMintedToken(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/generate-dynamic-token-agmarknet" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"token":"45c313a8-5178-434b-a4d7-dfb78f47ac35"}`))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	token, err := client.Token(context.Background(), "the-user", "the-secret")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if want := "45c313a8-5178-434b-a4d7-dfb78f47ac35"; token != want {
		t.Errorf("token = %q, want %q", token, want)
	}
	// The upstream's own spelling, which is one provider's choice rather than a
	// standard -- so it is asserted rather than assumed.
	if gotBody["access_name"] != "the-user" || gotBody["password"] != "the-secret" {
		t.Errorf("body = %v, want access_name/password", gotBody)
	}
}

func TestTokenErrorNamesTheSchemeNotTheCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"invalid credentials for user hunter2"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := client.Token(context.Background(), "the-user", "s3cr3t")
	if err == nil {
		t.Fatal("want an error for 401")
	}
	// The upstream quotes the request back on a rejection, so the body must not
	// reach the message.
	if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks a credential: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should carry the status, got %v", err)
	}
}

func TestTokenRejectsAResponseWithNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := client.Token(context.Background(), "u", "p"); err == nil {
		t.Fatal("want an error when the response carries no token")
	}
}

func TestAsQueryRendersScalarsAndRefusesTheRest(t *testing.T) {
	got, err := asQuery([]byte(`{"option":"4","token":"t-123"}`))
	if err != nil {
		t.Fatalf("asQuery: %v", err)
	}
	// Encoded, so key order is deterministic regardless of map iteration.
	if want := "option=4&token=t-123"; got != want {
		t.Errorf("asQuery = %q, want %q", got, want)
	}

	if _, err := asQuery([]byte(`{"bad":{"nested":1}}`)); err == nil {
		t.Error("want an error for a non-scalar field")
	} else if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error should name the offending field, got %v", err)
	}
}

func TestStatesFetchesAndMapsTheStateList(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"state_id":1,"state_name":"Andaman and Nicobar","agm_state_code":"AN"},
			{"state_id":20,"state_name":"Maharashtra","agm_state_code":"MH"}
		]`))
	}))
	defer upstream.Close()

	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	states, err := client.States(ctx, mapper, base, "t-123")
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d states, want 2", len(states))
	}
	if states[1].Code != "MH" || states[1].Name != "Maharashtra" {
		t.Errorf("states[1] = %+v", states[1])
	}
	if !strings.Contains(gotQuery, "option=4") || !strings.Contains(gotQuery, "token=t-123") {
		t.Errorf("query = %q, want option and token", gotQuery)
	}
}

func TestCallRefusesAnObjectResponse(t *testing.T) {
	// An upstream error such as {"message":"no data found"} answered with a
	// 200 must be refused, not mapped: JSONata's $map over an object yields
	// one element whose every field is undefined, which Go decodes as a
	// zero-valued struct -- a phantom market with marketId 0 that would
	// otherwise reach the output.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"no data found"}`))
	}))
	defer upstream.Close()

	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	states, err := client.States(ctx, mapper, base, "t-123")
	if err == nil {
		t.Fatalf("want an error for an object response, got %+v (a phantom market)", states)
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("error should name the shape received, got %v", err)
	}
}

func TestStatesSurvivesASingleRowAnswer(t *testing.T) {
	// The regression the [...] wrap in the mapping exists to prevent: JSONata
	// collapses a one-element sequence, and a decode into a slice then fails on
	// exactly the input nobody tests with.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"state_id":20,"state_name":"Maharashtra","agm_state_code":"MH"}]`))
	}))
	defer upstream.Close()

	base, stop, err := serveMappings()
	if err != nil {
		t.Fatalf("serveMappings: %v", err)
	}
	defer stop()

	ctx := context.Background()
	mapper, closer, err := newMapper(ctx)
	if err != nil {
		t.Fatalf("newMapper: %v", err)
	}
	defer func() { _ = closer() }()

	client := &Client{BaseURL: upstream.URL, HTTP: upstream.Client()}
	states, err := client.States(ctx, mapper, base, "t-123")
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Code != "MH" {
		t.Fatalf("got %+v, want one MH state", states)
	}
}
