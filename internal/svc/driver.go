package svc

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/fetch"
	"github.com/kitsyai/hey/internal/proc"
)

// Fetcher downloads the artifact at url, verifies it against sha256, and
// extracts it into destDir. The default (HTTPSFetcher) reuses hey's HTTPS +
// SHA-256 + zip-slip-safe pipeline; tests inject a local one to drive the full
// lifecycle with no network.
type Fetcher func(url, sha256, destDir string) error

// HTTPSFetcher is the production fetcher: download → verify → extract, all via
// internal/fetch.
func HTTPSFetcher(url, sha256, destDir string) error {
	tmp, got, err := fetch.Download(url, destDir)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := fetch.Verify(url, got, map[string]string{url: sha256}, nil); err != nil {
		return err
	}
	return fetch.ExtractTree(tmp, destDir)
}

const initMarker = ".hey-initialized"

// Up provisions (if needed), starts, and health-checks an instance. It is
// idempotent: a healthy running instance is reused; a stopped one is started;
// an absent one is provisioned from scratch. fetcher may be nil (defaults to
// HTTPSFetcher).
func Up(svcDir, name string, pack Pack, version string, fetcher Fetcher) (*Instance, error) {
	if fetcher == nil {
		fetcher = HTTPSFetcher
	}
	dir := filepath.Join(svcDir, name)

	// Reuse or resume an existing instance.
	if inst, err := LoadInstance(dir); err == nil {
		if inst.PID != 0 && proc.Alive(inst.PID) {
			return inst, nil // already running
		}
		if err := Start(inst, pack); err != nil {
			return nil, err
		}
		if err := WaitReady(inst, pack); err != nil {
			return nil, err
		}
		return inst, nil
	}

	art, plat, err := pack.Artifact(version)
	if err != nil {
		return nil, err
	}

	inst := &Instance{
		Name: name, Pack: pack.Pack, Version: version, Driver: pack.Driver,
		Platform: plat, BinSubdir: art.BinSubdir, State: StateStopped,
		Created: time.Now(), dir: dir,
	}

	// Stable port, avoiding ports other instances already hold.
	used, err := usedPorts(svcDir)
	if err != nil {
		return nil, err
	}
	if inst.Port, err = allocatePort(used); err != nil {
		return nil, err
	}
	if inst.User, inst.Password, err = genCredentials(); err != nil {
		return nil, err
	}

	for _, d := range []string{inst.BinDir(), filepath.Dir(inst.LogPath())} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := inst.Save(); err != nil {
		return nil, err
	}

	if err := provision(inst, pack, art, fetcher); err != nil {
		return nil, err
	}
	if err := runInit(inst, pack); err != nil {
		return nil, err
	}
	if err := Start(inst, pack); err != nil {
		return nil, err
	}
	if err := WaitReady(inst, pack); err != nil {
		return nil, err
	}
	return inst, nil
}

// provision downloads+verifies+extracts the artifact into the instance bin dir
// unless it is already present.
func provision(inst *Instance, pack Pack, art Artifact, fetcher Fetcher) error {
	// If the executables are already there (a prior provision), skip the fetch.
	if entries, err := os.ReadDir(inst.ExeDir()); err == nil && len(entries) > 0 {
		return nil
	}
	if err := fetcher(art.URL, art.SHA256, inst.BinDir()); err != nil {
		return fmt.Errorf("provision %s: %w", inst.Name, err)
	}
	return nil
}

