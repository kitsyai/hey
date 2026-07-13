package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/browser"
	"github.com/kitsyai/hey/internal/contract"
	"github.com/kitsyai/hey/internal/deploy"
	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/proc"
	"github.com/kitsyai/hey/internal/state"
)

type runOpts struct {
	registryOverride string
	noBrowser        bool
	timeout          time.Duration
}

// cmdRun handles both `hey run <app> ...` and the implicit `hey <app> ...`.
// hey's own flags must come before the app name; everything after it belongs
// to the app.
func cmdRun(args []string) error {
	o := runOpts{timeout: 30 * time.Second}
	d := deployOpts{timeout: 30 * time.Second}
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "--registry":
			if i+1 >= len(args) {
				return fmt.Errorf("--registry needs a value")
			}
			i++
			o.registryOverride = args[i]
		case "--no-browser":
			o.noBrowser = true
		case "--temp":
			d.temp = true
		case "--allow-untrusted":
			d.allowUntrusted = true
		case "--channel":
			if i+1 >= len(args) {
				return fmt.Errorf("--channel needs a value")
			}
			i++
			d.channel = args[i]
		case "--location":
			if i+1 >= len(args) {
				return fmt.Errorf("--location needs a value")
			}
			i++
			d.location = args[i]
		case "--timeout":
			if i+1 >= len(args) {
				return fmt.Errorf("--timeout needs a value (e.g. 45s)")
			}
			i++
			dur, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("bad --timeout: %w", err)
			}
			o.timeout = dur
			d.timeout = dur
		default:
			goto appName
		}
	}
appName:
	if i >= len(args) {
		usage()
		return exitCodeError(2)
	}

	// Route deploy refs (@scope/id or a direct https manifest URL) to the
	// hey.deploy.v1 install/run path; bare names stay on the legacy
	// github-release path so guten/djin keep working unchanged.
	if ref, err := deploy.ParseRef(args[i]); err == nil && ref.Kind != deploy.RefAppName {
		d.registryOverride = o.registryOverride
		d.noBrowser = o.noBrowser
		return runDeployRef(ref, args[i+1:], d)
	} else if err != nil && (strings.HasPrefix(args[i], "@") || strings.HasPrefix(args[i], "http")) {
		return err
	}

	name, pinned := splitAppRef(args[i])
	appArgs := args[i+1:]

	reg, err := loadRegistry(o.registryOverride)
	if err != nil {
		return err
	}
	app, err := lookupApp(reg, name)
	if err != nil {
		return err
	}
	version, err := resolveVersion(name, app, pinned, false)
	if err != nil {
		return err
	}
	binPath, err := ensureInstalled(name, app, version)
	if err != nil {
		return err
	}

	if len(appArgs) > 0 && app.IsUICommand(appArgs[0]) {
		return runUI(name, version, binPath, appArgs, o)
	}
	return runPassthrough(binPath, appArgs)
}

// runPassthrough executes the app in the foreground with inherited stdio and
// propagates its exit code.
func runPassthrough(binPath string, args []string) error {
	cmd := exec.Command(binPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	stopForwarding := forwardSignals(cmd)
	defer stopForwarding()
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return exitCodeError(ee.ExitCode())
	}
	return err
}

// runUI starts a long-running UI app detached, waits for the contract
// handshake, records it in state, and opens the browser.
func runUI(name, version, binPath string, appArgs []string, o runOpts) error {
	stateDir, err := home.StateDir()
	if err != nil {
		return err
	}

	// Already running and healthy? Reuse it.
	if existing, ok, err := state.Get(stateDir, name); err == nil && ok {
		if proc.Alive(existing.PID) && contract.Healthy(existing.URL) {
			fmt.Printf("%s already running at %s (pid %d)\n", name, existing.URL, existing.PID)
			openBrowser(existing.URL, o.noBrowser)
			return nil
		}
		_ = state.Remove(stateDir, name)
	}

	logsDir, err := home.LogsDir()
	if err != nil {
		return err
	}
	logPath := logsDir + string(os.PathSeparator) + name + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	args := appArgs
	if !hasFlag(args, "--port") {
		args = append(args, "--port", "0")
	}
	if !hasFlag(args, "--json") {
		args = append(args, "--json")
	}

	cmd := exec.Command(binPath, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), "HEY=1", fmt.Sprintf("HEY_CONTRACT=%d", contract.Version))
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start %s: %w", name, err)
	}
	logFile.Close() // the child holds its own handle
	pid := cmd.Process.Pid
	go cmd.Wait() // reap so liveness checks don't see a zombie

	h, logTail, err := contract.WaitHandshakeFromLog(logPath, o.timeout,
		func() bool { return proc.Alive(pid) })
	if err != nil {
		_ = proc.KillTree(pid)
		if logTail != "" {
			fmt.Fprintf(os.Stderr, "--- %s log tail ---\n%s\n", name, logTail)
		}
		return fmt.Errorf("start %s ui: %w (full log: %s)", name, err, logPath)
	}
	if err := contract.WaitHealthy(h.URL, o.timeout); err != nil {
		_ = proc.KillTree(pid)
		return fmt.Errorf("start %s ui: %w", name, err)
	}

	entry := state.Proc{
		App: name, Version: version, PID: pid, Port: h.Port,
		URL: h.URL, Started: time.Now(), Log: logPath,
	}
	if err := state.Put(stateDir, entry); err != nil {
		return err
	}

	fmt.Printf("%s running at %s (pid %d)\n", name, h.URL, pid)
	openBrowser(h.URL, o.noBrowser)
	return nil
}

// browserOpen is the browser launcher, a var so tests can capture the URL
// instead of spawning a real browser.
var browserOpen = browser.Open

func openBrowser(url string, skip bool) {
	if skip {
		return
	}
	if err := browserOpen(url); err != nil {
		fmt.Fprintf(os.Stderr, "hey: could not open browser (%v) — open %s yourself\n", err, url)
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
