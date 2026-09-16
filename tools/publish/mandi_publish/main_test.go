package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runMainInSubprocess re-execs the test binary with MANDI_PUBLISH_TEST_MAIN
// set, so the guarded branch below calls the real main() with the given
// CLI args and env, in a process of its own. main() parses flags into the
// global flag.CommandLine and may call os.Exit, neither of which a second
// call within the same test binary could survive.
func runMainInSubprocess(t *testing.T, extraEnv ...string) (stdout string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(append(os.Environ(), "MANDI_PUBLISH_TEST_MAIN=1"), extraEnv...)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("subprocess did not run: %v\n%s", err, out)
	return "", -1
}

// TestMainCollectsBuildsAndDryRunPublishes drives main() end to end: parse
// flags, run() against a fake upstream, print both summaries, and take the
// bottom --publish branch in dry-run.
func TestMainCollectsBuildsAndDryRunPublishes(t *testing.T) {
	if os.Getenv("MANDI_PUBLISH_TEST_MAIN") == "1" {
		upstream := fakeUpstream(t, "")
		defer upstream.Close()

		catalogOut := filepath.Join(t.TempDir(), "catalog")
		os.Args = append([]string{"mandi_publish",
			"--base-url", upstream.URL,
			"--from", "01-07-2026", "--to", "01-12-2026",
			"--catalog-out", catalogOut,
			"--publish", "--dry-run", "--publish-url", "http://example.invalid",
		})
		main()
		return
	}

	out, code := runMainInSubprocess(t,
		"MANDI_TOKEN_USER=user", "MANDI_TOKEN_SECRET=secret")
	if code != 0 {
		t.Fatalf("main exited %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"collected", "built", "dry-run: would publish to", "would POST"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// TestMainRefusesCatalogInWithoutPublishOrRetire covers the --catalog-in
// guard: naming a directory to re-post without --publish or --retire-old
// makes no sense, and main must say so and exit non-zero before doing
// anything else.
func TestMainRefusesCatalogInWithoutPublishOrRetire(t *testing.T) {
	if os.Getenv("MANDI_PUBLISH_TEST_MAIN") == "1" {
		os.Args = []string{"mandi_publish", "--catalog-in", t.TempDir()}
		main()
		return
	}

	out, code := runMainInSubprocess(t)
	if code != 1 {
		t.Fatalf("main exited %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "only makes sense with --publish or --retire-old") {
		t.Errorf("output does not explain the refusal:\n%s", out)
	}
}

// TestMainPublishesFromDiskInDryRun exercises the --catalog-in shortcut that
// republishes catalogs already on disk without collecting anything.
func TestMainPublishesFromDiskInDryRun(t *testing.T) {
	if os.Getenv("MANDI_PUBLISH_TEST_MAIN") == "1" {
		dir := t.TempDir()
		body := `{"context":{"action":"catalog/publish"},"message":{"catalogs":[` +
			`{"id":"catalog:mandi-price:MH","isActive":true,"resources":[{"id":"resource:mandi-price:market:1"}]}],` +
			`"publishDirectives":[{"catalogId":"catalog:mandi-price:MH"}]}}`
		if err := os.WriteFile(filepath.Join(dir, "mandi-MH.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write catalog fixture: %v", err)
		}

		os.Args = []string{"mandi_publish", "--catalog-in", dir, "--publish", "--dry-run",
			"--publish-url", "http://example.invalid"}
		main()
		return
	}

	out, code := runMainInSubprocess(t)
	if code != 0 {
		t.Fatalf("main exited %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "would POST") {
		t.Errorf("output does not report the dry-run outcome:\n%s", out)
	}
}
