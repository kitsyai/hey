package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/pkgreg"
	"github.com/kitsyai/hey/internal/pkgreg/npm"
	"github.com/kitsyai/hey/internal/proc"
)

const registryDefaultPort = 4874

const registryUsage = `hey registry — local package registry (npm today; more formats later)

Publishes and reads overlay a real upstream: local WIP versions win, and
everything else (metadata + tarballs, auth-injected) is proxied through so
teammates get a real @scope, not a fake one. Loopback (127.0.0.1) only.

Usage:
  hey registry start  [--port 4874] [--store <dir>]   start in the background (pidfile-tracked)
  hey registry serve  [--port 4874] [--store <dir>]   run in the foreground
  hey registry stop                                   stop the background server
  hey registry status                                 running?, port, store, published count
  hey registry ls                                     list locally-published package@version
  hey registry clear  [<pkg>[@<version>]]              drop local versions (all / one pkg / one version)

Team flow:
  hey self update; hey registry start
  (family repo)  pnpm publish --registry http://127.0.0.1:4874   # 0.x.y-dev.N
  (app repo)     point the @scope registry at 127.0.0.1:4874 for the session, install, test
  hey registry stop   -> back to the real registry

Default store: ~/.hey/registry/<format>/... (HEY_HOME to relocate). Upstream
resolution reads ~/.npmrc scope registries and tokenHelper executables —
never token/authToken lines.
`

func cmdRegistry(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, registryUsage)
		return exitCodeError(2)
	}
	switch args[0] {
	case "start":
		return registryStart(args[1:])
	case "serve":
		return registryServe(args[1:], false)
	case "stop":
		return registryStop(args[1:])
	case "status":
		return registryStatus(args[1:])
	case "ls":
		return registryLs(args[1:])
	case "clear":
		return registryClear(args[1:])
	case "__serve-detached":
		// internal: the detached child spawned by `hey registry start`
		return registryServe(args[1:], true)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, registryUsage)
		return nil
	default:
		return fmt.Errorf("unknown registry subcommand %q\n%s", args[0], registryUsage)
	}
}

// registryFlags parses the --port/--store flags shared by start/serve, and
// returns any remaining positional args.
func registryFlags(args []string) (port int, store string, rest []string, err error) {
	port = registryDefaultPort
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return 0, "", nil, fmt.Errorf("--port needs a value")
			}
			p, e := strconv.Atoi(args[i+1])
			if e != nil {
				return 0, "", nil, fmt.Errorf("bad --port: %w", e)
			}
			port = p
			i++
		case "--store":
			if i+1 >= len(args) {
				return 0, "", nil, fmt.Errorf("--store needs a value")
			}
			store = args[i+1]
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	return port, store, rest, nil
}

