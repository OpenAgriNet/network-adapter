// Command mandi_publish collects Agmarknet Vistaar's market master data.
//
// It authenticates, reads the state list, reads every market's coordinates once
// for all of India, then walks the states fetching what each market trades, and
// writes one normalized document.
//
// Publishing is not this command's job yet: the document it writes is what a
// later stage turns into a MandiPrice catalog. Keeping the two apart means the
// collected data can be reviewed by a human before anything reaches a network.
//
// Usage:
//
//	MANDI_TOKEN_USER=... MANDI_TOKEN_SECRET=... \
//	  mandi_publish --states MH --from 01-07-2026 --to 01-12-2026 --out markets.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// config is one run's inputs, gathered so collect can be tested without flags.
type config struct {
	baseURL  string
	user     string
	secret   string
	states   []string // empty means every state master data reports
	fromDate string
	toDate   string
}

func main() {
	baseURL := flag.String("base-url", "http://34.0.4.235:8080", "Agmarknet Vistaar base URL")
	states := flag.String("states", "", "comma-separated state codes; empty means every state")
	fromDate := flag.String("from", "", "window start, dd-MM-yyyy (default: today)")
	toDate := flag.String("to", "", "window end, dd-MM-yyyy (default: today)")
	out := flag.String("out", "markets.json", "where to write the collected document")
	flag.Parse()

	today := time.Now().Format("02-01-2006")
	if *fromDate == "" {
		*fromDate = today
	}
	if *toDate == "" {
		*toDate = today
	}

	cfg := config{
		baseURL: *baseURL,
		// CREDENTIALS FROM THE ENVIRONMENT, NEVER A FLAG. A flag value lands in
		// shell history and is visible in ps output to every user on the host.
		user:     os.Getenv("MANDI_TOKEN_USER"),
		secret:   os.Getenv("MANDI_TOKEN_SECRET"),
		states:   splitStates(*states),
		fromDate: *fromDate,
		toDate:   *toDate,
	}

	collection, err := collect(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mandi_publish: %v\n", err)
		os.Exit(1)
	}

	body, err := json.MarshalIndent(collection, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mandi_publish: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "mandi_publish: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "wrote %d markets to %s\n", len(collection.Markets), *out)
	for _, quality := range []string{coordinateMissing, coordinateSuspect, coordinateOutOfBounds} {
		if n := countQuality(collection.Markets, quality); n > 0 {
			fmt.Fprintf(os.Stderr, "  %d markets have %s coordinates\n", n, quality)
		}
	}
	if len(collection.EmptyStates) > 0 {
		// Not fatal -- see the EmptyStates doc comment -- but worth a human's
		// attention, since it can also be the first sign of an upstream problem.
		fmt.Fprintf(os.Stderr, "  %d states returned zero markets: %s\n",
			len(collection.EmptyStates), strings.Join(collection.EmptyStates, ", "))
	}
	if len(collection.StateErrors) > 0 {
		// Non-zero on a partial run: a caller scripting this must be able to
		// tell a complete collection from one missing a state.
		for _, failure := range collection.StateErrors {
			fmt.Fprintf(os.Stderr, "  %s failed: %s\n", failure.StateCode, failure.Reason)
		}
		os.Exit(1)
	}
}

// splitStates reads the comma-separated flag, dropping blanks so a trailing
// comma is not a state code.
func splitStates(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// countQuality counts markets carrying one verdict, for the run summary.
func countQuality(markets []CollectedMarket, quality string) int {
	n := 0
	for _, market := range markets {
		if market.CoordinateQuality == quality {
			n++
		}
	}
	return n
}

// collect runs the whole pipeline.
//
// Sequential deliberately. Thirty-six calls against a service that publishes no
// rate limit is the polite default, and concurrency here would buy seconds while
// risking a throttle that looks like data loss.
func collect(ctx context.Context, cfg config) (Collection, error) {
	if cfg.user == "" || cfg.secret == "" {
		return Collection{}, errors.New(
			"set MANDI_TOKEN_USER and MANDI_TOKEN_SECRET in the environment")
	}

	mappingBase, stop, err := serveMappings()
	if err != nil {
		return Collection{}, err
	}
	defer stop()

	mapper, closer, err := newMapper(ctx)
	if err != nil {
		return Collection{}, err
	}
	defer func() { _ = closer() }()

	client := newClient(cfg.baseURL)

	token, err := client.Token(ctx, cfg.user, cfg.secret)
	if err != nil {
		return Collection{}, err
	}

	stateCodes := cfg.states
	if len(stateCodes) == 0 {
		states, err := client.States(ctx, mapper, mappingBase, token)
		if err != nil {
			return Collection{}, err
		}
		for _, state := range states {
			stateCodes = append(stateCodes, state.Code)
		}
	}

	// FATAL, not a partial run: an empty state list means every market in
	// India would be silently lost, and the output would still be a
	// well-formed, zero-error document that looks complete. cfg.states is
	// never legitimately non-nil-but-empty (splitStates turns a blank flag
	// into nil, which is "every state"), so this only fires when the
	// upstream's own state list resolved to nothing.
	if len(stateCodes) == 0 {
		return Collection{}, errors.New("state list resolved to zero states; refusing to write an empty collection")
	}

	// Once, for all of India: this call has no per-state variant, so fetching
	// it inside the loop would download 600 KB thirty-six times.
	markets, err := client.Markets(ctx, mapper, mappingBase, token)
	if err != nil {
		return Collection{}, err
	}

	collection := Collection{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      Window{From: cfg.fromDate, To: cfg.toDate},
		Markets:     []CollectedMarket{},
		StateErrors: []StateError{},
		EmptyStates: []string{},
	}

	// Tracks marketId across every state processed so far. A market appearing
	// under two state codes (or a state code repeated in --states) must land
	// in the output once: Plan 1 makes one catalog resource per market, and a
	// duplicate here becomes a duplicate resource ID there. First occurrence
	// wins; later ones are skipped.
	seen := make(map[int]bool)

	for _, code := range stateCodes {
		rows, err := client.StateMarkets(ctx, mapper, mappingBase, token, code, cfg.fromDate, cfg.toDate)
		if err != nil {
			// RECORDED, NOT FATAL. One state failing must not discard the
			// thirty-five that succeeded; main exits non-zero so a partial run
			// is still distinguishable from a complete one.
			collection.StateErrors = append(collection.StateErrors,
				StateError{StateCode: code, Reason: err.Error()})
			continue
		}
		if len(rows) == 0 {
			// SUCCEEDED but empty is suspicious, not fatal: a state can
			// genuinely have nothing trading in the window, but this must be
			// visible rather than indistinguishable from a state that was
			// never fetched.
			collection.EmptyStates = append(collection.EmptyStates, code)
		}
		for _, cm := range join(rows, markets) {
			if seen[cm.MarketID] {
				continue
			}
			seen[cm.MarketID] = true
			collection.Markets = append(collection.Markets, cm)
		}
	}
	return collection, nil
}
