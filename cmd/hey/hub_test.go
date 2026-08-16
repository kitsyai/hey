package main

import (
	"strings"
	"testing"
)

func TestLooksLikeTestBinary(t *testing.T) {
	cases := map[string]bool{
		`C:\Users\x\AppData\Local\Temp\go-build12345\b001\hey.test.exe`: true,
		`/tmp/go-build98765/b001/hey.test`:                              true,
		`/home/x/pkg.test`:                                              true,
		`C:\Users\x\pkg.test.exe`:                                       true,
		`C:\Program Files\hey\hey.exe`:                                  false,
		`/usr/local/bin/hey`:                                            false,
	}
	for path, want := range cases {
		if got := looksLikeTestBinary(path); got != want {
			t.Errorf("looksLikeTestBinary(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestStartHubDetached_RefusesInsideTestBinary is a direct regression test
// for a real incident hit while building this feature: `startHubDetached`
// exec'd os.Executable() (the compiled *test* binary under `go test`) with
// "hub __serve-detached" — Go's flag package silently ignores unknown
// positional args, so the child just re-ran this entire test binary's whole
// suite, which (at the time) reached the same code path again, forking
// again, and again, spawning dozens of orphaned processes before the
// runaway was caught and killed by hand. This test proves the guard added
// afterward (looksLikeTestBinary, checked before every exec.Command in
// startHubDetached) fires under the exact conditions that caused it —
// running inside `go test`, calling the real (non-test-seeded) path — rather
// than only being exercised by code review.
func TestStartHubDetached_RefusesInsideTestBinary(t *testing.T) {
	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)
	err := startHubDetached()
	if err == nil {
		t.Fatal("expected startHubDetached to refuse to spawn from inside a test binary")
	}
	if got := err.Error(); !strings.Contains(got, "test binary") {
		t.Errorf("error should explain the refusal, got: %v", err)
	}
}
