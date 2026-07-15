package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/deploy"
	"github.com/kitsyai/hey/internal/gh"
	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/keeper"
	"github.com/kitsyai/hey/internal/source"
)

// installShim writes a launcher next to the hey executable (that directory is
// on PATH from the installer) so a source-installed tool runs directly as
// `<id> <args>` — it delegates to `hey runner run <id>`, which resolves the
// current installed version, so the shim never goes stale across updates.
func installShim(id string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(self)
	if runtime.GOOS == "windows" {
		shim := filepath.Join(dir, id+".cmd")
		content := "@echo off\r\n\"" + self + "\" runner run " + id + " %*\r\n"
		return os.WriteFile(shim, []byte(content), 0o755)
	}
	shim := filepath.Join(dir, id)
	content := "#!/bin/sh\nexec \"" + self + "\" runner run " + id + " \"$@\"\n"
	return os.WriteFile(shim, []byte(content), 0o755)
}

// removeShim deletes a tool's PATH shim (best-effort, used by `hey remove`).
func removeShim(id string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(self)
	for _, name := range []string{id, id + ".cmd"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// cmdBuddy is the errand module: fetch/install native bundles and clone source.
// buddy never builds by default and never knows a tool's internals — it moves
// bytes per a manifest (install) or per git (clone). Private sources are
// authenticated with a named credential resolved from keeper (--cred <name>).
func cmdBuddy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hey buddy <install|update|clone> ...")
	}
	switch args[0] {
	case "install":
		return buddyInstall(args[1:])
	case "update":
		return buddyUpdate(args[1:])
	case "clone":
		return buddyClone(args[1:])
	default:
		return fmt.Errorf("unknown buddy subcommand %q (use install|update|clone)", args[0])
	}
}

// buddyUpdate refreshes buddy-installed (source) bundles to whatever their
// repo's hey.json now points to. With an id it updates that one; with no id it
// updates every source bundle. `hey update <id>` reaches the same code, so both
// spellings work.
func buddyUpdate(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: hey buddy update [<id>]")
	}
	if len(args) == 1 {
		id := args[0]
		m, ok, err := readMeta(id)
		if err != nil {
			return err
		}
		if !ok || m.Kind != "source" {
			return fmt.Errorf("%q is not a buddy-installed tool — see `hey ls`", id)
		}
		return buddySourceInstall(m.Repo, m.Cred)
	}
	updated := false
	for _, b := range installedBundles() {
		if b.Kind != "source" {
			continue
		}
		updated = true
		if err := buddySourceInstall(b.Repo, b.Cred); err != nil {
			fmt.Fprintf(os.Stderr, "hey: skip %s: %v\n", b.ID, err)
		}
	}
	if !updated {
		fmt.Println("no buddy-installed tools yet — try `hey buddy install <owner/repo>`")
	}
	return nil
}

// buddyInstall fetches + installs a deployed bundle (@scope/id or an https
// manifest URL), authenticating the manifest and artifact fetches with a keeper
// credential when --cred is given (private releases). It never launches.
func buddyInstall(args []string) error {
	var ref, cred string
	o := deployOpts{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cred":
			cred, i = nextVal(args, i)
		case "--channel":
			o.channel, i = nextVal(args, i)
		case "--registry":
			o.registryOverride, i = nextVal(args, i)
		case "--location":
			o.location, i = nextVal(args, i)
		case "--allow-untrusted":
			o.allowUntrusted = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q", args[i])
			}
			if ref != "" {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
			ref = args[i]
		}
	}
	if ref == "" {
		return fmt.Errorf("usage: hey buddy install <owner/repo|@scope/id|https-manifest-url> [--cred <name>]")
	}
	// An owner/repo ref (e.g. acme/tool) is a source install: read the repo's
	// checked-in hey.json and fetch the prebuilt binary for this platform.
	if isRepoRef(ref) {
		return buddySourceInstall(ref, cred)
	}
	if cred != "" {
		tok, err := keeper.Get(cred)
		if err != nil {
			return err
		}
		o.token = tok
	}
	parsed, err := deploy.ParseRef(ref)
	if err != nil {
		return err
	}
	if parsed.Kind == deploy.RefAppName {
		return fmt.Errorf("buddy install takes a repo (owner/repo), a deploy ref (@scope/id), or an https manifest URL, not %q", ref)
	}
	return installDeployRef(parsed, o)
}

