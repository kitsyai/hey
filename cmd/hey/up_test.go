package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kitsyai/hey/internal/fetch"
	"github.com/kitsyai/hey/internal/svc"
)

// buildProbeArchive is up_test.go's own copy of internal/svc/driver_test.go's
// helper of the same name (unexported there, so not reachable from this
// package): builds the shared internal/svcprobe test binary, zips it, and
// returns the archive path plus its real SHA-256, so a synthetic pack pins a
// genuine checksum and the whole `hey up`/`hey down` CLI lifecycle runs
// end-to-end with zero network.
func buildProbeArchive(t *testing.T) (archivePath, sha string) {
	t.Helper()
	tmp := t.TempDir()
	exeName := "svcprobe"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	binPath := filepath.Join(tmp, exeName)
	build := exec.Command("go", "build", "-o", binPath, "../../internal/svcprobe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build svcprobe: %v: %s", err, out)
	}
	binBytes, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}

	archivePath = filepath.Join(tmp, "probe.zip")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	hdr := &zip.FileHeader{Name: exeName, Method: zip.Deflate}
	hdr.SetMode(0o755)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return archivePath, hex.EncodeToString(sum[:])
}

// servePacksHTTPS spins up a real httptest TLS server serving archivePath's
// bytes at /probe.zip, points internal/fetch.Client at it (trusting its
// self-signed cert, the same pattern internal/fetch/fetch_test.go uses), and
// returns the pack's now-fetchable artifact URL. The caller must restore
// fetch.Client (returned as a cleanup func).
func servePacksHTTPS(t *testing.T, archivePath string) (url string, cleanup func()) {
	t.Helper()
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	oldClient := fetch.Client
	fetch.Client = srv.Client()
	return srv.URL + "/probe.zip", func() {
		srv.Close()
		fetch.Client = oldClient
	}
}

