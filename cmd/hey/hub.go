package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
func startHubDetached() error {
	_, _, port, err := hubHandler()
	if err != nil {
		return err
	}
	if hub.ServerUp(port) {
		return fmt.Errorf("start: port %d is already in use by another process (not a hey hub)", port)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Guard against a real landmine hit while building this feature: inside a
	// `go test`-compiled binary, os.Executable() resolves to the *test*
	// binary (a go-build tmp path, or a name ending in "_test"/".test"), not
	// a real `hey`. Exec'ing it with "hub __serve-detached" doesn't run
	// cmd/hey's dispatch at all — Go's flag package silently ignores unknown
	// positional args, so the child just runs the *entire test suite again*,
	// which (if anything in it also reaches this code path) forks again, and
	// again. Refusing here — once, at the source — is cheap insurance against
	// that class of runaway recursive spawn for every test in this binary,
	// not just the ones that happen to be careful about it today.
	if looksLikeTestBinary(exe) {
		return fmt.Errorf("start: refusing to spawn a detached hub from what looks like a test binary (%s) — "+
			"tests must pre-start the hub in-process instead of exercising this path", exe)
	}
	cmd := exec.Command(exe, "hub", "__serve-detached")
	proc.Detach(cmd)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	for i := 0; i < 50 && !hub.ServerUp(port); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !hub.ServerUp(port) {
		return fmt.Errorf("start: hub did not come up on port %d", port)
	}
	fmt.Printf("hey hub started at http://127.0.0.1:%d/ (pid %d) — stop with: hey hub stop\n", port, cmd.Process.Pid)
	return nil
}

// looksLikeTestBinary heuristically detects a `go test`-compiled executable
// by its path/name shape: `go test` builds into a `go-buildNNNN` temp
// directory, and/or names the binary `<pkg>.test`/`<pkg>.test.exe`. Neither
// signal is foolproof (a renamed real binary could false-positive; unlikely
// in practice), but the cost of a false positive is a clear error telling a
// test to fix itself, versus the cost of a false negative being exactly the
// runaway recursive-spawn this guard exists to prevent.
func looksLikeTestBinary(exe string) bool {
	base := filepath.Base(exe)
	if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return true
	}
	return strings.Contains(exe, "go-build")
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
