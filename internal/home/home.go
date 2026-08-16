// Package home resolves and prepares the hey home directory (~/.hey by
// default, HEY_HOME to override) and its well-known subdirectories.
package home

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir returns the hey home directory, creating it if needed.
func Dir() (string, error) {
	root := os.Getenv("HEY_HOME")
	if root == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(userHome, ".hey")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create hey home %s: %w", root, err)
	}
	return root, nil
}

func subdir(name string) (string, error) {
	root, err := Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// BinDir returns ~/.hey/bin, creating it if needed.
func BinDir() (string, error) { return subdir("bin") }

// AppDir returns ~/.hey/bin/<app>, creating it if needed.
func AppDir(app string) (string, error) {
	bin, err := BinDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(bin, app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// LogsDir returns ~/.hey/logs, creating it if needed.
func LogsDir() (string, error) { return subdir("logs") }

// AppsDir returns ~/.hey/apps, creating it if needed. Deployed bundles (the
// hey.deploy.v1 track) live here, keyed by manifest id then version; this is
// separate from ~/.hey/bin, which holds github-release single binaries.
func AppsDir() (string, error) { return subdir("apps") }

// DeployAppDir returns ~/.hey/apps/<id>/<version>, creating it if needed.
func DeployAppDir(id, version string) (string, error) {
	apps, err := AppsDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(apps, id, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// TempInstallDir returns a fresh throwaway install directory under
// ~/.hey/tmp (same volume as ~/.hey so extraction and renames stay cheap). The
// caller owns it and must remove it — `hey run --temp` deletes it on exit.
func TempInstallDir() (string, error) {
	root, err := subdir("tmp")
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return "", fmt.Errorf("create temp install dir: %w", err)
	}
	return dir, nil
}

// StateDir returns ~/.hey/state, creating it if needed.
func StateDir() (string, error) { return subdir("state") }

// SvcDir returns ~/.hey/svc, creating it if needed. Service instances live
// under it; this state is separate from app state (~/.hey/state).
func SvcDir() (string, error) { return subdir("svc") }

// InstanceDir returns ~/.hey/svc/<name>, creating it if needed.
func InstanceDir(name string) (string, error) {
	svc, err := SvcDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(svc, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// ArtifactsDir returns ~/.hey/artifacts, creating it if needed — the
// content-addressed store for large, dedup-worthy downloads (model weights
// today; docs/design/services.md §8), distinct from ~/.hey/svc/<inst>/bin,
// which is namespaced per-instance and never shared.
func ArtifactsDir() (string, error) { return subdir("artifacts") }

// ModelsDir returns ~/.hey/artifacts/models, creating it if needed. Model
// files live at <ModelsDir>/<sha256[:2]>/<sha256>/<filename> — the same path
// shape internal/svc.EnsureModel derives from a raw svcDir, so both agree on
// one location regardless of which one resolves it first.
func ModelsDir() (string, error) {
	artifacts, err := ArtifactsDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(artifacts, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// HubDir returns ~/.hey/hub, creating it if needed — the discovery hub's
// pidfile lives here (internal/hub), mirroring internal/pkgreg's
// registry.pid pattern.
func HubDir() (string, error) { return subdir("hub") }

// PkgRegistryDir returns ~/.hey/registry, creating it if needed — the
// default store root for `hey registry` (the local package registry: npm
// today, other formats later, each namespaced under their own subdir e.g.
// ~/.hey/registry/npm). Distinct from ~/.hey/registry.json, the unrelated
// app-registry override file used by app resolution (see internal/registry).
func PkgRegistryDir() (string, error) { return subdir("registry") }
