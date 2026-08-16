package svc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kitsyai/hey/internal/proc"
)

// fakeModelFetcher writes payload to destPath and records how many times it
// was invoked — the stand-in for a real (multi-GB, network) model download,
// per the P1 build's instruction to test the model-server driver's lifecycle
// against a fake artifact rather than a real llama.cpp/GGUF pin.
func fakeModelFetcher(payload []byte, calls *int) ModelFetcher {
	return func(url, sha256Want, destPath string, progress func(done, total int64)) error {
		*calls++
		sum := sha256.Sum256(payload)
		if got := hex.EncodeToString(sum[:]); got != sha256Want {
			return fmt.Errorf("fakeModelFetcher: checksum mismatch: want %s got %s", sha256Want, got)
		}
		return os.WriteFile(destPath, payload, 0o644)
	}
}

func TestEnsureModel_ContentAddressedAndIdempotent(t *testing.T) {
	modelsDir := t.TempDir()
	payload := []byte("fake gguf bytes")
	sum := sha256.Sum256(payload)
	shaHex := hex.EncodeToString(sum[:])
	model := &ModelArtifact{URL: "https://example.invalid/model.gguf", SHA256: shaHex, Filename: "model.gguf"}

	calls := 0
	path, err := EnsureModel(modelsDir, model, fakeModelFetcher(payload, &calls), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(modelsDir, shaHex[:2], shaHex, "model.gguf")
	if path != wantPath {
		t.Errorf("path = %q, want content-addressed %q", path, wantPath)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("model content mismatch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("fetcher called %d times, want 1", calls)
	}

	// Idempotent: a second EnsureModel for the same digest must not re-fetch.
	path2, err := EnsureModel(modelsDir, model, fakeModelFetcher(payload, &calls), nil)
	if err != nil {
		t.Fatal(err)
	}
	if path2 != path {
		t.Errorf("second EnsureModel path = %q, want %q", path2, path)
	}
	if calls != 1 {
		t.Errorf("fetcher called %d times after dedup hit, want 1", calls)
	}
}

func TestEnsureModel_RejectsPendingPin(t *testing.T) {
	model := &ModelArtifact{URL: "https://example.invalid/model.gguf", SHA256: PendingPinSHA256, Filename: "model.gguf"}
	_, err := EnsureModel(t.TempDir(), model, fakeModelFetcher(nil, new(int)), nil)
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("expected a loud not-pinned error, got %v", err)
	}
}

func TestEnsureModel_NoModelConfigured(t *testing.T) {
	if _, err := EnsureModel(t.TempDir(), nil, nil, nil); err == nil {
		t.Error("expected an error for a nil model artifact")
	}
}

// modelServerPack is a driver-agnostic model-server pack around svcprobe,
// exactly as syntheticPack (driver_test.go) is for archive-exec: nothing
// service-specific lives outside the pack data. {model} in Start proves the
// resolved model path really reaches the launched process's argv, not just
// svc.json.
func modelServerPack(binSHA, modelSHA string) Pack {
	plat := runtime.GOOS + "_" + runtime.GOARCH
	return Pack{
		Pack:   "vinci-test",
		Driver: DriverModelServer,
		Versions: map[string]Version{
			"1.0.0": {Artifacts: map[string]Artifact{
				plat: {URL: "https://example.invalid/llama-server.zip", SHA256: binSHA},
			}},
		},
		Model: &ModelArtifact{URL: "https://example.invalid/model.gguf", SHA256: modelSHA, Filename: "model.gguf"},
		Start: "{bin}/svcprobe --port {port} --model {model} --model-marker {data}/model-arg.txt",
		Ready: Ready{TCP: "127.0.0.1:{port}", TimeoutSeconds: 10},
		Stop:  StopSpec{Signal: "term", GraceSeconds: 5},
	}
}

func TestModelServerLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("process-spawning lifecycle test; run without -short")
	}
	archivePath, binSHA := buildProbeArchive(t)
	modelPayload := []byte("fake qwen2.5-3b gguf bytes, obviously not a real model")
	modelSum := sha256.Sum256(modelPayload)
	modelSHA := hex.EncodeToString(modelSum[:])

	pack := modelServerPack(binSHA, modelSHA)
	svcDir := filepath.Join(t.TempDir(), "svc")
	modelCalls := 0

	inst, err := UpModel(svcDir, "vinci", pack, "1.0.0", "", localFetcher(archivePath), fakeModelFetcher(modelPayload, &modelCalls), nil)
	if err != nil {
		t.Fatalf("up model: %v", err)
	}
	t.Cleanup(func() { _ = proc.KillTree(inst.PID) })

	if modelCalls != 1 {
		t.Fatalf("model fetcher called %d times, want 1", modelCalls)
	}
	if inst.ModelPath == "" {
		t.Fatal("instance ModelPath not recorded")
	}
	if _, err := os.Stat(inst.ModelPath); err != nil {
		t.Fatalf("resolved model path does not exist: %v", err)
	}

	// The launched process actually received the resolved {model} path in
	// its argv (not just svc.json) — svcprobe wrote it to a marker file.
	// WaitReady only guarantees the TCP listener is up, which svcprobe opens
	// a moment after writing the marker — readMarkerWithRetry absorbs that
	// small gap (and any OS-level write-visibility lag under heavy parallel
	// test load) rather than requiring a same-instant read.
	got := readMarkerWithRetry(t, filepath.Join(inst.DataDir(), "model-arg.txt"))
	if got != inst.ModelPath {
		t.Errorf("process saw model path %q, want %q", got, inst.ModelPath)
	}

	// Idempotent restart: second UpModel reuses the running process and does
	// not re-fetch the model.
	inst2, err := UpModel(svcDir, "vinci", pack, "1.0.0", "", localFetcher(archivePath), fakeModelFetcher(modelPayload, &modelCalls), nil)
	if err != nil {
		t.Fatalf("second up model: %v", err)
	}
	if inst2.PID != inst.PID {
		t.Errorf("second UpModel restarted the process: %d vs %d", inst2.PID, inst.PID)
	}
	if modelCalls != 1 {
		t.Errorf("model fetcher called %d times after idempotent up, want 1", modelCalls)
	}

	_ = Stop(inst, pack)
}

