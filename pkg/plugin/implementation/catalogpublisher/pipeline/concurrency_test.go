package pipeline

// concurrency_test.go pins the parts of the engine that two pipelines reach
// at the same time. The crawler runs one runner per binding key, each on its
// own goroutine, so this is the ordinary case rather than an edge one.
//
// Run with -race or these prove nothing.

import (
	"sync"
	"testing"
)

// An unguarded cache write here is not a subtle race: Go kills the PROCESS
// with "fatal error: concurrent map writes", taking the crawl loops down with
// the publish ones.
func TestRaceValidateIsConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Validate(fixtureFS, "testdata/minimal.yaml")
		}()
	}
	wg.Wait()
}
