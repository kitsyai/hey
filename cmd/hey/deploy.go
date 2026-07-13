package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/heypkv/hey/internal/deploy"
	"github.com/heypkv/hey/internal/fetch"
	"github.com/heypkv/hey/internal/home"
	"github.com/heypkv/hey/internal/proc"
)

// deployOpts carries the flags the deploy (hey.deploy.v1) install/run path
// understands. The subset overlaps runOpts on purpose so `hey run` can drive
// both a legacy github-release binary and a deployed bundle.
type deployOpts struct {
	registryOverride string
	channel          string
	temp             bool
	location         string
	noBrowser        bool
	timeout          time.Duration
}

// resolveManifest turns a classified deploy ref into a validated manifest.
// RefAppName is never passed here — those route to the legacy path.
func resolveManifest(ref deploy.Ref, o deployOpts) (*deploy.Manifest, error) {
	var url string
	switch ref.Kind {
	case deploy.RefManifestURL:
		url = ref.ManifestURL
	case deploy.RefScoped:
		reg, err := loadRegistry(o.registryOverride)
		if err != nil {
			return nil, err
		}
		url, err = reg.ManifestURL(ref.Scope, ref.ID, o.channel)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("internal: resolveManifest called with a non-manifest ref")
	}
	fmt.Fprintf(os.Stderr, "hey: resolving manifest %s\n", url)
	return deploy.Fetch(url)
}

// installDir resolves where a manifest's bundle should land and returns a
// cleanup func (non-nil only for --temp). It also reports whether an existing
// cached install can be reused.
func installDir(m *deploy.Manifest, o deployOpts) (dir string, cleanup func(), reuse bool, err error) {
	switch {
	case o.temp:
		dir, err = home.TempInstallDir()
		if err != nil {
			return "", nil, false, err
		}
		return dir, func() { _ = os.RemoveAll(dir) }, false, nil
	case o.location != "":
		abs, aerr := filepath.Abs(o.location)
		if aerr != nil {
			return "", nil, false, aerr
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", nil, false, err
		}
		return abs, nil, false, nil
	default:
		dir, err = home.DeployAppDir(m.ID, m.Version)
		if err != nil {
			return "", nil, false, err
		}
		// Reuse a cached install only if it already holds files (a prior,
		// completed extraction) — an empty version dir means fetch again.
		if entries, rerr := os.ReadDir(dir); rerr == nil && len(entries) > 0 {
			reuse = true
		}
		return dir, nil, reuse, nil
	}
}

