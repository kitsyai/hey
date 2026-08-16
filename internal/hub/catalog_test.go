package hub

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/svc"
)

// writeFakeInstance hand-writes a svc.json in the exact shape internal/svc
// itself would (see internal/svc/instance.go's json tags) directly to
// <svcDir>/<name>/svc.json and reads it back via the package's own exported
// svc.LoadInstance — so the hub's tests exercise real disk I/O and a real
// *svc.Instance (dir set correctly by LoadInstance) without needing an
// unexported constructor or spawning a subprocess. pid uses os.Getpid() (the
// test binary itself, genuinely alive) to simulate a running service; pid=0
// simulates a stopped one (svc.Alive short-circuits on PID==0).
func writeFakeInstance(t *testing.T, svcDir, name, pack string, port, pid int) *svc.Instance {
	t.Helper()
	dir := filepath.Join(svcDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
		"name": "` + name + `", "pack": "` + pack + `", "version": "1.0.0", "driver": "archive-exec",
		"platform": "test", "port": ` + strconv.Itoa(port) + `, "user": "hey_test", "password": "pw",
		"pid": ` + strconv.Itoa(pid) + `, "state": "running", "created": "2026-01-01T00:00:00Z",
		"started": "2026-01-01T00:00:05Z"
	}`
	if err := os.WriteFile(filepath.Join(dir, "svc.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	inst, err := svc.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	p, err := strconv.Atoi(u)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	return p
}

func TestBuildCatalog_AbsentNeverProvisioned(t *testing.T) {
	svcDir := t.TempDir()
	packs := &svc.PackSet{Packs: map[string]svc.Pack{
		"vinci": {Pack: "vinci", Protocol: "openai-http"},
	}}
	cat, err := BuildCatalog(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Services) != 0 {
		t.Errorf("a never-provisioned pack must not appear in the catalog, got %+v", cat.Services)
	}
	if _, ok, err := ServiceByName(svcDir, packs, testConfig(), DefaultPort, "vinci"); err != nil || ok {
		t.Errorf("ServiceByName for a never-provisioned pack should be ok=false, got ok=%v err=%v", ok, err)
	}
}

func TestBuildCatalog_StoppedInstance(t *testing.T) {
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "vinci", "vinci", 51234, 0 /* not alive */)
	packs := &svc.PackSet{Packs: map[string]svc.Pack{
		"vinci": {Pack: "vinci", Protocol: "openai-http"},
	}}
	view, ok, err := ServiceByName(svcDir, packs, testConfig(), DefaultPort, "vinci")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if view.Health != "stopped" {
		t.Errorf("Health = %q, want stopped", view.Health)
	}
}

func TestBuildCatalog_WarmingVsAvailable(t *testing.T) {
	warmingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // "still loading the model"
	}))
	defer warmingSrv.Close()
	readySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer readySrv.Close()

	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "warming-vinci", "vinci", serverPort(t, warmingSrv), os.Getpid())
	writeFakeInstance(t, svcDir, "ready-vinci", "vinci", serverPort(t, readySrv), os.Getpid())

	pack := svc.Pack{Pack: "vinci", Protocol: "openai-http", Ready: svc.Ready{HTTP: "http://127.0.0.1:{port}/health"}}
	packs := &svc.PackSet{Packs: map[string]svc.Pack{"vinci": pack}}

	warmView, ok, err := ServiceByName(svcDir, packs, testConfig(), DefaultPort, "warming-vinci")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if warmView.Health != "unhealthy" {
		t.Errorf("Health = %q, want unhealthy (warming) for a process that's up but failing Ready", warmView.Health)
	}

	readyView, ok, err := ServiceByName(svcDir, packs, testConfig(), DefaultPort, "ready-vinci")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if readyView.Health != "healthy" {
		t.Errorf("Health = %q, want healthy (available)", readyView.Health)
	}
}

func TestBuildCatalog_URLsAndBridge(t *testing.T) {
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "vinci", "vinci", 51234, os.Getpid())
	writeFakeInstance(t, svcDir, "postgres", "postgres", 5432, os.Getpid())
	packs := &svc.PackSet{Packs: map[string]svc.Pack{
		"vinci":    {Pack: "vinci", Protocol: "openai-http"}, // no Ready — probeReady falls back to raw TCP, which will fail against port 51234 (nothing listening); health irrelevant to this test
		"postgres": {Pack: "postgres", Protocol: "postgres-wire", Bridge: &svc.Bridge{Path: "/svc/postgres", Kind: "http-query-api"}},
	}}
	vinciView, _, err := ServiceByName(svcDir, packs, testConfig(), DefaultPort, "vinci")
	if err != nil {
		t.Fatal(err)
	}
	if vinciView.URL != "http://127.0.0.1:51234" {
		t.Errorf("URL = %q", vinciView.URL)
	}
	if vinciView.HubURL != "http://127.0.0.1:8525/svc/vinci" {
		t.Errorf("HubURL = %q", vinciView.HubURL)
	}
	if vinciView.BridgeURL != "" {
		t.Errorf("a Bridge-less pack must not set BridgeURL, got %q", vinciView.BridgeURL)
	}

	pgView, _, err := ServiceByName(svcDir, packs, testConfig(), DefaultPort, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if pgView.BridgeURL != "http://127.0.0.1:8525/svc/postgres" {
		t.Errorf("BridgeURL = %q", pgView.BridgeURL)
	}
	if pgView.URL != "" || pgView.HubURL != "" {
		t.Errorf("a Bridge pack (postgres-wire) must not set direct URL/HubURL — a browser can't speak the raw protocol, got URL=%q HubURL=%q", pgView.URL, pgView.HubURL)
	}
}

func TestBuildCatalog_DisabledServiceExcluded(t *testing.T) {
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "vinci", "vinci", 51234, os.Getpid())
	packs := &svc.PackSet{Packs: map[string]svc.Pack{"vinci": {Pack: "vinci", Protocol: "openai-http"}}}
	cfg := testConfig()
	cfg.Services = map[string]ServiceConfig{"vinci": {Disabled: true}}
	if _, ok, _ := ServiceByName(svcDir, packs, cfg, DefaultPort, "vinci"); ok {
		t.Error("a disabled service must not appear, even if provisioned")
	}
}

func TestBuildCatalog_NonHubEligiblePackExcluded(t *testing.T) {
	svcDir := t.TempDir()
	writeFakeInstance(t, svcDir, "plainsvc", "plainsvc", 6000, os.Getpid())
	// No Protocol set — an internal/svc service with no browser-facing surface.
	packs := &svc.PackSet{Packs: map[string]svc.Pack{"plainsvc": {Pack: "plainsvc"}}}
	cat, err := BuildCatalog(svcDir, packs, testConfig(), DefaultPort, "0.1.0-dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Services) != 0 {
		t.Errorf("a pack with no Protocol must never appear in the hub catalog, got %+v", cat.Services)
	}
}
