package svc

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// tinyArchive zips one file named "tool" holding content, returning the
// archive path and its SHA-256.
func tinyArchive(t *testing.T, content string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("tool")
	w.Write([]byte(content))
	zw.Close()
	f.Close()
	data, _ := os.ReadFile(path)
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

func TestReplaceBinariesUpgradesStatelessInstance(t *testing.T) {
	plat := runtime.GOOS + "_" + runtime.GOARCH
	a1, s1 := tinyArchive(t, "v1")
	a2, s2 := tinyArchive(t, "v2")
	bySHA := map[string]string{s1: a1, s2: a2}
	fetcher := func(url, sha, dest string) error {
		archive, ok := bySHA[sha]
		if !ok {
			return errors.New("download failed")
		}
		return localFetcher(archive)(url, sha, dest)
	}
	pack := Pack{
		Pack: "tool", Driver: DriverArchiveExec, Stateless: true,
		Versions: map[string]Version{
			"1.0.0": {Artifacts: map[string]Artifact{plat: {URL: "https://example.invalid/1", SHA256: s1}}},
			"2.0.0": {Artifacts: map[string]Artifact{plat: {URL: "https://example.invalid/2", SHA256: s2}}},
			"3.0.0": {Artifacts: map[string]Artifact{plat: {URL: "https://example.invalid/3", SHA256: "ab" + s2[2:]}}},
		},
		Start: "{bin}/tool --port {port}",
		Ready: Ready{TCP: "127.0.0.1:{port}"},
	}

	svcDir := t.TempDir()
	inst := &Instance{Name: "tool", Pack: "tool", Version: "1.0.0", Platform: plat, Port: 5555, State: StateStopped, dir: filepath.Join(svcDir, "tool")}
	if err := os.MkdirAll(inst.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := provision(inst, pack, pack.Versions["1.0.0"].Artifacts[plat], fetcher); err != nil {
		t.Fatal(err)
	}
	if err := inst.Save(); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinaries(inst, pack, "2.0.0", fetcher); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(inst.ExeDir(), "tool"))
	saved, err := LoadInstance(inst.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" || saved.Version != "2.0.0" || saved.Port != 5555 {
		t.Fatalf("upgrade: binary %q, saved version %s, port %d", got, saved.Version, saved.Port)
	}

	// A failed download leaves the recorded version alone, so `hey up` retries.
	if err := replaceBinaries(saved, pack, "3.0.0", fetcher); err == nil {
		t.Fatal("expected the failed download to be reported")
	}
	again, _ := LoadInstance(inst.Dir())
	if again.Version != "2.0.0" {
		t.Fatalf("failed upgrade must not record the new version, got %s", again.Version)
	}
}
