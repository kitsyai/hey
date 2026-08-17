package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DaemonChildEnv is the environment marker every hey-spawned background child
// carries (value: a short kind tag like "hub", "registry", "svc:postgres").
// It is the spawn-loop circuit breaker: any code path that would re-exec the
// hey binary as a detached child MUST call GuardReexec first, which refuses
// when the marker is already present — so even if a background child somehow
// reaches a spawn path again (a bug, a misconfigured pack whose start command
// is hey itself, a test binary re-running its own suite), the recursion is
// stopped at depth one with a clear error instead of forking until the
// machine falls over. Born of a real incident: a runaway recursive spawn out
// of `hey hub start` under `go test` (see cmd/hey/hub_test.go), and the same
// latent shape in `hey registry start`.
const DaemonChildEnv = "HEY_DAEMON_CHILD"

// InDaemonChild reports whether this process is a hey-spawned background
// child, and which kind spawned it.
func InDaemonChild() (kind string, ok bool) {
	v := os.Getenv(DaemonChildEnv)
	return v, v != ""
}

// GuardReexec must be called immediately before any exec of the hey binary
// itself (exe) as a detached background child. It refuses when:
//
//   - DaemonChildEnv is already set: this process IS a background child, and
//     a child spawning another copy of hey is exactly how a spawn loop
//     starts; or
//   - exe looks like a `go test` binary (LooksLikeTestBinary): exec'ing a
//     test binary with subcommand args just re-runs the whole test suite,
//     which historically recursed.
//
// kind names what was about to be spawned ("hub", "registry", ...) so the
// error says which path tripped.
func GuardReexec(exe, kind string) error {
	if parent, ok := InDaemonChild(); ok {
		return fmt.Errorf(
			"refusing to spawn a detached %s: this process is already a hey background child (%s=%s) — "+
				"a background child re-execing hey is a spawn loop; run this from a normal interactive `hey` instead",
			kind, DaemonChildEnv, parent)
	}
	if LooksLikeTestBinary(exe) {
		return fmt.Errorf("refusing to spawn a detached %s from what looks like a test binary (%s) — "+
			"tests must pre-start the %s in-process instead of exercising this path", kind, exe, kind)
	}
	return nil
}

// LooksLikeTestBinary heuristically detects a `go test`-compiled executable
// by its path/name shape: `go test` builds into a `go-buildNNNN` temp
// directory, and/or names the binary `<pkg>.test`/`<pkg>.test.exe`. Neither
// signal is foolproof (a renamed real binary could false-positive; unlikely
// in practice), but the cost of a false positive is a clear error telling a
// test to fix itself, versus the cost of a false negative being exactly the
// runaway recursive spawn GuardReexec exists to prevent.
func LooksLikeTestBinary(exe string) bool {
	base := filepath.Base(exe)
	if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return true
	}
	return strings.Contains(exe, "go-build")
}

// MarkDaemonChild stamps cmd's environment with DaemonChildEnv=kind (on top
// of this process's environment, replacing any prior marker) so the child —
// and everything the child spawns — is recognizable as a hey background
// child and GuardReexec can stop it from ever re-execing hey.
func MarkDaemonChild(cmd *exec.Cmd, kind string) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, DaemonChildEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	cmd.Env = append(out, DaemonChildEnv+"="+kind)
}