// writeFakePacks writes an HEY_PACKS-overridable packs.json defining one
// driver-agnostic svcprobe-backed pack, hub-eligible (Protocol set) so it
// exercises the same code path `hey up postgres`/`hey up vinci` do.
func writeFakePacks(t *testing.T, heyHome, artifactURL, sha string) {
	t.Helper()
	plat := runtime.GOOS + "_" + runtime.GOARCH
	body := `{
	  "hey_packs": 0,
	  "packs": {
	    "fakesvc": {
	      "pack": "fakesvc", "driver": "archive-exec", "kind": "service",
	      "versions": { "1.0.0": { "artifacts": { "` + plat + `": { "url": "` + artifactURL + `", "sha256": "` + sha + `" } } } },
	      "start": "{bin}/svcprobe --port {port}",
	      "ready": { "tcp": "127.0.0.1:{port}", "timeout_seconds": 10 },
	      "stop": { "signal": "term", "grace_seconds": 5 },
	      "protocol": "http"
	    }
	  }
	}`
	if err := os.WriteFile(filepath.Join(heyHome, "packs.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startHubInProcess brings the hub up for a test WITHOUT going through `hey
// hub start`'s detached-subprocess spawn: os.Executable() inside `go test`
// resolves to the compiled *test* binary, not a real `hey` executable, so
// exec'ing it with "hub __serve-detached" args would just re-run go test's
// own main. Serving hubServe(true) in a background goroutine (still a real
// listener on the real hub port, still writes the real pidfile) satisfies
// `ensureHubUp`'s liveness probe exactly the same way a real detached
// process would, so `hey up`'s own auto-start logic under test never takes
// the (untestable-this-way) subprocess path at all.
func startHubInProcess(t *testing.T) {
	t.Helper()
	_, _, port, err := hubHandler()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = hubServe(true) }()
	deadline := 50
	for i := 0; i < deadline && !EnsureHubRunning(port); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !EnsureHubRunning(port) {
		t.Fatal("in-process hub did not come up")
	}
}

func TestUpDown_FakePackLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("process-spawning lifecycle test; run without -short")
	}
	archivePath, sha := buildProbeArchive(t)
	artifactURL, cleanup := servePacksHTTPS(t, archivePath)
	defer cleanup()

	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)
	writeFakePacks(t, heyHome, artifactURL, sha)
	startHubInProcess(t)

	if err := cmdUp([]string{"fakesvc"}); err != nil {
		t.Fatalf("hey up fakesvc: %v", err)
	}

	svcDir := filepath.Join(heyHome, "svc")
	inst, err := svc.LoadInstance(filepath.Join(svcDir, "fakesvc"))
	if err != nil {
		t.Fatalf("load instance: %v", err)
	}
	t.Cleanup(func() {
		packs, _ := svc.LoadPacks(heyHome)
		if p, e := packs.Get("fakesvc"); e == nil {
			_ = svc.Stop(inst, p)
		}
	})
	if !svc.Alive(inst) {
		t.Fatal("fakesvc should be running after hey up")
	}

	// The hub picked it up without any explicit registration call — polled
	// straight from internal/svc's on-disk instance (docs/design/
	// services.md §4: "the hub is a view, not a second source of truth").
	_, _, hubPort, err := hubHandler()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(hubPort)+"/services", nil)
	req.Header.Set("X-Hey-Client", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /services: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Services []struct {
			Name   string `json:"name"`
			Health string `json:"health"`
		} `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range body.Services {
		if s.Name == "fakesvc" {
			found = true
			if s.Health != "healthy" {
				t.Errorf("fakesvc health = %q, want healthy", s.Health)
			}
		}
	}
	if !found {
		t.Errorf("hub /services did not list fakesvc: %+v", body.Services)
	}

	// Idempotent: a second `hey up` reuses the running process.
	if err := cmdUp([]string{"fakesvc"}); err != nil {
		t.Fatalf("second hey up: %v", err)
	}
	reloaded, err := svc.LoadInstance(filepath.Join(svcDir, "fakesvc"))
	if err != nil || reloaded.PID != inst.PID {
		t.Errorf("second hey up should reuse the same process: %+v (want pid %d)", reloaded, inst.PID)
	}

	if err := cmdDown([]string{"fakesvc"}); err != nil {
		t.Fatalf("hey down fakesvc: %v", err)
	}
	stopped, err := svc.LoadInstance(filepath.Join(svcDir, "fakesvc"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Alive(stopped) {
		t.Error("fakesvc should be stopped after hey down")
	}
}

func TestUp_NamespaceCollisionRefused(t *testing.T) {
	archivePath, sha := buildProbeArchive(t)
	artifactURL, cleanup := servePacksHTTPS(t, archivePath)
	defer cleanup()

	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)
	// "ps" is a reserved hey command (internal/registry.Reserved) — a pack
	// named "ps" must be refused before any provisioning is attempted.
	plat := runtime.GOOS + "_" + runtime.GOARCH
	body := `{
	  "hey_packs": 0,
	  "packs": {
	    "ps": {
	      "pack": "ps", "driver": "archive-exec",
	      "versions": { "1.0.0": { "artifacts": { "` + plat + `": { "url": "` + artifactURL + `", "sha256": "` + sha + `" } } } },
	      "start": "{bin}/svcprobe --port {port}",
	      "ready": { "tcp": "127.0.0.1:{port}" },
	      "protocol": "http"
	    }
	  }
	}`
	if err := os.WriteFile(filepath.Join(heyHome, "packs.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cmdUp([]string{"ps"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected a namespace-collision refusal, got %v", err)
	}
}

func TestUp_CrossPackNameRefused(t *testing.T) {
	archivePath, sha := buildProbeArchive(t)
	artifactURL, cleanup := servePacksHTTPS(t, archivePath)
	defer cleanup()

	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)
	plat := runtime.GOOS + "_" + runtime.GOARCH
	body := `{
	  "hey_packs": 0,
	  "packs": {
	    "fakesvc": {
	      "pack": "fakesvc", "driver": "archive-exec",
	      "versions": { "1.0.0": { "artifacts": { "` + plat + `": { "url": "` + artifactURL + `", "sha256": "` + sha + `" } } } },
	      "start": "{bin}/svcprobe --port {port}", "ready": { "tcp": "127.0.0.1:{port}" }, "protocol": "http"
	    },
	    "othersvc": {
	      "pack": "othersvc", "driver": "archive-exec",
	      "versions": { "1.0.0": { "artifacts": { "` + plat + `": { "url": "` + artifactURL + `", "sha256": "` + sha + `" } } } },
	      "start": "{bin}/svcprobe --port {port}", "ready": { "tcp": "127.0.0.1:{port}" }, "protocol": "http"
	    }
	  }
	}`
	if err := os.WriteFile(filepath.Join(heyHome, "packs.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	err := cmdUp([]string{"fakesvc", "--name", "othersvc"})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("expected a cross-pack --name collision refusal, got %v", err)
	}
}