// isRepoRef reports whether ref is a bare owner/repo (not a URL, not @scoped).
func isRepoRef(ref string) bool {
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "@") {
		return false
	}
	parts := strings.Split(ref, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// buddySourceInstall installs a tool from its own repo per an in-repo
// hey.source.v1 manifest: fetch hey.json (authenticated via keeper), pick the
// prebuilt binary for this platform, verify it, install it, record the bundle,
// and drop a PATH shim so the tool runs as `<id> <args>` and `hey runner run
// <id>`. It fetches only the manifest and the one binary — no monorepo clone.
func buddySourceInstall(repo, cred string) error {
	var token string
	if cred != "" {
		tok, err := keeper.Get(cred)
		if err != nil {
			return err
		}
		token = tok
	}

	fmt.Fprintf(os.Stderr, "hey: reading %s hey.json\n", repo)
	data, err := gh.FetchContent(repo, "hey.json", "", token)
	if err != nil {
		return err
	}
	m, err := source.Parse(data)
	if err != nil {
		return err
	}
	pb, ok := m.PrebuiltFor()
	if !ok {
		return fmt.Errorf("%s has no prebuilt binary for %s (offers: %s); build-from-source is not yet supported by buddy v0",
			m.ID, source.Platform(), strings.Join(m.Platforms(), ", "))
	}

	// Already at this version with the exact same binary? Skip the download.
	// A rebuilt binary at the same version (new sha) still updates — so a
	// producer can either bump the manifest version or just push a new binary.
	if existing, ok, _ := readMeta(m.ID); ok && existing.Kind == "source" &&
		existing.Current == m.Version && installedMatchesSHA(existing, m.Version, pb.SHA256) {
		fmt.Printf("%s is already up to date (%s)\n", m.ID, m.Version)
		return nil
	}

	fmt.Fprintf(os.Stderr, "hey: fetching %s (%s)\n", pb.Path, source.Platform())
	bin, err := gh.FetchContent(repo, pb.Path, "", token)
	if err != nil {
		return err
	}
	if pb.SHA256 != "" {
		got := fmt.Sprintf("%x", sha256.Sum256(bin))
		if !strings.EqualFold(got, pb.SHA256) {
			return fmt.Errorf("checksum mismatch for %s:\n  want %s\n  got  %s", pb.Path, pb.SHA256, got)
		}
	}

	dir, err := home.DeployAppDir(m.ID, m.Version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	execName := m.ID + exeExt()
	dest := filepath.Join(dir, execName)
	if err := os.WriteFile(dest, bin, 0o755); err != nil {
		return err
	}

	meta := bundleMeta{
		ID: m.ID, Kind: "source", Repo: repo, Cred: cred,
		Exec: execName, Current: m.Version, Enabled: true, Updated: time.Now(),
	}
	if existing, ok, _ := readMeta(m.ID); ok {
		meta.Enabled = existing.Enabled
	}
	if err := writeMeta(meta); err != nil {
		return err
	}
	if err := installShim(m.ID); err != nil {
		fmt.Fprintf(os.Stderr, "hey: warning — could not create the `%s` shim (%v); use `hey runner run %s`\n", m.ID, err, m.ID)
	}
	fmt.Printf("%s %s -> %s\n", m.ID, m.Version, dest)
	fmt.Printf("run it with `%s` or `hey runner run %s`\n", m.ID, m.ID)
	return nil
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// installedMatchesSHA reports whether the on-disk installed executable matches
// wantSHA. With no expected sha we can't confirm, so we report false (reinstall)
// rather than risk skipping a real change.
func installedMatchesSHA(meta bundleMeta, version, wantSHA string) bool {
	if wantSHA == "" {
		return false
	}
	dir, err := home.DeployAppDir(meta.ID, version)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, meta.Exec))
	if err != nil {
		return false
	}
	return strings.EqualFold(fmt.Sprintf("%x", sha256.Sum256(data)), wantSHA)
}

// buddyClone does an authenticated git clone, and — only when --build is given
// explicitly — runs that build command in the clone. hey never builds on its
// own; the toolchain must be present and the command is the caller's.
func buddyClone(args []string) error {
	var repo, dest, cred, build string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cred":
			cred, i = nextVal(args, i)
		case "--build":
			build, i = nextVal(args, i)
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q", args[i])
			}
			switch {
			case repo == "":
				repo = args[i]
			case dest == "":
				dest = args[i]
			default:
				return fmt.Errorf("unexpected argument %q", args[i])
			}
		}
	}
	if repo == "" {
		return fmt.Errorf("usage: hey buddy clone <owner/name|https-url> [dest] [--cred <name>] [--build \"<cmd>\"]")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is not installed — buddy clone needs git on PATH")
	}
	url := gitURL(repo)
	if dest == "" {
		dest = repoDir(repo)
	}

	gitArgs := []string{}
	if cred != "" {
		tok, err := keeper.Get(cred)
		if err != nil {
			return err
		}
		// One-shot -c: the token authenticates this clone but is not written to
		// the clone's .git/config, so it never persists to disk.
		gitArgs = append(gitArgs, "-c", "http.extraHeader=Authorization: Bearer "+tok)
	}
	gitArgs = append(gitArgs, "clone", url, dest)

	fmt.Fprintf(os.Stderr, "hey: cloning %s -> %s\n", url, dest)
	if err := runInDir("", exec.Command("git", gitArgs...)); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	fmt.Printf("cloned %s -> %s\n", repo, dest)

	if build == "" {
		return nil
	}
	fmt.Fprintf(os.Stderr, "hey: building (explicit --build): %s\n", build)
	if err := runInDir(dest, shellCmd(build)); err != nil {
		return fmt.Errorf("build %q: %w", build, err)
	}
	fmt.Printf("built %s\n", dest)
	return nil
}

// gitURL turns owner/name into an https GitHub URL; a full https/ssh URL is
// used as-is.
func gitURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
}

// repoDir is the default clone directory: the repo's last path segment.
func repoDir(repo string) string {
	r := strings.TrimSuffix(repo, ".git")
	if i := strings.LastIndexAny(r, "/:"); i >= 0 {
		r = r[i+1:]
	}
	return r
}

// shellCmd wraps a build command for the platform shell so pipes/&& work.
func shellCmd(cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", cmd)
	}
	return exec.Command("sh", "-c", cmd)
}

// runInDir runs c (optionally in dir) with inherited stdio.
func runInDir(dir string, c *exec.Cmd) error {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		c.Dir = abs
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// nextVal returns the value after a flag at index i and the advanced index.
func nextVal(args []string, i int) (string, int) {
	if i+1 >= len(args) {
		return "", i
	}
	return args[i+1], i + 1
}
