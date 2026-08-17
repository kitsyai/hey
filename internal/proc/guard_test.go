package proc

import (
	"os/exec"
	"strings"
	"testing"
)

func TestLooksLikeTestBinary(t *testing.T) {
	cases := map[string]bool{
		`C:\Users\x\AppData\Local\Temp\go-build12345\b001\hey.test.exe`: true,
		`/tmp/go-build98765/b001/hey.test`:                              true,
		`/home/x/pkg.test`:                                              true,
		`C:\Users\x\pkg.test.exe`:                                       true,
		`C:\Program Files\hey\hey.exe`:                                  false,
		`/usr/local/bin/hey`:                                            false,
	}
	for path, want := range cases {
		if got := LooksLikeTestBinary(path); got != want {
			t.Errorf("LooksLikeTestBinary(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestGuardReexec_RefusesTestBinary(t *testing.T) {
	err := GuardReexec(`/tmp/go-build1/b001/hey.test`, "hub")
	if err == nil {
		t.Fatal("expected refusal for a test-binary path")
	}
	if !strings.Contains(err.Error(), "test binary") {
		t.Errorf("error should mention the test binary, got: %v", err)
	}
}

func TestGuardReexec_RefusesInsideDaemonChild(t *testing.T) {
	t.Setenv(DaemonChildEnv, "hub")
	err := GuardReexec(`/usr/local/bin/hey`, "registry")
	if err == nil {
		t.Fatal("expected refusal when HEY_DAEMON_CHILD is set")
	}
	if !strings.Contains(err.Error(), DaemonChildEnv) || !strings.Contains(err.Error(), "registry") {
		t.Errorf("error should name the marker and the spawn kind, got: %v", err)
	}
}

func TestGuardReexec_AllowsNormalBinary(t *testing.T) {
	// t.Setenv registers cleanup; use it to guarantee the var is absent even
	// if the enclosing environment set it.
	t.Setenv(DaemonChildEnv, "")
	if err := GuardReexec(`/usr/local/bin/hey`, "hub"); err != nil {
		t.Fatalf("expected a real binary outside a daemon child to be allowed, got: %v", err)
	}
}

func TestMarkDaemonChild_SetsAndReplacesMarker(t *testing.T) {
	t.Setenv(DaemonChildEnv, "outer")
	cmd := exec.Command("x")
	MarkDaemonChild(cmd, "svc:postgres")
	var found []string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, DaemonChildEnv+"=") {
			found = append(found, kv)
		}
	}
	if len(found) != 1 || found[0] != DaemonChildEnv+"=svc:postgres" {
		t.Errorf("child env marker = %v, want exactly [%s=svc:postgres]", found, DaemonChildEnv)
	}
}
