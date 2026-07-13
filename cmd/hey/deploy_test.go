package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kitsyai/hey/internal/deploy"
	"github.com/kitsyai/hey/internal/fetch"
)

// buildProbeZip builds the deployprobe helper and zips it at the archive root
// under exeName, returning the zip bytes and their real SHA-256 — so the
// synthetic manifest pins a genuine checksum and the whole flow runs with no
// network beyond loopback httptest.
func buildProbeZip(t *testing.T, exeName string) (zipBytes []byte, sha string) {
	t.Helper()
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, exeName)
	build := exec.Command("go", "build", "-o", binPath, "../../internal/deployprobe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build deployprobe: %v: %s", err, out)
	}
	binBytes, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(tmp, "bundle.zip")
	f, err := os.Create(zipPath)
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
	w.Write(binBytes)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	zipBytes, err = os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(zipBytes)
	return zipBytes, hex.EncodeToString(sum[:])
}

// serveBundle stands up a loopback TLS server that answers the manifest at
// /main.json and the artifact at /bundle.zip, and points hey's manifest+fetch
// clients at it (they would otherwise reject the self-signed cert). markerPath
// is baked into the manifest's launch.args so the launched probe proves it ran.
func serveBundle(t *testing.T, zipBytes []byte, sha, exeName, iface, markerPath string) (*httptest.Server, *int64) {
	t.Helper()
	var bundleHits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/bundle.zip", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&bundleHits, 1)
		w.Write(zipBytes)
	})
	// Serve the manifest on the catch-all so both a direct .json URL and a
	// scope template's {id}/{channel}.json path resolve to it.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		manifest := fmt.Sprintf(`{
  "hey_deploy": 1,
  "id": "probeapp",
  "name": "Probe App",
  "version": "1.2.3",
  "channel": "stable",
  "artifacts": [
    {"platform":%q,"arch":%q,"kind":"archive","format":"zip",
     "url":"https://%s/bundle.zip","sha256":%q,
     "launch":{"exec":%q,"args":[%q]},"interface":%q}
  ]
}`, deploy.CurrentPlatform(), deploy.CurrentArch(), r.Host, sha, exeName, markerPath, iface)
		fmt.Fprint(w, manifest)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	oldF, oldD := fetch.Client, deploy.Client
	fetch.Client = srv.Client()
	deploy.Client = srv.Client()
	t.Cleanup(func() { fetch.Client, deploy.Client = oldF, oldD })
	return srv, &bundleHits
}

func exeName() string {
	if runtime.GOOS == "windows" {
		return "deployprobe.exe"
	}
	return "deployprobe"
}

func waitMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("marker %s never appeared — the bundle was not launched", path)
}

// TestDeployRunWindowE2E drives the default (cached) install+launch of a
// window-interface archive bundle end to end: resolve manifest → download →
// verify sha256 → extract → launch → marker, then ls/which/cache clean.
func TestDeployRunWindowE2E(t *testing.T) {
	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)

	marker := filepath.Join(t.TempDir(), "launched.marker")
	zipBytes, sha := buildProbeZip(t, exeName())
	srv, bundleHits := serveBundle(t, zipBytes, sha, exeName(), "window", marker)

	manifestURL := srv.URL + "/main.json"
	if err := cmdRun([]string{"--no-browser", manifestURL}); err != nil {
		t.Fatalf("run deploy bundle: %v", err)
	}
	waitMarker(t, marker)

	// Installed into ~/.hey/apps/<id>/<version>/ with the exec extracted.
	installExec := filepath.Join(heyHome, "apps", "probeapp", "1.2.3", exeName())
	if _, err := os.Stat(installExec); err != nil {
		t.Fatalf("bundle exec not installed at %s: %v", installExec, err)
	}

	// ls shows the deployed bundle.
	if err := cmdLs(nil); err != nil {
		t.Fatalf("ls: %v", err)
	}
	// which resolves the deployed bundle's install dir.
	if err := cmdWhich([]string{"probeapp"}); err != nil {
		t.Fatalf("which: %v", err)
	}

	// A second run reuses the cached extraction: the artifact must not be
	// re-downloaded (the manifest is still fetched to learn the version).
	os.Remove(marker)
	if err := cmdRun([]string{"--no-browser", manifestURL}); err != nil {
		t.Fatalf("second run should reuse cache: %v", err)
	}
	waitMarker(t, marker)
	if got := atomic.LoadInt64(bundleHits); got != 1 {
		t.Errorf("artifact fetched %d times; cached run should not re-download", got)
	}

	// cache clean removes the deployed bundle.
	if err := cmdCache([]string{"clean", "probeapp"}); err != nil {
		t.Fatalf("cache clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(heyHome, "apps", "probeapp")); !os.IsNotExist(err) {
		t.Error("deployed bundle should be gone after cache clean")
	}
}

// TestDeployRunTempE2E proves --temp installs to a throwaway dir, runs the
// bundle to completion (foreground for window apps), then deletes the dir.
func TestDeployRunTempE2E(t *testing.T) {
	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)

	marker := filepath.Join(t.TempDir(), "temp.marker")
	zipBytes, sha := buildProbeZip(t, exeName())
	srv, _ := serveBundle(t, zipBytes, sha, exeName(), "window", marker)

	if err := cmdRun([]string{"--no-browser", "--temp", srv.URL + "/main.json"}); err != nil {
		t.Fatalf("run --temp: %v", err)
	}
	// Foreground run: the marker is written before cmdRun returns.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker missing after --temp run: %v", err)
	}
	// The ephemeral dir was cleaned up: nothing lingers under ~/.hey/tmp.
	tmpRoot := filepath.Join(heyHome, "tmp")
	if entries, err := os.ReadDir(tmpRoot); err == nil {
		for _, e := range entries {
			t.Errorf("temp install dir not cleaned up: %s", e.Name())
		}
	}
	// And nothing was written to the persistent apps cache.
	if _, err := os.Stat(filepath.Join(heyHome, "apps", "probeapp")); !os.IsNotExist(err) {
		t.Error("--temp run should not populate the persistent apps cache")
	}
}

// TestDeployScopedRefResolves proves a @scope/id ref resolves through a
// registry scope template to the served manifest.
func TestDeployScopedRefResolves(t *testing.T) {
	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)

	marker := filepath.Join(t.TempDir(), "scoped.marker")
	zipBytes, sha := buildProbeZip(t, exeName())
	srv, _ := serveBundle(t, zipBytes, sha, exeName(), "window", marker)

	// A registry whose scope template points at the test server. {id}/{channel}
	// expand to probeapp/stable → /main.json is served regardless of path, so
	// the manifest is found; this exercises scope→URL resolution.
	reg := fmt.Sprintf(`{"hey_registry":0,"scopes":{"t":{"manifest_url":"%s/{id}/{channel}.json"}},
		"apps":{"placeholder":{"source":{"type":"github-release","repo":"x/y","tag_prefix":"","asset_template":"a.{ext}","checksums_asset":"c.txt","binary":"a"}}}}`, srv.URL)
	if err := os.WriteFile(filepath.Join(heyHome, "registry.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdRun([]string{"--no-browser", "@t/main"}); err != nil {
		t.Fatalf("run scoped ref: %v", err)
	}
	waitMarker(t, marker)
}
