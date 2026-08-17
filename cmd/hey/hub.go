package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/hub"
	"github.com/kitsyai/hey/internal/proc"
	"github.com/kitsyai/hey/internal/svc"
)

const hubUsage = `hey hub — the discovery hub for hey services (docs/design/services.md)

Every service 'hey up' starts is discoverable and browser-reachable through
one loopback front door, 127.0.0.1:8525 (HEY_HUB_PORT to override): GET
/services lists what's running, /svc/<name>/* reverse-proxies to it with a
CORS/PNA security envelope applied once. Usually you don't run this
directly — 'hey up <service>' starts the hub automatically if it isn't
already running.

Usage:
  hey hub start    start in the background (pidfile-tracked)
  hey hub stop     stop the background hub
  hey hub status   running?, port, known services
`

func cmdHub(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, hubUsage)
		return exitCodeError(2)
	}
	switch args[0] {
	case "start":
		return hubStart(args[1:])
	case "stop":
		return hubStop(args[1:])
	case "status":
		return hubStatus(args[1:])
	case "__serve-detached":
		// internal: the detached child spawned by `hey hub start`
		return hubServe(true)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, hubUsage)
		return nil
	default:
		return fmt.Errorf("unknown hub subcommand %q\n%s", args[0], hubUsage)
	}
}

func hubHandler() (http.Handler, *hub.Config, int, error) {
	heyHome, err := home.Dir()
	if err != nil {
		return nil, nil, 0, err
	}
	cfg, err := hub.LoadConfig(heyHome)
	if err != nil {
		return nil, nil, 0, err
	}
	port, err := hub.ResolvePort(cfg)
	if err != nil {
		return nil, nil, 0, err
	}
	packs, err := svc.LoadPacks(heyHome)
	if err != nil {
		return nil, nil, 0, err
	}
	svcDir, err := home.SvcDir()
	if err != nil {
		return nil, nil, 0, err
	}
	return hub.NewHandler(svcDir, packs, cfg, port, version), cfg, port, nil
}

func hubServe(detached bool) error {
	handler, _, port, err := hubHandler()
	if err != nil {
		return err
	}
	hubDir, err := home.HubDir()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if detached {
		if err := hub.WritePidFile(hubDir, hub.PidInfo{PID: os.Getpid(), Port: port}); err != nil {
			return err
		}
		defer hub.RemovePidFile(hubDir)
	}
	fmt.Printf("hey hub: serving at http://%s/ (services: http://%s/services)\n", addr, addr)
	return http.ListenAndServe(addr, handler)
}

func hubStart(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey hub start")
	}
	if _, _, port, err := hubHandler(); err == nil {
		if EnsureHubRunning(port) {
			fmt.Printf("hey hub already running at http://127.0.0.1:%d/\n", port)
			return nil
		}
	}
	return startHubDetached()
}

