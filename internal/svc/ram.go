package svc

import (
	"fmt"
	"os"
	"strconv"

	"github.com/kitsyai/hey/internal/sysmem"
)

// Pre-launch RAM check knobs (model-server driver only). A llama.cpp server
// launched into a machine that cannot hold model + KV cache + everything else
// already running doesn't fail politely — it thrashes and can take the whole
// machine down. So Start refuses up front when available physical memory is
// below (model requirement + headroom), with the numbers in the error.
const (
	// SkipRAMCheckEnv disables the pre-launch RAM check entirely ("1"/"true").
	SkipRAMCheckEnv = "HEY_SKIP_RAM_CHECK"
	// RAMHeadroomEnv overrides the headroom, in MB, kept free on top of the
	// model's own requirement (default DefaultRAMHeadroomMB).
	RAMHeadroomEnv = "HEY_MODEL_RAM_HEADROOM_MB"
	// DefaultRAMHeadroomMB is the default headroom: 2 GB for the server's KV
	// cache/compute buffers plus the rest of the OS staying usable.
	DefaultRAMHeadroomMB = 2048
)

// ramHeadroomBytes resolves the configured headroom (env override, else the
// 2 GB default). A malformed or negative override falls back to the default —
// a typo must never silently disable the safety margin.
func ramHeadroomBytes() uint64 {
	if s := os.Getenv(RAMHeadroomEnv); s != "" {
		if mb, err := strconv.ParseUint(s, 10, 32); err == nil {
			return mb << 20
		}
		fmt.Fprintf(os.Stderr, "hey: ignoring invalid %s=%q (want a size in MB); using %d MB\n",
			RAMHeadroomEnv, os.Getenv(RAMHeadroomEnv), DefaultRAMHeadroomMB)
	}
	return DefaultRAMHeadroomMB << 20
}

func skipRAMCheck() bool {
	switch os.Getenv(SkipRAMCheckEnv) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// preflightModelRAM refuses a model-server launch when available physical
// memory is below the model's requirement plus headroom. The requirement is
// the pack's explicit min_ram_mb when set, else the model file's on-disk size
// (a GGUF is loaded/mapped roughly whole; the KV cache and buffers live in
// the headroom). Fail-open by design: if memory can't be measured (probe
// unsupported or erroring) or the model file can't be statted, launch
// proceeds — the check exists to stop predictable crashes, never to add a
// flaky new failure mode. inst names the instance for the error message.
func preflightModelRAM(instName, modelPath string, model *ModelArtifact) error {
	if skipRAMCheck() {
		return nil
	}
	avail, err := sysmem.Available()
	if err != nil || avail == 0 {
		return nil // can't measure here — fail open
	}
	var modelNeed uint64
	if model != nil && model.MinRAMMB > 0 {
		modelNeed = uint64(model.MinRAMMB) << 20
	}
	if fi, err := os.Stat(modelPath); err == nil && uint64(fi.Size()) > modelNeed {
		modelNeed = uint64(fi.Size())
	}
	if modelNeed == 0 {
		return nil // nothing to size the requirement from
	}
	headroom := ramHeadroomBytes()
	need := modelNeed + headroom
	if avail >= need {
		return nil
	}
	return fmt.Errorf(
		"refusing to start %s: %s of RAM available, but the model needs about %s (%s model + %s headroom).\n"+
			"  Free some memory (close applications) and retry, or:\n"+
			"    - adjust the headroom:      set %s (MB, default %d)\n"+
			"    - bypass the check (risky): set %s=1",
		instName, humanBytes(avail), humanBytes(need), humanBytes(modelNeed), humanBytes(headroom),
		RAMHeadroomEnv, DefaultRAMHeadroomMB, SkipRAMCheckEnv)
}

func humanBytes(n uint64) string {
	const (
		mb = 1 << 20
		gb = 1 << 30
	)
	if n >= gb {
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	}
	return fmt.Sprintf("%d MB", n/mb)
}
