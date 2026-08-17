package svc

import (
	"fmt"
	"net"
	"net/http"
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

// ModelFetcher ensures the model-server artifact described by url/sha256 is
// present at destPath, downloading it (resumably) if not. progress, if
// non-nil, reports bytes-so-far/total periodically — first-run UX for a
// multi-GB model download (docs/design/services.md §8). The default
// (HTTPSModelFetcher) verifies the digest and content-addresses the result;
// tests inject a local one to drive the lifecycle with no network.
type ModelFetcher func(url, sha256, destPath string, progress func(done, total int64)) error

// HTTPSModelFetcher is the production model fetcher: a resumable, checksummed
// HTTPS download straight to destPath (the content-addressed final path —
// see EnsureModel), via internal/fetch.DownloadResumable.
func HTTPSModelFetcher(url, sha256, destPath string, progress func(done, total int64)) error {
	return fetch.DownloadResumable(url, destPath, sha256, progress)
}

// modelsDirFor returns the content-addressed model store for a given svcDir:
// <parent of svcDir>/artifacts/models, i.e. ~/.hey/artifacts/models when
// svcDir is the real ~/.hey/svc (see internal/home.ModelsDir, which resolves
// the identical path from HEY_HOME). Deriving it from svcDir rather than
// importing internal/home keeps this package's tests free to use any svcDir,
// including a bare t.TempDir(), with no HEY_HOME wiring.
func modelsDirFor(svcDir string) string {
	return filepath.Join(filepath.Dir(svcDir), "artifacts", "models")
}

// EnsureModel makes pack.Model present on disk, downloading it into the
// content-addressed model store under modelsDir only if it isn't already
// there (dedup: the same weight referenced by two packs/instances is fetched
// once) — idempotent, so a second `hey up vinci` after a completed download
// is a fast no-op, matching the P1 UX requirement. Refuses, with a message
// naming exactly what to fix, if the pack's pin is still the obviously-fake
// PendingPinSHA256 placeholder.
func EnsureModel(modelsDir string, model *ModelArtifact, fetcher ModelFetcher, progress func(done, total int64)) (string, error) {
	if model == nil {
		return "", fmt.Errorf("pack has no model artifact configured")
	}
	if model.Pending() {
		return "", fmt.Errorf(
			"model artifact not pinned yet: packs.json still has the placeholder "+
				"url/sha256 for %q — replace them with a verified upstream URL and its "+
				"real sha256 (see docs/design/services.md §8); refusing to download an "+
				"unverified placeholder", model.Filename)
	}
	if fetcher == nil {
		fetcher = HTTPSModelFetcher
	}
	dest := filepath.Join(modelsDir, model.SHA256[:2], model.SHA256, model.Filename)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil // already have it, byte-identical by content address
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := fetcher(model.URL, model.SHA256, dest, progress); err != nil {
		return "", fmt.Errorf("ensure model %s: %w", model.Filename, err)
	}
	return dest, nil
}

// Up provisions (if needed), starts, and health-checks an instance. It is
// idempotent: a healthy running instance is reused; a stopped one is started;
// an absent one is provisioned from scratch. fetcher may be nil (defaults to
// HTTPSFetcher).
func Up(svcDir, name string, pack Pack, version string, fetcher Fetcher) (*Instance, error) {
	return up(svcDir, name, pack, version, "", fetcher, nil, nil)
}

// UpModel is Up for model-server packs: it additionally resolves and ensures
// a model artifact (see Pack.ResolveModel, EnsureModel) before Start runs.
// tier selects a Pack.Models entry ("" = the pack's DefaultModel, or its
// single Pack.Model); an existing instance provisioned under a *different*
// tier is refused rather than silently ignoring the flag — switching tiers
// needs `hey down` first, since one instance can only run one model at a
// time. modelFetcher/progress may be nil — tests supply a local modelFetcher
// to drive the full lifecycle with no multi-GB network download; production
// leaves it nil for HTTPSModelFetcher.
func UpModel(svcDir, name string, pack Pack, version, tier string, fetcher Fetcher, modelFetcher ModelFetcher, progress func(done, total int64)) (*Instance, error) {
	return up(svcDir, name, pack, version, tier, fetcher, modelFetcher, progress)
}

func up(svcDir, name string, pack Pack, version, tier string, fetcher Fetcher, modelFetcher ModelFetcher, progress func(done, total int64)) (*Instance, error) {
	if fetcher == nil {
		fetcher = HTTPSFetcher
	}
	dir := filepath.Join(svcDir, name)
	hasModel := pack.Model != nil || len(pack.Models) > 0

	// Reuse or resume an existing instance.
	if inst, err := LoadInstance(dir); err == nil {
		if hasModel && tier != "" && inst.ModelTier != "" && tier != inst.ModelTier {
			return nil, fmt.Errorf(
				"%s is already provisioned with model tier %q; `hey down %s` first to switch to %q",
				name, inst.ModelTier, name, tier)
		}
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
	if art.Pending() {
		return nil, fmt.Errorf(
			"pack %q %s/%s artifact not pinned yet: packs.json still has the "+
				"placeholder url/sha256 — replace them with a verified upstream "+
				"release URL and its real sha256 (see docs/design/services.md §8); "+
				"refusing to download an unverified placeholder", pack.Pack, version, plat)
	}

	var model *ModelArtifact
	var resolvedTier string
	if hasModel {
		model, resolvedTier, err = pack.ResolveModel(tier)
		if err != nil {
			return nil, err
		}
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
	if model != nil {
		modelPath, err := EnsureModel(modelsDirFor(svcDir), model, modelFetcher, progress)
		if err != nil {
			return nil, err
		}
		inst.ModelPath = modelPath
		inst.ModelTier = resolvedTier
		if err := inst.Save(); err != nil {
			return nil, err
		}
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
//
// Resource policy applied here, before anything is spawned:
//   - single instance: a live recorded PID is reused (idempotent), and a port
//     that is already bound — by an orphan from a crash, or by a foreign
//     process — refuses the start instead of piling a second server on top;
//   - model-server packs get a pre-launch RAM check (preflightModelRAM) so a
//     multi-GB model is never loaded into a machine that can't hold it;
//   - the child is stamped with proc.DaemonChildEnv, so even a misconfigured
//     pack whose start command resolves to hey itself can never re-exec into
//     a spawn loop (proc.GuardReexec trips in the child);
//   - the PID is recorded (inst.Save) immediately; if recording fails, the
//     just-spawned process is killed rather than left as an untracked orphan
//     `hey down` could never find.
func Start(inst *Instance, pack Pack) error {
	if inst.PID != 0 && proc.Alive(inst.PID) {
		return nil
	}
	if inst.Port != 0 && !portFree(inst.Port) {
		return fmt.Errorf(
			"start %s: port 127.0.0.1:%d is already in use, but no live PID is recorded for this instance — "+
				"refusing to start a second copy. Something else (possibly an orphaned earlier %s) holds the port; "+
				"free it, then retry",
			inst.Name, inst.Port, inst.Pack)
	}
	if pack.Driver == DriverModelServer && inst.ModelPath != "" {
		model, _, _ := pack.ResolveModel(inst.ModelTier)
		if err := preflightModelRAM(inst.Name, inst.ModelPath, model); err != nil {
			return err
		}
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
	proc.MarkDaemonChild(cmd, "svc:"+inst.Name)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start %s: %w", inst.Name, err)
	}
	logFile.Close()
	inst.PID = cmd.Process.Pid
	inst.State = StateRunning
	inst.Started = time.Now()
	go cmd.Wait() // reap so liveness checks don't see a zombie
	if err := inst.Save(); err != nil {
		// An unrecorded PID is an orphan `hey down` can never find — kill the
		// process we just spawned rather than leak it.
		_ = proc.KillTree(cmd.Process.Pid)
		return fmt.Errorf("start %s: recording pid failed (%w); the spawned process was killed to avoid an orphan", inst.Name, err)
	}
	return nil
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

var readyHTTPClient = &http.Client{Timeout: 2 * time.Second}

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
	if pack.Ready.HTTP != "" {
		url := rep.Replace(pack.Ready.HTTP)
		resp, err := readyHTTPClient.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
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