// startHubDetached spawns `hey hub __serve-detached` and waits for it to
// come up. Split out from hubStart so `hey up` can call it directly without
// re-parsing args.
//
// Two guard layers protect this re-exec of the hey binary (both in
// proc.GuardReexec): the test-binary shape check, born of a real runaway
// recursive spawn under `go test` (a test binary exec'd with "hub
// __serve-detached" just re-runs its whole suite — see cmd/hey/hub_test.go);
// and the HEY_DAEMON_CHILD env marker, stamped on every background child
// hey spawns, so a background child that somehow reaches this path again is
// stopped at depth one instead of forking forever. Single-instance
// enforcement: a live pidfile or an already-bound port refuses a second
// spawn; a child that never comes up is killed rather than orphaned.
func startHubDetached() error {
	_, _, port, err := hubHandler()
	if err != nil {
		return err
	}
	hubDir, err := home.HubDir()
	if err != nil {
		return err
	}
	if info, ok := hub.ReadPidFile(hubDir); ok && proc.Alive(info.PID) {
		return fmt.Errorf("start: a hey hub is already recorded as running (pid %d, port %d) — `hey hub stop` first", info.PID, info.Port)
	}
	if hub.ServerUp(port) {
		return fmt.Errorf("start: port %d is already in use by another process (not a hey hub)", port)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := proc.GuardReexec(exe, "hub"); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	cmd := exec.Command(exe, "hub", "__serve-detached")
	proc.Detach(cmd)
	proc.MarkDaemonChild(cmd, "hub")
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	for i := 0; i < 50 && !hub.ServerUp(port); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !hub.ServerUp(port) {
		// Don't leave a half-started child behind with nothing tracking it.
		_ = proc.KillTree(cmd.Process.Pid)
		return fmt.Errorf("start: hub did not come up on port %d (spawned pid %d killed)", port, cmd.Process.Pid)
	}
	fmt.Printf("hey hub started at http://127.0.0.1:%d/ (pid %d) — stop with: hey hub stop\n", port, cmd.Process.Pid)
	return nil
}

// looksLikeTestBinary reports whether exe has the path/name shape of a
// `go test`-compiled binary. Kept as a thin alias — the shared implementation
// (with the full rationale) lives in internal/proc, next to the rest of the
// spawn-loop guard machinery, so `hey registry start` and any future detached
// spawn use the identical check.
func looksLikeTestBinary(exe string) bool {
	return proc.LooksLikeTestBinary(exe)
}

// EnsureHubRunning reports whether the hub is already reachable on port —
// `hey up` uses this to decide whether it needs to start one first
// (docs/design/services.md §4 "hey up <service> composes three existing
// calls" — step 1, "ensures the hub is running").
func EnsureHubRunning(port int) bool {
	return hub.ServerUp(port)
}

func hubStop(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey hub stop")
	}
	hubDir, err := home.HubDir()
	if err != nil {
		return err
	}
	info, ok := hub.ReadPidFile(hubDir)
	if !ok {
		fmt.Println("hey hub: no background server recorded")
		return nil
	}
	if proc.Alive(info.PID) {
		_ = proc.TermSignal(info.PID, "term")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && proc.Alive(info.PID) {
			time.Sleep(100 * time.Millisecond)
		}
		if proc.Alive(info.PID) {
			_ = proc.KillTree(info.PID)
		}
	}
	hub.RemovePidFile(hubDir)
	fmt.Printf("hey hub stopped (pid %d)\n", info.PID)
	return nil
}

func hubStatus(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: hey hub status")
	}
	heyHome, err := home.Dir()
	if err != nil {
		return err
	}
	cfg, err := hub.LoadConfig(heyHome)
	if err != nil {
		return err
	}
	port, err := hub.ResolvePort(cfg)
	if err != nil {
		return err
	}
	hubDir, err := home.HubDir()
	if err != nil {
		return err
	}
	info, ok := hub.ReadPidFile(hubDir)
	running := ok && hub.ServerUp(info.Port)
	if !running {
		fmt.Println("hey hub: not running")
		fmt.Printf("  configured port: %d\n", port)
		return nil
	}
	fmt.Printf("hey hub: running (pid %d)\n", info.PID)
	fmt.Printf("  port: %d\n", info.Port)

	packs, err := svc.LoadPacks(heyHome)
	if err != nil {
		return err
	}
	svcDir, err := home.SvcDir()
	if err != nil {
		return err
	}
	cat, err := hub.BuildCatalog(svcDir, packs, cfg, info.Port, version)
	if err != nil {
		return err
	}
	if len(cat.Services) == 0 {
		fmt.Println("  services: none up yet")
		return nil
	}
	fmt.Printf("  %-10s %-9s %-14s %s\n", "SERVICE", "HEALTH", "PROTOCOL", "HUB URL")
	for _, s := range cat.Services {
		fmt.Printf("  %-10s %-9s %-14s %s\n", s.Name, s.Health, s.Protocol, firstNonEmpty(s.HubURL, s.BridgeURL))
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
