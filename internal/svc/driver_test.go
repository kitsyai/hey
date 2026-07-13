package svc

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kitsyai/hey/internal/fetch"
	"github.com/kitsyai/hey/internal/proc"
)

// buildProbeArchive builds the svcprobe helper, zips it at the archive root,
// and returns the archive path plus its real SHA-256 — so the synthetic pack
// pins a genuine checksum and the lifecycle runs with zero network.
func buildProbeArchive(t *testing.T) (archivePath, sha string) {
	t.Helper()
	tmp := t.TempDir()
	exe := "svcprobe"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	binPath := filepath.Join(tmp, exe)
	build := exec.Command("go", "build", "-o", binPath, "../svcprobe")
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
	hdr := &zip.FileHeader{Name: exe, Method: zip.Deflate}
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

	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return archivePath, hex.EncodeToString(sum[:])
}

// localFetcher mimics HTTPSFetcher without a network: it verifies the archive's
// real SHA-256 against the pinned value, then extracts it via the same
// zip-slip-safe pipeline the production fetcher uses.
func localFetcher(archivePath string) Fetcher {
	return func(_ /*url*/ string, wantSHA, destDir string) error {
		data, err := os.ReadFile(archivePath)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != wantSHA {
			return fmt.Errorf("checksum mismatch: want %s got %s", wantSHA, got)
		}
		return fetch.ExtractTree(archivePath, destDir)
	}
}

// syntheticPack builds a driver-agnostic pack around svcprobe. Note: no field
// here is service-specific — it is exactly the shape a real pack would take.
func syntheticPack(sha string) Pack {
	plat := runtime.GOOS + "_" + runtime.GOARCH
	return Pack{
		Pack:   "probe",
		Driver: DriverArchiveExec,
		Versions: map[string]Version{
			"1.0.0": {Artifacts: map[string]Artifact{
				plat: {URL: "https://example.invalid/probe.zip", SHA256: sha},
			}},
		},
		// {data}/probe.init proves init-once: svcprobe --init writes it and exits.
		Init:  []string{"{bin}/svcprobe --init {data}/probe.init"},
		Start: "{bin}/svcprobe --port {port}",
		Ready: Ready{TCP: "127.0.0.1:{port}", TimeoutSeconds: 10},
		Conn:  "probe://{user}:{password}@127.0.0.1:{port}",
		Stop:  StopSpec{Signal: "term", GraceSeconds: 5},
	}
}

func TestArchiveExecLifecycle(t *testing.T) {
	archivePath, sha := buildProbeArchive(t)
	pack := syntheticPack(sha)
	svcDir := t.TempDir()

	inst, err := Up(svcDir, "probe", pack, "1.0.0", localFetcher(archivePath))
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	t.Cleanup(func() { _ = proc.KillTree(inst.PID) })

	// Provisioned: extracted binary present.
	exe := "svcprobe"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if _, err := os.Stat(filepath.Join(inst.ExeDir(), exe)); err != nil {
		t.Fatalf("binary not extracted: %v", err)
	}

	// Init ran exactly once (marker written by the pack's init command).
	initMark := filepath.Join(inst.DataDir(), "probe.init")
	fi, err := os.Stat(initMark)
	if err != nil {
		t.Fatalf("pack init did not run: %v", err)
	}
	initModTime := fi.ModTime()

	// Credentials generated (never defaulted) and non-empty.
	if inst.User == "" || len(inst.Password) < 16 {
		t.Fatalf("weak/absent credentials: user=%q passlen=%d", inst.User, len(inst.Password))
	}

	// svc.json is mode 0600 (holds credentials).
	si, err := os.Stat(filepath.Join(inst.Dir(), "svc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && si.Mode().Perm() != 0o600 {
		t.Errorf("svc.json mode = %o, want 600", si.Mode().Perm())
	}

	// Ready: the port actually answers, on loopback only.
	if !dialable(t, inst.Port) {
		t.Fatal("service port not answering after up")
	}

	// Conn string carries the generated creds and port.
	conn := Conn(inst, pack)
	if conn == "" || !strings.Contains(conn, inst.User) ||
		!strings.Contains(conn, inst.Password) ||
		!strings.Contains(conn, fmt.Sprintf("%d", inst.Port)) {
		t.Errorf("conn string missing substitutions: %q", conn)
	}

	// Idempotent up: reuses the same process.
	inst2, err := Up(svcDir, "probe", pack, "1.0.0", localFetcher(archivePath))
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if inst2.PID != inst.PID {
		t.Errorf("second up restarted the process: %d vs %d", inst2.PID, inst.PID)
	}

	// Stop: graceful, process gone, port freed.
	if err := Stop(inst, pack); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if proc.Alive(inst.PID) {
		t.Error("process still alive after stop")
	}
	if dialable(t, inst.Port) {
		t.Error("port still answering after stop")
	}

	// Restart: data survives, init does NOT re-run (marker mtime unchanged).
	reloaded, err := LoadInstance(inst.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(reloaded, pack); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { _ = proc.KillTree(reloaded.PID) })
	if err := WaitReady(reloaded, pack); err != nil {
		t.Fatalf("restart ready: %v", err)
	}
	if err := runInit(reloaded, pack); err != nil {
		t.Fatalf("re-run init guard: %v", err)
	}
	fi2, err := os.Stat(initMark)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(initModTime) {
		t.Error("init re-ran on restart — should be once-only")
	}
	_ = Stop(reloaded, pack)
}

func TestArtifactResolutionUnknownPlatform(t *testing.T) {
	pack := Pack{
		Pack: "x", Driver: DriverArchiveExec,
		Versions: map[string]Version{"1.0.0": {Artifacts: map[string]Artifact{
			"plan9_sparc": {URL: "https://x/y", SHA256: "z"},
		}}},
	}
	if _, _, err := pack.Artifact("1.0.0"); err == nil {
		t.Error("expected error resolving artifact for a foreign platform")
	}
}

func dialable(t *testing.T, port int) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
