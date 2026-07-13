// Package registry defines how app names resolve to fetchable release
// artifacts. A registry is a JSON document (see docs/registry-v0.md); the
// default one is embedded, and users can override it with a local file or an
// https URL.
package registry

import (
	"crypto/ed25519"
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

	"github.com/kitsyai/hey/internal/sign"
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
	"cache", "version", "help", "svc", "mobile", "open",
	"keygen", "sign", "verify", "uninstall",
}

// Registry maps app names to their sources, and scopes to manifest-URL
// templates (the deploy track, see docs/deployment-manifest-v0.md).
type Registry struct {
	HeyRegistry int              `json:"hey_registry"`
	Apps        map[string]App   `json:"apps"`
	Scopes      map[string]Scope `json:"scopes,omitempty"`
}

// Scope maps a deploy scope (the "heypkv" in @heypkv/main) to a manifest URL
// template. The template is the ONLY producer-specific data hey holds: it
// contains {id} and {channel} placeholders and nothing else. This is the whole
// integration seam — no product knowledge enters hey.
type Scope struct {
	ManifestURL    string     `json:"manifest_url"`
	DefaultChannel string     `json:"default_channel,omitempty"`
	Threshold      int        `json:"threshold,omitempty"` // quorum: distinct valid signatures needed (default 1)
	Keys           []ScopeKey `json:"keys,omitempty"`      // pinned trust parties; empty = unsigned scope
}

// ScopeKey pins one trust party's ed25519 public key. Multiple keys plus a
// threshold make trust a quorum (judge & jury) rather than a single point.
type ScopeKey struct {
	ID      string `json:"id"`
	Ed25519 string `json:"ed25519"` // base64 of the 32-byte public key
	Role    string `json:"role,omitempty"`
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
	if len(r.Apps) == 0 && len(r.Scopes) == 0 {
		return nil, fmt.Errorf("registry (%s) lists no apps or scopes", from)
	}
	for name, app := range r.Apps {
		if err := validateApp(name, app); err != nil {
			return nil, fmt.Errorf("registry (%s): %w", from, err)
		}
	}
	for scope, s := range r.Scopes {
		if err := validateScope(scope, s); err != nil {
			return nil, fmt.Errorf("registry (%s): %w", from, err)
		}
	}
	return &r, nil
}

func validateScope(scope string, s Scope) error {
	if strings.ContainsAny(scope, `/\@ `) || scope == "" {
		return fmt.Errorf("invalid scope name %q", scope)
	}
	if !strings.HasPrefix(s.ManifestURL, "https://") {
		return fmt.Errorf("scope %q: manifest_url must be https, got %q", scope, s.ManifestURL)
	}
	if !strings.Contains(s.ManifestURL, "{id}") || !strings.Contains(s.ManifestURL, "{channel}") {
		return fmt.Errorf("scope %q: manifest_url must contain {id} and {channel} placeholders", scope)
	}
	if len(s.Keys) > 0 {
		t := s.Threshold
		if t == 0 {
			t = 1
		}
		if t < 1 || t > len(s.Keys) {
			return fmt.Errorf("scope %q: threshold %d must be between 1 and the number of keys (%d)", scope, t, len(s.Keys))
		}
		ids := map[string]bool{}
		for _, k := range s.Keys {
			if k.ID == "" {
				return fmt.Errorf("scope %q: a key is missing its id", scope)
			}
			if ids[k.ID] {
				return fmt.Errorf("scope %q: duplicate key id %q", scope, k.ID)
			}
			ids[k.ID] = true
			if _, err := sign.DecodePublic(k.Ed25519); err != nil {
				return fmt.Errorf("scope %q key %q: %w", scope, k.ID, err)
			}
		}
	}
	return nil
}

// ScopeTrust returns the scope's pinned keys (id -> ed25519 public key), its
// effective quorum threshold, and whether the scope is signed at all. An
// unsigned scope (no keys) falls to the untrusted, checksum-only tier.
func (r *Registry) ScopeTrust(scope string) (keys map[string]ed25519.PublicKey, threshold int, signed bool, err error) {
	s, ok := r.Scopes[scope]
	if !ok {
		return nil, 0, false, fmt.Errorf("unknown scope %q", scope)
	}
	if len(s.Keys) == 0 {
		return nil, 0, false, nil
	}
	keys = make(map[string]ed25519.PublicKey, len(s.Keys))
	for _, k := range s.Keys {
		pub, derr := sign.DecodePublic(k.Ed25519)
		if derr != nil {
			return nil, 0, false, fmt.Errorf("scope %q key %q: %w", scope, k.ID, derr)
		}
		keys[k.ID] = pub
	}
	threshold = s.Threshold
	if threshold == 0 {
		threshold = 1
	}
	return keys, threshold, true, nil
}

// ManifestURL resolves a scope's manifest URL template for an id and channel.
// An empty channel falls back to the scope's default_channel, else "stable".
func (r *Registry) ManifestURL(scope, id, channel string) (string, error) {
	s, ok := r.Scopes[scope]
	if !ok {
		known := make([]string, 0, len(r.Scopes))
		for k := range r.Scopes {
			known = append(known, k)
		}
		sort.Strings(known)
		if len(known) == 0 {
			return "", fmt.Errorf("unknown scope %q — this registry defines no scopes", scope)
		}
		return "", fmt.Errorf("unknown scope %q — scopes in this registry: %s", scope, strings.Join(known, ", "))
	}
	if channel == "" {
		channel = s.DefaultChannel
	}
	if channel == "" {
		channel = "stable"
	}
	url := strings.NewReplacer("{id}", id, "{channel}", channel).Replace(s.ManifestURL)
	return url, nil
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