// resolveRegistryStore returns the store root to use: the --store override
// (made absolute, created if needed) or ~/.hey/registry.
func resolveRegistryStore(override string) (string, error) {
	if override == "" {
		return home.PkgRegistryDir()
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

// newRegistryHandler builds the mux for every registered format. npm is the
// first and, for now, only format — see internal/pkgreg/npm.Mount for why it
// owns the mux root.
func newRegistryHandler(core *pkgreg.Core) (http.Handler, error) {
	npmrcPath, err := npm.DefaultNpmrcPath()
	if err != nil {
		return nil, err
	}
	npmFmt, err := npm.New(npmrcPath)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	npmFmt.Mount(mux, core)
	return pkgreg.LoggingMiddleware(mux), nil
}

func registryServe(args []string, detached bool) error {
	port, storeOverride, _, err := registryFlags(args)
	if err != nil {
		return err
	}
	store, err := resolveRegistryStore(storeOverride)
	if err != nil {
		return err
	}
	core, err := pkgreg.NewCore(store, port)
	if err != nil {
		return err
	}
	handler, err := newRegistryHandler(core)
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	if detached {
		if err := pkgreg.WritePidFile(store, pkgreg.PidInfo{PID: os.Getpid(), Port: port, Store: store}); err != nil {
			return err
		}
		defer pkgreg.RemovePidFile(store)
	}
	fmt.Printf("hey registry: serving npm at http://%s/ (store: %s)\n", addr, store)
	return http.ListenAndServe(addr, handler)
}

func registryStart(args []string) error {
	port, storeOverride, _, err := registryFlags(args)
	if err != nil {
		return err
	}
	store, err := resolveRegistryStore(storeOverride)
	if err != nil {
		return err
	}

	if info, ok := pkgreg.ReadPidFile(store); ok && pkgreg.ServerUp(info.Port) {
		fmt.Printf("hey registry already running at http://127.0.0.1:%d/ (pid %d, store %s)\n", info.Port, info.PID, info.Store)
		return nil
	}
	if pkgreg.ServerUp(port) {
		return fmt.Errorf("start: port %d is already in use by another process", port)
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	childArgs := []string{"registry", "__serve-detached", "--port", strconv.Itoa(port), "--store", store}
	cmd := exec.Command(exe, childArgs...)
	proc.Detach(cmd)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	for i := 0; i < 30 && !pkgreg.ServerUp(port); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if !pkgreg.ServerUp(port) {
		return fmt.Errorf("start: server did not come up on port %d", port)
	}
	fmt.Printf("hey registry started at http://127.0.0.1:%d/ (pid %d, store %s) — stop with: hey registry stop\n",
		port, cmd.Process.Pid, store)
	return nil
}

func registryStop(args []string) error {
	_, storeOverride, _, err := registryFlags(args)
	if err != nil {
		return err
	}
	store, err := resolveRegistryStore(storeOverride)
	if err != nil {
		return err
	}
	info, ok := pkgreg.ReadPidFile(store)
	if !ok {
		fmt.Println("hey registry: no background server recorded")
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
	pkgreg.RemovePidFile(store)
	fmt.Printf("hey registry stopped (pid %d)\n", info.PID)
	return nil
}

func registryStatus(args []string) error {
	_, storeOverride, _, err := registryFlags(args)
	if err != nil {
		return err
	}
	store, err := resolveRegistryStore(storeOverride)
	if err != nil {
		return err
	}
	info, ok := pkgreg.ReadPidFile(store)
	running := ok && pkgreg.ServerUp(info.Port)

	if !running {
		fmt.Println("hey registry: not running")
		fmt.Printf("  store: %s\n", store)
	} else {
		fmt.Printf("hey registry: running (pid %d)\n", info.PID)
		fmt.Printf("  port:  %d\n", info.Port)
		fmt.Printf("  store: %s\n", store)
	}

	entries, err := (&npm.Format{}).List(store)
	if err != nil {
		return err
	}
	pkgs := map[string]bool{}
	for _, e := range entries {
		pkgs[e.Package] = true
	}
	fmt.Printf("  npm:   %d package(s), %d version(s) published locally\n", len(pkgs), len(entries))
	return nil
}

func registryLs(args []string) error {
	_, storeOverride, _, err := registryFlags(args)
	if err != nil {
		return err
	}
	store, err := resolveRegistryStore(storeOverride)
	if err != nil {
		return err
	}
	entries, err := (&npm.Format{}).List(store)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no locally-published versions — try `pnpm publish --registry http://127.0.0.1:4874`")
		return nil
	}
	for _, e := range entries {
		fmt.Printf("%s@%s\n", e.Package, e.Version)
	}
	return nil
}

func registryClear(args []string) error {
	_, storeOverride, rest, err := registryFlags(args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("usage: hey registry clear [<pkg>[@<version>]]")
	}
	store, err := resolveRegistryStore(storeOverride)
	if err != nil {
		return err
	}
	var pkg, version string
	if len(rest) == 1 {
		pkg, version = splitPkgVersion(rest[0])
	}
	n, err := (&npm.Format{}).Clear(store, pkg, version)
	if err != nil {
		return err
	}
	fmt.Printf("cleared %d version(s)\n", n)
	return nil
}

// splitPkgVersion splits "pkg@version" (scoped-aware: "@scope/pkg@version")
// into name and version. The scope's own leading "@" is not a separator —
// only a later "@" is.
func splitPkgVersion(s string) (pkg, version string) {
	if idx := strings.LastIndex(s, "@"); idx > 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}
