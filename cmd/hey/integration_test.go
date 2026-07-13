package main

// Integration tests hit the real network / spawn real processes.
// Run with: HEY_INTEGRATION=1 go test ./cmd/hey -run Integration -v

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kitsyai/hey/internal/contract"
	"github.com/kitsyai/hey/internal/proc"
	"github.com/kitsyai/hey/internal/state"
)

func requireIntegration(t *testing.T) string {
	t.Helper()
	if os.Getenv("HEY_INTEGRATION") != "1" {
		t.Skip("set HEY_INTEGRATION=1 to run integration tests")
	}
	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)
	return heyHome
}

// TestIntegrationInstallRealGuten proves the whole resolve→fetch→verify→
// extract pipeline against the real kitsyai/guten GitHub release.
func TestIntegrationInstallRealGuten(t *testing.T) {
	heyHome := requireIntegration(t)

	if err := cmdInstall([]string{"guten"}); err != nil {
		t.Fatalf("install guten: %v", err)
	}

	// which resolves via the current file.
	if err := cmdWhich([]string{"guten"}); err != nil {
		t.Fatalf("which guten: %v", err)
	}

	// The binary actually runs.
	ver, ok := currentVersion("guten")
	if !ok {
		t.Fatal("no current version recorded")
	}
	binName := "guten"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(heyHome, "bin", "guten", ver, binName)
	out, err := exec.Command(binPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("run guten version: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "guten ") {
		t.Errorf("unexpected guten version output: %q", out)
	}

	// Second install: resolve cache + cached binary → zero API calls.
	resolvePath := filepath.Join(heyHome, "state", "resolve.json")
	before, err := os.Stat(resolvePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdInstall([]string{"guten"}); err != nil {
		t.Fatalf("second install: %v", err)
	}
	after, err := os.Stat(resolvePath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("second install rewrote the resolve cache — it should not have hit the API")
	}

	// cache clean removes it.
	if err := cmdCache([]string{"clean", "guten"}); err != nil {
		t.Fatalf("cache clean: %v", err)
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Error("binary should be gone after cache clean")
	}
}

// TestIntegrationUIContractE2E runs the full UI lifecycle against the
// testapp reference implementation: spawn detached, handshake via log tail,
// state tracking, ps liveness, graceful stop with tree-kill fallback.
func TestIntegrationUIContractE2E(t *testing.T) {
	heyHome := requireIntegration(t)

	// Build testapp and pre-seed it into the cache (no fetch path involved).
	binName := "testapp"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	verDir := filepath.Join(heyHome, "bin", "testapp", "0.0.1")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(verDir, binName), "../../internal/testapp")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build testapp: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(heyHome, "bin", "testapp", "current"), []byte("0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Registry listing testapp with "ui" as a UI command. The github-release
	// source is never touched because the binary is already cached.
	reg := map[string]any{
		"hey_registry": 0,
		"apps": map[string]any{
			"testapp": map[string]any{
				"source": map[string]any{
					"type": "github-release", "repo": "heypkv/testapp", "tag_prefix": "",
					"asset_template": "testapp_{version}_{os}_{arch}.{ext}",
					"checksums_asset": "checksums.txt", "binary": "testapp",
				},
				"ui_commands": []string{"ui"},
			},
		},
	}
	regData, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(heyHome, "registry.json"), regData, 0o644); err != nil {
		t.Fatal(err)
	}

	// Start the UI (pinned version → no resolve; --no-browser for CI).
	if err := cmdRun([]string{"--no-browser", "--timeout", "20s", "testapp@0.0.1", "ui"}); err != nil {
		t.Fatalf("run testapp ui: %v", err)
	}

	stateDir := filepath.Join(heyHome, "state")
	entry, ok, err := state.Get(stateDir, "testapp")
	if err != nil || !ok {
		t.Fatalf("state entry missing: ok=%v err=%v", ok, err)
	}
	t.Cleanup(func() { _ = proc.KillTree(entry.PID) }) // safety net

	if !proc.Alive(entry.PID) {
		t.Fatal("testapp should be alive")
	}
	if !contract.Healthy(entry.URL) {
		t.Fatal("testapp should answer /healthz")
	}

	// Second run reuses the existing instance instead of double-starting.
	if err := cmdRun([]string{"--no-browser", "testapp@0.0.1", "ui"}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	entry2, _, _ := state.Get(stateDir, "testapp")
	if entry2.PID != entry.PID {
		t.Errorf("second run started a new process: pid %d vs %d", entry2.PID, entry.PID)
	}

	// ps sees it (prune keeps it).
	if err := cmdPs(nil); err != nil {
		t.Fatalf("ps: %v", err)
	}

	// stop: graceful shutdown path.
	if err := cmdStop([]string{"testapp"}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && proc.Alive(entry.PID) {
		time.Sleep(100 * time.Millisecond)
	}
	if proc.Alive(entry.PID) {
		t.Error("testapp still alive after stop")
	}
	if _, ok, _ := state.Get(stateDir, "testapp"); ok {
		t.Error("state entry should be removed after stop")
	}

	// The app's HTTP port must be closed too.
	client := &http.Client{Timeout: 1 * time.Second}
	if resp, err := client.Get(entry.URL + "/healthz"); err == nil {
		resp.Body.Close()
		t.Errorf("port still answering after stop: %s", entry.URL)
	}
	fmt.Println("ui contract e2e ok:", entry.URL)
}