// downloadVerify downloads art into dir (a temp file on the same volume) and
// enforces its manifest sha256. It returns the temp path; the caller removes it.
func downloadVerify(art deploy.Artifact, dir string) (string, error) {
	fmt.Fprintf(os.Stderr, "hey: fetching %s\n", art.URL)
	tmp, got, err := fetch.Download(art.URL, dir)
	if err != nil {
		return "", err
	}
	if err := fetch.Verify(art.URL, got, map[string]string{art.URL: art.SHA256}, nil); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// materialize installs art into dir per its kind and returns the executable
// path to launch (empty for kinds hey does not launch itself: installer/link).
// For a reused cache it only recomputes the exec path.
func materialize(m *deploy.Manifest, art deploy.Artifact, dir string, reuse bool) (string, error) {
	switch art.Kind {
	case deploy.KindArchive:
		exec := filepath.Join(dir, filepath.FromSlash(art.Launch.Exec))
		if reuse {
			if _, err := os.Stat(exec); err == nil {
				return exec, nil
			}
		}
		tmp, err := downloadVerify(art, filepath.Dir(dir))
		if err != nil {
			return "", err
		}
		defer os.Remove(tmp)
		if err := fetch.ExtractTree(tmp, dir); err != nil {
			return "", err
		}
		if art.Launch.Exec == "" {
			return "", fmt.Errorf("archive artifact has no launch.exec")
		}
		return exec, nil

	case deploy.KindBinary, deploy.KindAppImage:
		name := execFileName(art)
		exec := filepath.Join(dir, name)
		if reuse {
			if _, err := os.Stat(exec); err == nil {
				return exec, nil
			}
		}
		tmp, err := downloadVerify(art, dir)
		if err != nil {
			return "", err
		}
		if err := os.Rename(tmp, exec); err != nil {
			os.Remove(tmp)
			return "", err
		}
		if err := os.Chmod(exec, 0o755); err != nil {
			return "", err
		}
		return exec, nil

	case deploy.KindInstaller:
		installer := filepath.Join(dir, execFileName(art))
		tmp, err := downloadVerify(art, dir)
		if err != nil {
			return "", err
		}
		if err := os.Rename(tmp, installer); err != nil {
			os.Remove(tmp)
			return "", err
		}
		fmt.Fprintf(os.Stderr, "hey: handing installer to the OS: %s\n", installer)
		if err := openWithOS(installer); err != nil {
			return "", fmt.Errorf("launch installer: %w", err)
		}
		return "", nil // hey does not manage a native installer's lifecycle

	case deploy.KindLink:
		fmt.Fprintf(os.Stderr, "hey: opening %s\n", art.URL)
		openBrowser(art.URL, false)
		return "", nil

	case deploy.KindPackage:
		return "", fmt.Errorf("%s is a device package (%s) — install it with `hey mobile push`, not a desktop install", m.ID, art.Platform)

	default:
		return "", fmt.Errorf("unknown artifact kind %q", art.Kind)
	}
}

// execFileName derives a stable on-disk filename for a single-file artifact
// (binary/appimage/installer). It prefers launch.exec, else the URL basename.
func execFileName(art deploy.Artifact) string {
	if art.Launch.Exec != "" {
		return filepath.Base(filepath.FromSlash(art.Launch.Exec))
	}
	base := art.URL
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base == "" {
		base = art.Platform + "-" + art.Kind
	}
	return base
}

// openWithOS hands a file (installer, .dmg, .pkg) to the OS default handler.
func openWithOS(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// installDeployRef installs (but does not launch) a deployed bundle, printing
// where it landed. Used by `hey install <@scope/id|https-url>`.
func installDeployRef(ref deploy.Ref, o deployOpts) error {
	m, err := resolveManifest(ref, o)
	if err != nil {
		return err
	}
	art, err := selectForInstall(m)
	if err != nil {
		return err
	}
	dir, cleanup, reuse, err := installDir(m, o)
	if err != nil {
		return err
	}
	// A plain install is persistent; --temp makes no sense without running.
	if cleanup != nil {
		defer cleanup()
		return fmt.Errorf("--temp only applies to `hey run`")
	}
	execPath, err := materialize(m, art, dir, reuse)
	if err != nil {
		return err
	}
	switch art.Kind {
	case deploy.KindLink:
		fmt.Printf("%s %s: opened %s\n", m.ID, m.Version, art.URL)
	case deploy.KindInstaller:
		fmt.Printf("%s %s: installer handed to the OS\n", m.ID, m.Version)
	default:
		fmt.Printf("%s %s -> %s\n", m.ID, m.Version, execPath)
	}
	return nil
}

// runDeployRef installs and launches a deployed bundle. Used by
// `hey run <@scope/id|https-url>`.
func runDeployRef(ref deploy.Ref, appArgs []string, o deployOpts) error {
	m, err := resolveManifest(ref, o)
	if err != nil {
		return err
	}
	art, err := selectForInstall(m)
	if err != nil {
		return err
	}
	dir, cleanup, reuse, err := installDir(m, o)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}
	execPath, err := materialize(m, art, dir, reuse)
	if err != nil {
		return err
	}
	if execPath == "" {
		return nil // installer/link: nothing for hey to launch
	}
	return launchDeploy(m, art, execPath, appArgs, o)
}

// selectForInstall chooses the artifact for the current desktop machine and
// rejects mobile-only manifests with a pointer to `hey mobile`.
func selectForInstall(m *deploy.Manifest) (deploy.Artifact, error) {
	art, err := m.SelectDesktop()
	if err == nil {
		return art, nil
	}
	// No desktop artifact — a link-only manifest (e.g. a store page) is still
	// runnable by opening it. Otherwise surface the desktop no-match error.
	if link, lerr := m.SelectLink(""); lerr == nil {
		return link, nil
	}
	return deploy.Artifact{}, err
}

// launchDeploy runs execPath per the artifact's interface.
//
//   - hey-contract: reuse runUI — the port handshake, health check, browser.
//   - window (default): a self-windowing GUI. With --temp we run it in the
//     foreground so the ephemeral dir stays valid for the app's whole life and
//     is cleaned up when it exits; otherwise we launch detached and return.
func launchDeploy(m *deploy.Manifest, art deploy.Artifact, execPath string, appArgs []string, o deployOpts) error {
	args := append(append([]string{}, art.Launch.Args...), appArgs...)

	if art.Interface == deploy.InterfaceHeyContract {
		return runUI(m.ID, m.Version, execPath, args, runOpts{
			registryOverride: o.registryOverride,
			noBrowser:        o.noBrowser,
			timeout:          o.timeout,
		})
	}

	// window (or unspecified interface): launch the GUI.
	cmd := launchCmd(execPath, args)
	if o.temp {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Fprintf(os.Stderr, "hey: running %s %s (ephemeral)\n", m.ID, m.Version)
		err := cmd.Run() // block for the app's lifetime; temp dir cleaned on return
		if ee, ok := err.(*exec.ExitError); ok {
			return exitCodeError(ee.ExitCode())
		}
		return err
	}
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch %s: %w", m.ID, err)
	}
	go cmd.Wait()
	fmt.Printf("%s %s launched (pid %d)\n", m.ID, m.Version, cmd.Process.Pid)
	return nil
}

// launchCmd builds the command to run a window/binary exec, resolving a macOS
// .app bundle through `open`.
func launchCmd(execPath string, args []string) *exec.Cmd {
	if runtime.GOOS == "darwin" && strings.HasSuffix(execPath, ".app") {
		// `open -n <app> --args ...` launches a bundle with arguments.
		full := []string{"-n", execPath}
		if len(args) > 0 {
			full = append(full, "--args")
			full = append(full, args...)
		}
		return exec.Command("open", full...)
	}
	return exec.Command(execPath, args...)
}
