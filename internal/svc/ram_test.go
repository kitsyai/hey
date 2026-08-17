package svc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/sysmem"
)

func writeFakeModel(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPreflightModelRAM_SkipEnvBypasses(t *testing.T) {
	t.Setenv(SkipRAMCheckEnv, "1")
	model := &ModelArtifact{MinRAMMB: 1 << 30} // absurd: an exabyte
	if err := preflightModelRAM("vinci", writeFakeModel(t, 1), model); err != nil {
		t.Fatalf("HEY_SKIP_RAM_CHECK=1 must bypass, got: %v", err)
	}
}

func TestPreflightModelRAM_RefusesImpossibleRequirement(t *testing.T) {
	if avail, err := sysmem.Available(); err != nil || avail == 0 {
		t.Skip("available-memory probe unsupported here; check fails open by design")
	}
	t.Setenv(SkipRAMCheckEnv, "")
	t.Setenv(RAMHeadroomEnv, "")
	model := &ModelArtifact{MinRAMMB: 1 << 30} // an exabyte: no machine passes
	err := preflightModelRAM("vinci", writeFakeModel(t, 1), model)
	if err == nil {
		t.Fatal("expected refusal for an impossibly large model requirement")
	}
	for _, want := range []string{"refusing to start vinci", "headroom", SkipRAMCheckEnv, RAMHeadroomEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestPreflightModelRAM_AllowsTinyModel(t *testing.T) {
	if avail, err := sysmem.Available(); err != nil || avail == 0 {
		t.Skip("available-memory probe unsupported here")
	}
	t.Setenv(SkipRAMCheckEnv, "")
	t.Setenv(RAMHeadroomEnv, "1") // 1 MB headroom: a 1-byte model must pass
	if err := preflightModelRAM("vinci", writeFakeModel(t, 1), nil); err != nil {
		t.Fatalf("tiny model should pass the RAM check, got: %v", err)
	}
}

func TestPreflightModelRAM_FailsOpenWithoutSizeSignal(t *testing.T) {
	t.Setenv(SkipRAMCheckEnv, "")
	// Nonexistent model file and no explicit requirement: nothing to size the
	// check from, so it must not block the launch.
	if err := preflightModelRAM("vinci", filepath.Join(t.TempDir(), "missing.gguf"), nil); err != nil {
		t.Fatalf("expected fail-open with no size signal, got: %v", err)
	}
}

func TestRAMHeadroomBytes_InvalidOverrideFallsBack(t *testing.T) {
	t.Setenv(RAMHeadroomEnv, "not-a-number")
	if got := ramHeadroomBytes(); got != DefaultRAMHeadroomMB<<20 {
		t.Errorf("invalid override: headroom = %d, want default %d", got, DefaultRAMHeadroomMB<<20)
	}
	t.Setenv(RAMHeadroomEnv, "512")
	if got := ramHeadroomBytes(); got != 512<<20 {
		t.Errorf("headroom = %d, want %d", got, 512<<20)
	}
}