// multiTierPack mirrors docs/design/services.md §8's tiny/small/medium size
// tiers: one server binary, several selectable named model artifacts.
func multiTierPack(binSHA string, tierSHAs map[string]string) Pack {
	plat := runtime.GOOS + "_" + runtime.GOARCH
	models := map[string]ModelArtifact{}
	for tier, sha := range tierSHAs {
		models[tier] = ModelArtifact{URL: "https://example.invalid/" + tier + ".gguf", SHA256: sha, Filename: tier + ".gguf"}
	}
	return Pack{
		Pack:   "vinci-tiers",
		Driver: DriverModelServer,
		Versions: map[string]Version{
			"1.0.0": {Artifacts: map[string]Artifact{
				plat: {URL: "https://example.invalid/llama-server.zip", SHA256: binSHA},
			}},
		},
		Models:       models,
		DefaultModel: "small",
		Start:        "{bin}/svcprobe --port {port} --model {model} --model-marker {data}/model-arg.txt",
		Ready:        Ready{TCP: "127.0.0.1:{port}", TimeoutSeconds: 10},
		Stop:         StopSpec{Signal: "term", GraceSeconds: 5},
	}
}

func TestUpModel_TierSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("process-spawning lifecycle test; run without -short")
	}
	archivePath, binSHA := buildProbeArchive(t)
	tinyPayload, smallPayload := []byte("tiny model bytes"), []byte("small model bytes, bigger")
	tierSHAs := map[string]string{"tiny": shaHex(tinyPayload), "small": shaHex(smallPayload)}
	pack := multiTierPack(binSHA, tierSHAs)
	svcDir := filepath.Join(t.TempDir(), "svc")

	fetcher := func(payloads map[string][]byte) ModelFetcher {
		return func(url, sha, dest string, _ func(int64, int64)) error {
			for _, p := range payloads {
				if hex.EncodeToString(mustSHA256(p)) == sha {
					return os.WriteFile(dest, p, 0o644)
				}
			}
			return fmt.Errorf("no fixture for sha %s", sha)
		}
	}(map[string][]byte{"tiny": tinyPayload, "small": smallPayload})

	// --model tiny selects the tiny tier explicitly.
	inst, err := UpModel(svcDir, "vinci-tiny", pack, "1.0.0", "tiny", localFetcher(archivePath), fetcher, nil)
	if err != nil {
		t.Fatalf("up model tiny: %v", err)
	}
	t.Cleanup(func() { _ = proc.KillTree(inst.PID) })
	if inst.ModelTier != "tiny" {
		t.Errorf("ModelTier = %q, want tiny", inst.ModelTier)
	}
	got := readMarkerWithRetry(t, filepath.Join(inst.DataDir(), "model-arg.txt"))
	if got != inst.ModelPath || !strings.Contains(inst.ModelPath, "tiny.gguf") {
		t.Errorf("expected the tiny model path, got %q (marker %q)", inst.ModelPath, got)
	}

	// Omitting --model on a fresh instance falls back to default_model (small).
	inst2, err := UpModel(svcDir, "vinci-default", pack, "1.0.0", "", localFetcher(archivePath), fetcher, nil)
	if err != nil {
		t.Fatalf("up model default: %v", err)
	}
	t.Cleanup(func() { _ = proc.KillTree(inst2.PID) })
	if inst2.ModelTier != "small" {
		t.Errorf("ModelTier = %q, want small (the pack default)", inst2.ModelTier)
	}

	_ = Stop(inst, pack)
	_ = Stop(inst2, pack)
}

func TestUpModel_SwitchingTierOnExistingInstanceRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("process-spawning lifecycle test; run without -short")
	}
	archivePath, binSHA := buildProbeArchive(t)
	tinyPayload, smallPayload := []byte("tiny model bytes"), []byte("small model bytes, bigger")
	tierSHAs := map[string]string{"tiny": shaHex(tinyPayload), "small": shaHex(smallPayload)}
	pack := multiTierPack(binSHA, tierSHAs)
	svcDir := filepath.Join(t.TempDir(), "svc")
	fetcher := func(url, sha, dest string, _ func(int64, int64)) error {
		if sha == shaHex(tinyPayload) {
			return os.WriteFile(dest, tinyPayload, 0o644)
		}
		return os.WriteFile(dest, smallPayload, 0o644)
	}

	inst, err := UpModel(svcDir, "vinci", pack, "1.0.0", "tiny", localFetcher(archivePath), fetcher, nil)
	if err != nil {
		t.Fatalf("up model tiny: %v", err)
	}
	t.Cleanup(func() { _ = proc.KillTree(inst.PID) })

	_, err = UpModel(svcDir, "vinci", pack, "1.0.0", "small", localFetcher(archivePath), fetcher, nil)
	if err == nil || !strings.Contains(err.Error(), "hey down") {
		t.Fatalf("expected a clear refusal to silently switch tiers, got %v", err)
	}
	_ = Stop(inst, pack)
}

func TestResolveModel_UnknownTier(t *testing.T) {
	pack := multiTierPack("x", map[string]string{"tiny": strings.Repeat("a", 64)})
	if _, _, err := pack.ResolveModel("giant"); err == nil || !strings.Contains(err.Error(), "available") {
		t.Errorf("expected an unknown-tier error listing available tiers, got %v", err)
	}
}

// readMarkerWithRetry polls path for up to 2s — svcprobe writes its
// model-arg marker a moment before opening the TCP listener WaitReady polls,
// and under heavy parallel test load (many svcprobe processes competing for
// CPU/disk) that gap can occasionally outlast a single immediate read.
func readMarkerWithRetry(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		last = data
		time.Sleep(20 * time.Millisecond)
	}
	return string(last)
}

func mustSHA256(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func shaHex(b []byte) string { return hex.EncodeToString(mustSHA256(b)) }

func TestUp_RejectsPendingArtifactPin(t *testing.T) {
	plat := runtime.GOOS + "_" + runtime.GOARCH
	pack := Pack{
		Pack:   "pending",
		Driver: DriverArchiveExec,
		Versions: map[string]Version{"1.0.0": {Artifacts: map[string]Artifact{
			plat: {URL: "https://example.invalid/x.zip", SHA256: PendingPinSHA256},
		}}},
		Start: "{bin}/x --port {port}",
		Ready: Ready{TCP: "127.0.0.1:{port}"},
	}
	_, err := Up(t.TempDir(), "pending", pack, "1.0.0", nil)
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("expected a loud not-pinned error before any network attempt, got %v", err)
	}
}
