package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kitsyai/hey/internal/fetch"
	"github.com/kitsyai/hey/internal/gh"
	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/registry"
)

// splitAppRef splits "app@1.2.3" (or "app@v1.2.3") into name and version.
func splitAppRef(s string) (name, ver string) {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i], strings.TrimPrefix(s[i+1:], "v")
	}
	return s, ""
}

func loadRegistry(override string) (*registry.Registry, error) {
	if override == "" {
		override = os.Getenv("HEY_REGISTRY")
	}
	heyHome, err := home.Dir()
	if err != nil {
		return nil, err
	}
	return registry.Load(override, heyHome)
}

func lookupApp(reg *registry.Registry, name string) (registry.App, error) {
	app, ok := reg.Apps[name]
	if !ok {
		return registry.App{}, fmt.Errorf("unknown app or command %q — apps in this registry: %s",
			name, strings.Join(reg.Names(), ", "))
	}
	return app, nil
}

// resolveVersion returns the version to use: the pinned one, or the latest
// release (cached for 24h unless refresh).
func resolveVersion(name string, app registry.App, pinned string, refresh bool) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	stateDir, err := home.StateDir()
	if err != nil {
		return "", err
	}
	return gh.ResolveLatest(name, app.Source.Repo, app.Source.TagPrefix, stateDir, refresh)
}

// ensureInstalled makes sure bin/<app>/<version>/<binary> exists (downloading
// and verifying if needed) and returns its absolute path.
func ensureInstalled(name string, app registry.App, version string) (string, error) {
	appDir, err := home.AppDir(name)
	if err != nil {
		return "", err
	}
	binFile := app.Source.BinaryFile()
	binPath := filepath.Join(appDir, version, binFile)
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	src := app.Source
	tag := src.Tag(version)
	asset := src.AssetName(version)
	fmt.Fprintf(os.Stderr, "hey: fetching %s %s (%s)\n", name, version, asset)

	sumsRaw, err := fetch.Checksums(gh.DownloadURL(src.Repo, tag, src.ChecksumsAsset))
	if err != nil {
		return "", err
	}
	sums := fetch.ParseChecksums(sumsRaw)

	heyHome, err := home.Dir()
	if err != nil {
		return "", err
	}
	archive, sha, err := fetch.Download(gh.DownloadURL(src.Repo, tag, asset), heyHome)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)

	if err := fetch.Verify(asset, sha, sums, nil); err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp(appDir, ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	if err := fetch.ExtractBinary(archive, binFile, tmpDir); err != nil {
		return "", err
	}
	versionDir := filepath.Join(appDir, version)
	if err := os.Rename(tmpDir, versionDir); err != nil {
		// A parallel install may have won the race; that's success.
		if _, statErr := os.Stat(binPath); statErr != nil {
			return "", fmt.Errorf("install %s %s: %w", name, version, err)
		}
	}

	// "current" is a plain text file, not a symlink (Windows symlinks need
	// privilege/developer mode).
	if err := os.WriteFile(filepath.Join(appDir, "current"), []byte(version+"\n"), 0o644); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "hey: verified and installed %s %s\n", name, version)
	return binPath, nil
}

// currentVersion reads bin/<app>/current, if present.
func currentVersion(name string) (string, bool) {
	appDir, err := home.AppDir(name)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(appDir, "current"))
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(data))
	return v, v != ""
}