// runInit runs the pack's init commands exactly once (guarded by a marker in
// the data dir). Init that fails leaves no marker, so it retries next time.
func runInit(inst *Instance, pack Pack) error {
	if err := os.MkdirAll(inst.DataDir(), 0o755); err != nil {
		return err
	}
	marker := filepath.Join(inst.DataDir(), initMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if len(pack.Init) == 0 {
		return os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
	}

	// The password is handed to init via a transient 0600 file, never argv.
	pwfile := filepath.Join(inst.dir, ".pwfile")
	if err := os.WriteFile(pwfile, []byte(inst.Password), 0o600); err != nil {
		return err
	}
	defer os.Remove(pwfile)

	for _, tmpl := range pack.Init {
		argv, err := expand(tmpl, inst.vars(pwfile))
		if err != nil {
			return err
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = inst.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("init %q: %v\n%s", tmpl, err, out)
		}
	}
	return os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
}

// Start launches the pack's start command as a detached managed process, its
// output streaming to logs/service.log, and records the PID + running state.
func Start(inst *Instance, pack Pack) error {
	if inst.PID != 0 && proc.Alive(inst.PID) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(inst.LogPath()), 0o755); err != nil {
		return err
	}
	argv, err := expand(pack.Start, inst.vars(""))
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(inst.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = inst.dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start %s: %w", inst.Name, err)
	}
	logFile.Close()
	inst.PID = cmd.Process.Pid
	inst.State = StateRunning
	inst.Started = time.Now()
	go cmd.Wait() // reap so liveness checks don't see a zombie
	return inst.Save()
}

// WaitReady polls the pack's ready check until it passes or the timeout
// elapses. If the process dies while waiting, it fails fast with a log tail.
func WaitReady(inst *Instance, pack Pack) error {
	timeout := time.Duration(pack.Ready.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	rep := inst.vars("").replacer()

	for time.Now().Before(deadline) {
		if inst.PID != 0 && !proc.Alive(inst.PID) {
			return fmt.Errorf("%s exited before it became ready\n%s", inst.Name, logTail(inst))
		}
		if readyOnce(inst, pack, rep) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become ready within %s\n%s", inst.Name, timeout, logTail(inst))
}

func readyOnce(inst *Instance, pack Pack, rep *strings.Replacer) bool {
	if pack.Ready.TCP != "" {
		addr := rep.Replace(pack.Ready.TCP)
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	argv, err := expand(pack.Ready.Command, inst.vars(""))
	if err != nil {
		return false
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = inst.dir
	return cmd.Run() == nil
}

// Stop shuts the instance down: the pack's stop command if present, else a
// termination signal; after grace_seconds the whole process tree is force
// killed as a guaranteed fallback. data/ is never touched.
func Stop(inst *Instance, pack Pack) error {
	defer func() {
		inst.State = StateStopped
		inst.PID = 0
		_ = inst.Save()
	}()

	if inst.PID == 0 || !proc.Alive(inst.PID) {
		return nil
	}
	grace := time.Duration(pack.Stop.GraceSeconds) * time.Second
	if grace <= 0 {
		grace = 15 * time.Second
	}

	if pack.Stop.Command != "" {
		if argv, err := expand(pack.Stop.Command, inst.vars("")); err == nil {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir = inst.dir
			_ = cmd.Run()
		}
	} else {
		_ = proc.TermSignal(inst.PID, pack.Stop.Signal)
	}

	if waitGone(inst.PID, grace) {
		return nil
	}
	return proc.KillTree(inst.PID)
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !proc.Alive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !proc.Alive(pid)
}

// Conn returns the pack's connection string for the instance, with credentials
// and port substituted.
func Conn(inst *Instance, pack Pack) string {
	if pack.Conn == "" {
		return ""
	}
	return inst.vars("").replacer().Replace(pack.Conn)
}

// Alive reports whether the instance's process is currently running.
func Alive(inst *Instance) bool {
	return inst.PID != 0 && proc.Alive(inst.PID)
}

func logTail(inst *Instance) string {
	data, err := os.ReadFile(inst.LogPath())
	if err != nil {
		return ""
	}
	const max = 2000
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return "--- service.log tail ---\n" + string(data)
}

// ListInstances returns every provisioned instance under svcDir, sorted by name.
func ListInstances(svcDir string) ([]*Instance, error) {
	entries, err := os.ReadDir(svcDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Instance
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inst, err := LoadInstance(filepath.Join(svcDir, e.Name()))
		if err != nil {
			continue // not a valid instance dir
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func usedPorts(svcDir string) (map[int]bool, error) {
	insts, err := ListInstances(svcDir)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, in := range insts {
		used[in.Port] = true
	}
	return used, nil
}

// DirSize returns the total byte size of files under dir (best effort).
func DirSize(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
