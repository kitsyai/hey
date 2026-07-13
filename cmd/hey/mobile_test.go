package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/deploy"
	"github.com/kitsyai/hey/internal/fetch"
)

// mockADB builds the adbprobe helper as "adb" onto a temp dir, prepends that
// dir to PATH, and points adb's log at a file the test can inspect. It returns
// the log path so a test can assert exactly how hey invoked adb.
func mockADB(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	adbName := "adb"
	if runtime.GOOS == "windows" {
		adbName = "adb.exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, adbName), "../../internal/adbprobe")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build adbprobe: %v: %s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	logPath = filepath.Join(t.TempDir(), "adb.log")
	t.Setenv("ADB_MOCK_LOG", logPath)
	return logPath
}

// serveAPKManifest serves an android/package manifest at /pkg.json and the apk
// bytes at /app.apk over loopback TLS, pointing hey's clients at it.
func serveAPKManifest(t *testing.T) *httptest.Server {
	t.Helper()
	apk := []byte("PK\x03\x04 not-a-real-apk but has a real sha256")
	sum := sha256.Sum256(apk)
	sha := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/app.apk", func(w http.ResponseWriter, r *http.Request) { w.Write(apk) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{
  "hey_deploy": 1,
  "id": "mobileapp",
  "version": "3.0.0",
  "channel": "stable",
  "artifacts": [
    {"platform":"android","kind":"package","url":"https://%s/app.apk","sha256":%q},
    {"platform":"ios","kind":"link","url":"https://testflight.apple.com/join/abc"}
  ]
}`, r.Host, sha)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	oldF, oldD := fetch.Client, deploy.Client
	fetch.Client = srv.Client()
	deploy.Client = srv.Client()
	t.Cleanup(func() { fetch.Client, deploy.Client = oldF, oldD })
	return srv
}

// TestMobilePushInvokesADB drives the whole push flow against a mock adb:
// resolve manifest → pick the android package → verify sha256 → adb -s install.
func TestMobilePushInvokesADB(t *testing.T) {
	t.Setenv("HEY_HOME", t.TempDir())
	logPath := mockADB(t)
	srv := serveAPKManifest(t)

	if err := cmdMobile([]string{"push", srv.URL + "/pkg.json", "--device", "emulator-5554"}); err != nil {
		t.Fatalf("mobile push: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read adb log: %v", err)
	}
	log := string(data)
	// hey must have called: adb -s emulator-5554 install <apk>
	if !strings.Contains(log, "-s emulator-5554 install ") {
		t.Errorf("adb not invoked with -s <device> install: %q", log)
	}
	if !strings.Contains(log, ".apk") && !strings.Contains(log, "hey-download") && !strings.Contains(log, "run-") {
		// The apk path is a temp file; just confirm an install target was passed.
		if !strings.Contains(log, "install ") || strings.TrimSpace(strings.SplitN(log, "install ", 2)[1]) == "" {
			t.Errorf("adb install had no apk argument: %q", log)
		}
	}
}

// TestMobilePushRejectsAppName ensures a bare github-release name is not a
// valid push target.
func TestMobilePushRejectsAppName(t *testing.T) {
	t.Setenv("HEY_HOME", t.TempDir())
	mockADB(t)
	if err := cmdMobile([]string{"push", "guten"}); err == nil || !strings.Contains(err.Error(), "deploy ref") {
		t.Errorf("push of a bare app name should be rejected, got %v", err)
	}
}

// TestMobileDevices parses the mock adb's device list without a real device.
func TestMobileDevices(t *testing.T) {
	mockADB(t)
	if err := cmdMobile([]string{"devices"}); err != nil {
		t.Fatalf("mobile devices: %v", err)
	}
}

func TestParseDevices(t *testing.T) {
	out := "List of devices attached\nemulator-5554\tdevice\n192.168.1.42:5555\tdevice\n\n* daemon started *\n"
	devs := parseDevices(out)
	if len(devs) != 2 {
		t.Fatalf("want 2 devices, got %d: %+v", len(devs), devs)
	}
	if devs[0].Serial != "emulator-5554" || devs[0].State != "device" {
		t.Errorf("bad first device: %+v", devs[0])
	}
	if devs[1].Serial != "192.168.1.42:5555" {
		t.Errorf("network device not parsed: %+v", devs[1])
	}
}

// TestOpenLink resolves a manifest and opens its link artifact (captured, no
// real browser launched).
func TestOpenLink(t *testing.T) {
	t.Setenv("HEY_HOME", t.TempDir())
	srv := serveAPKManifest(t)

	var opened string
	old := browserOpen
	browserOpen = func(url string) error { opened = url; return nil }
	defer func() { browserOpen = old }()

	if err := cmdOpen([]string{srv.URL + "/pkg.json"}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != "https://testflight.apple.com/join/abc" {
		t.Errorf("opened %q, want the testflight link", opened)
	}
}
