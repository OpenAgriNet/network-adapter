package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstNonEmptyReturnsTheFirstSetValue(t *testing.T) {
	if got := firstNonEmpty("", "", "b", "c"); got != "b" {
		t.Errorf("firstNonEmpty = %q, want %q", got, "b")
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("a flag must beat a fallback: got %q, want %q", got, "a")
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty of nothing set = %q, want empty", got)
	}
}

func TestSplitStatesDropsBlanksAndTrims(t *testing.T) {
	got := splitStates(" MH, TN ,,KA,")
	want := []string{"MH", "TN", "KA"}
	if len(got) != len(want) {
		t.Fatalf("splitStates = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("splitStates[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestSplitStatesOfBlankIsNil(t *testing.T) {
	if got := splitStates("   "); got != nil {
		t.Errorf("splitStates(blank) = %v, want nil", got)
	}
}

func TestLoadEnvFileSetsOnlyMissingVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# a comment\n\nMANDI_TEST_NEW=\"new-value\"\nMANDI_TEST_EXISTING=ignored\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	os.Unsetenv("MANDI_TEST_NEW")
	t.Setenv("MANDI_TEST_EXISTING", "already-set")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer os.Chdir(wd)

	loadEnvFile(".env")

	if got := os.Getenv("MANDI_TEST_NEW"); got != "new-value" {
		t.Errorf("MANDI_TEST_NEW = %q, want %q", got, "new-value")
	}
	if got := os.Getenv("MANDI_TEST_EXISTING"); got != "already-set" {
		t.Errorf("loadEnvFile overwrote an already-set var: got %q, want %q", got, "already-set")
	}
}

func TestLoadEnvFileOfAMissingPathIsANoop(t *testing.T) {
	// Must not panic or exit; a run without a .env file is the common case.
	loadEnvFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
}

// TestFail runs in the same binary under a marker env var, so `go test` can
// assert on the exit code and stderr of a call that would otherwise end the
// test process itself.
func TestFail(t *testing.T) {
	if os.Getenv("MANDI_PUBLISH_TEST_FAIL") == "1" {
		fail("boom %d", 42)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestFail$")
	cmd.Env = append(os.Environ(), "MANDI_PUBLISH_TEST_FAIL=1")
	out, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("fail() exited %v, want exit code 1: %s", err, out)
	}
	if !strings.Contains(string(out), "mandi_publish: boom 42") {
		t.Errorf("fail() output = %q, want it to contain %q", out, "mandi_publish: boom 42")
	}
}
