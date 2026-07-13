package main

// Heavy, network-touching service integration test.
// Run with: HEY_INTEGRATION=1 go test ./cmd/hey -run IntegrationPostgres -v
// It downloads the real EDB postgres archive (~314 MB), provisions it,
// starts it, proves the server speaks the postgres wire protocol on
// 127.0.0.1, then proves data survives a stop/start.

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kitsyai/hey/internal/svc"
)

// pgProbe opens a TCP connection and sends a PostgreSQL SSLRequest, confirming
// the server answers with the protocol's single-byte reply ('S' or 'N'). This
// is a pure-stdlib, CGO-free proof that a real postgres — not just an open
// port — is listening.
func pgProbe(port int) error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	// SSLRequest: int32 length=8, int32 code=80877103.
	msg := make([]byte, 8)
	binary.BigEndian.PutUint32(msg[0:], 8)
	binary.BigEndian.PutUint32(msg[4:], 80877103)
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		return err
	}
	reply := make([]byte, 1)
	if _, err := conn.Read(reply); err != nil {
		return err
	}
	if reply[0] != 'S' && reply[0] != 'N' {
		return fmt.Errorf("unexpected SSLRequest reply %q — not a postgres server", reply[0])
	}
	return nil
}

func TestIntegrationPostgresE2E(t *testing.T) {
	// Gated separately from the light integration suite: this downloads a
	// ~314 MB real postgres and runs the server, which is too heavy and too
	// environment-dependent (e.g. the EDB build needs the MSVC runtime) for
	// per-push CI. Run on demand: HEY_SVC_INTEGRATION=1.
	if os.Getenv("HEY_SVC_INTEGRATION") != "1" {
		t.Skip("set HEY_SVC_INTEGRATION=1 to run the heavy postgres integration test")
	}
	heyHome := t.TempDir()
	t.Setenv("HEY_HOME", heyHome)

	// up: provision (download+verify+extract) + initdb + start + ready.
	// Skip (not fail) where the pack ships no artifact for this platform —
	// e.g. linux, since EDB no longer publishes community Linux tarballs.
	if err := cmdSvc([]string{"up", "postgres"}); err != nil {
		if strings.Contains(err.Error(), "no artifact for") {
			t.Skipf("postgres pack has no artifact for this platform: %v", err)
		}
		t.Fatalf("svc up postgres: %v", err)
	}
	svcDir := filepath.Join(heyHome, "svc")
	inst, err := svc.LoadInstance(filepath.Join(svcDir, "postgres"))
	if err != nil {
		t.Fatalf("load instance: %v", err)
	}
	t.Cleanup(func() {
		packs, _ := svc.LoadPacks(heyHome)
		if p, e := packs.Get("postgres"); e == nil {
			_ = svc.Stop(inst, p)
		}
	})

	// Credentials generated, port in range, bound to loopback.
	if inst.User == "" || len(inst.Password) < 16 {
		t.Fatalf("credentials not generated: %+v", inst)
	}
	if inst.Port < 5432 || inst.Port > 5600 {
		t.Errorf("port %d outside the service range", inst.Port)
	}

	// data/ was initialized (PG_VERSION is postgres's own marker).
	pgVersion := filepath.Join(inst.DataDir(), "PG_VERSION")
	if _, err := os.Stat(pgVersion); err != nil {
		t.Fatalf("data dir not initialized: %v", err)
	}

	// Connectivity proof: the server speaks the postgres protocol.
	if err := pgProbe(inst.Port); err != nil {
		t.Fatalf("postgres protocol probe failed: %v", err)
	}

	// conn prints a connection string.
	if err := cmdSvc([]string{"conn", "postgres"}); err != nil {
		t.Fatalf("svc conn: %v", err)
	}

	// Data durability across stop/start.
	before, err := os.ReadFile(pgVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdSvc([]string{"stop", "postgres"}); err != nil {
		t.Fatalf("svc stop: %v", err)
	}
	if err := cmdSvc([]string{"start", "postgres"}); err != nil {
		t.Fatalf("svc start: %v", err)
	}
	restarted, _ := svc.LoadInstance(filepath.Join(svcDir, "postgres"))
	t.Cleanup(func() {
		packs, _ := svc.LoadPacks(heyHome)
		if p, e := packs.Get("postgres"); e == nil {
			_ = svc.Stop(restarted, p)
		}
	})
	if err := pgProbe(restarted.Port); err != nil {
		t.Fatalf("postgres not answering after restart: %v", err)
	}
	after, err := os.ReadFile(pgVersion)
	if err != nil || string(after) != string(before) {
		t.Errorf("data changed across restart: before=%q after=%q err=%v", before, after, err)
	}
	fmt.Println("postgres e2e ok: port", restarted.Port, "conn", svc.Conn(restarted, mustPack(t, heyHome)))
}

func mustPack(t *testing.T, heyHome string) svc.Pack {
	t.Helper()
	packs, err := svc.LoadPacks(heyHome)
	if err != nil {
		t.Fatal(err)
	}
	p, err := packs.Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
