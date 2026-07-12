// Package registry defines how app names resolve to fetchable release
// artifacts. A registry is a JSON document (see docs/registry-v0.md); the
// default one is embedded, and users can override it with a local file or an
// https URL.
package registry

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

//go:embed default.json
var defaultJSON []byte

// SchemaVersion is the registry format version this build understands.
const SchemaVersion = 0

// maxRegistryBytes caps remote registry downloads.
const maxRegistryBytes = 1 << 20 // 1 MB

// Reserved lists subcommand names that can never be app names.
var Reserved = []string{
	"run", "install", "update", "ls", "ps", "stop", "which",
	"cache", "version", "help", "svc",
}

// Registry maps app names to their sources.
type Registry struct {
	HeyRegistry int            `json:"hey_registry"`
	Apps        map[string]App `json:"apps"`
}

// App is one runnable tool.
type App struct {
	Description string   `json:"description,omitempty"`
	Source      Source   `json:"source"`
	UICommands  []string `json:"ui_commands,omitempty"`
}

// Source describes where an app's release artifacts live.
type Source struct {
	Type           string `json:"type"`
	Repo           string `json:"repo"`
	TagPrefix      string `json:"tag_prefix"`
	AssetTemplate  string `json:"asset_template"`
	ChecksumsAsset string `json:"checksums_asset"`
	Binary         string `json:"binary"`
}

// Load resolves the effective registry. Precedence: explicit override (path
// or https URL, typically from --registry or HEY_REGISTRY), then
// <heyHome>/registry.json, then the embedded default.
func Load(override, heyHome string) (*Registry, error) {
	if override != "" {
		data, err := readOverride(override)
		if err != nil {
			return nil, err
		}
		return parse(data, override)
	}
	userFile := filepath.Join(heyHome, "registry.json")
	if data, err := os.ReadFile(userFile); err == nil {
		return parse(data, userFile)
	}
	return parse(defaultJSON, "embedded default")
}

func readOverride(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") {
		return nil, fmt.Errorf("registry URL must be https: %s", src)
	}
	if strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(src)
		if err != nil {
			return nil, fmt.Errorf("fetch registry %s: %w", src, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch registry %s: HTTP %d", src, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, maxRegistryBytes))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	return data, nil
}

func parse(data []byte, from string) (*Registry, error) {
	var r Registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse registry (%s): %w", from, err)
	}
	if r.HeyRegistry > SchemaVersion {
		return nil, fmt.Errorf("registry (%s) uses format v%d; this hey only understands v%d — update hey", from, r.HeyRegistry, SchemaVersion)
	}
	if len(r.Apps) == 0 {
		return nil, fmt.Errorf("registry (%s) lists no apps", from)
	}
	for name, app := range r.Apps {
		if err := validateApp(name, app); err != nil {
			return nil, fmt.Errorf("registry (%s): %w", from, err)
		}
	}
	return &r, nil
}

func validateApp(name string, app App) error {
	for _, res := range Reserved {
		if name == res {
			return fmt.Errorf("app %q collides with a reserved hey command", name)
		}
	}
	if strings.ContainsAny(name, `/\@ `) || name == "" {
		return fmt.Errorf("invalid app name %q", name)
	}
	s := app.Source
	if s.Type != "github-release" {
		return fmt.Errorf("app %q: unsupported source type %q (this hey only supports github-release)", name, s.Type)
	}
	if !strings.Contains(s.Repo, "/") {
		return fmt.Errorf("app %q: source.repo must be owner/name, got %q", name, s.Repo)
	}
	if s.AssetTemplate == "" || s.ChecksumsAsset == "" || s.Binary == "" {
		return fmt.Errorf("app %q: source needs asset_template, checksums_asset and binary", name)
	}
	return nil
}

// Names returns the registry's app names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Apps))
	for n := range r.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// IsUICommand reports whether sub triggers the UI-run flow for this app.
func (a App) IsUICommand(sub string) bool {
	for _, c := range a.UICommands {
		if c == sub {
			return true
		}
	}
	return false
}

// AssetName expands the asset template for a version on the current platform.
func (s Source) AssetName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	rep := strings.NewReplacer(
		"{version}", version,
		"{os}", runtime.GOOS,
		"{arch}", runtime.GOARCH,
		"{ext}", ext,
	)
	return rep.Replace(s.AssetTemplate)
}

// Tag returns the git tag for a version (tag_prefix + "v" + version).
func (s Source) Tag(version string) string {
	return s.TagPrefix + "v" + version
}

// BinaryFile returns the binary filename on the current platform.
func (s Source) BinaryFile() string {
	if runtime.GOOS == "windows" {
		return s.Binary + ".exe"
	}
	return s.Binary
}
