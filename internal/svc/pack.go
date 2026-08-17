// Package svc provisions and manages local services (postgres, caches, object
// storage) from data-only *packs* run by drivers that ship in the hey binary.
// See docs/pack-manifest-v0.md. v0 implements one driver, archive-exec, which
// contains zero service-specific logic — everything about postgres lives in
// its pack, never here.
package svc

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

//go:embed packs.json
var defaultPacksJSON []byte

// PacksVersion is the pack-manifest format version this build understands.
const PacksVersion = 0

// DriverArchiveExec is v0's generic archive-fetch-and-run driver (postgres).
const DriverArchiveExec = "archive-exec"

// DriverModelServer is the hey-services-P1 driver (docs/design/services.md
// §8): identical archive-exec lifecycle for the server binary (llama-server),
// plus one extra provisioning step — ensuring a separately-pinned, large
// model-weight artifact is present via a resumable, content-addressed
// download (see Pack.Model, EnsureModel) — before Start runs.
const DriverModelServer = "model-server"

const maxPacksBytes = 1 << 20 // 1 MB

// PendingPinSHA256 is the obviously-fake placeholder digest a pack uses to
// mark an artifact that needs a real upstream pin. It has the right shape
// (64 hex chars) to pass structural validation — so the rest of packs.json
// still loads — but Pack.Artifact/EnsureModel refuse to fetch it, with an
// error naming exactly what needs pinning. See docs/design/services.md §8.
const PendingPinSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"

// PackSet is a collection of packs, embedded or overridden (same format).
type PackSet struct {
	HeyPacks int             `json:"hey_packs"`
	Packs    map[string]Pack `json:"packs"`
}

// Pack describes one service as data. The driver interprets it.
//
// Protocol/Bridge/CORS/MCP are the hey-services-P1 extension
// (docs/design/services.md §3): a data-only addition to the pack schema so
// the hub can describe a pack's browser-facing surface without either
// process manager (internal/svc here, internal/state+internal/contract for
// contract-native services) growing hub-specific fields. All four are
// optional; a pack with none of them set simply never appears in the hub's
// /services catalog.
type Pack struct {
	Pack     string             `json:"pack"`
	Driver   string             `json:"driver"`
	Kind     string             `json:"kind,omitempty"`
	Versions map[string]Version `json:"versions"`
	Init     []string           `json:"init,omitempty"`
	Start    string             `json:"start"`
	Ready    Ready              `json:"ready"`
	Conn     string             `json:"conn,omitempty"`
	Stop     StopSpec           `json:"stop"`

	Protocol string     `json:"protocol,omitempty"` // "postgres-wire", "http", "openai-http", "mcp"
	Bridge   *Bridge    `json:"bridge,omitempty"`
	CORS     CORSPolicy `json:"cors,omitempty"`
	MCP      bool       `json:"mcp,omitempty"`

	// Model, driver=="model-server" only, single-tier packs: the separately-
	// pinned, large model weight artifact, fetched resumably into a
	// content-addressed store (EnsureModel) rather than into the pack's own
	// bin/ archive. {model} resolves to its on-disk path in Start's command
	// template. Mutually exclusive with Models/DefaultModel below; a pack
	// sets exactly one of the two shapes.
	Model *ModelArtifact `json:"model,omitempty"`

	// Models + DefaultModel, driver=="model-server" only, multi-tier packs
	// (docs/design/services.md §8's tiny/small/medium size tiers — "hey up
	// vinci --model <tier>"): named model artifacts sharing one server
	// binary. DefaultModel names the entry Models used when --model is
	// omitted (P1: a fixed default per north-star's "small" balanced tier;
	// RAM-probe auto-selection is a fast-follow, not implemented here).
	Models       map[string]ModelArtifact `json:"models,omitempty"`
	DefaultModel string                   `json:"default_model,omitempty"`
}

// ResolveModel picks the model artifact for tier ("" = the pack's default),
// covering both single-tier packs (Model) and multi-tier packs (Models +
// DefaultModel). It returns the resolved tier name alongside the artifact so
// callers (EnsureModel via UpModel, the hub catalog) can record which tier an
// instance is actually running.
func (p Pack) ResolveModel(tier string) (*ModelArtifact, string, error) {
	if len(p.Models) > 0 {
		if tier == "" {
			tier = p.DefaultModel
		}
		if tier == "" {
			return nil, "", fmt.Errorf("pack %q defines model tiers but no default_model", p.Pack)
		}
		m, ok := p.Models[tier]
		if !ok {
			names := make([]string, 0, len(p.Models))
			for n := range p.Models {
				names = append(names, n)
			}
			sort.Strings(names)
			return nil, "", fmt.Errorf("pack %q has no model tier %q — available: %s", p.Pack, tier, strings.Join(names, ", "))
		}
		return &m, tier, nil
	}
	if p.Model != nil {
		if tier != "" {
			return nil, "", fmt.Errorf("pack %q does not define multiple model tiers — omit --model", p.Pack)
		}
		return p.Model, "", nil
	}
	return nil, "", fmt.Errorf("pack %q has no model artifact configured", p.Pack)
}

// Bridge describes hey's browser-facing translation of a native protocol —
// docs/design/services.md §3. nil means Protocol() is already
// browser-speakable HTTP/JSON and the hub just reverse-proxies it.
type Bridge struct {
	Path string `json:"path"` // e.g. "/svc/postgres" — the hub-mounted prefix
	Kind string `json:"kind"` // "http-query-api", "openai-compatible", "mcp"
}

// CORSPolicy is a pack's additive origin allow-list on top of the hub
// default (docs/design/services.md §3/§5).
type CORSPolicy struct {
	AddOrigins []string `json:"add_origins,omitempty"`
}

// ModelArtifact is a model-server pack's separately-downloaded weight file.
// SHA256 may be PendingPinSHA256 to mark "not pinned yet" — packs.json still
// loads, but EnsureModel refuses to fetch it (see PendingPinSHA256).
type ModelArtifact struct {
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Filename string `json:"filename"`

	// MinRAMMB, optional, is the model's explicit physical-RAM requirement in
	// MB for the pre-launch RAM check (see preflightModelRAM). When 0, the
	// model file's on-disk size is used as the estimate instead.
	MinRAMMB int `json:"min_ram_mb,omitempty"`
}

// Pending reports whether a is an unpinned placeholder (obviously-fake hash
// or URL) rather than a real, fetchable artifact.
func (a Artifact) Pending() bool { return isPendingPin(a.SHA256, a.URL) }

// Pending reports whether m is an unpinned placeholder model artifact. A nil
// m (no model configured) is never pending.
func (m *ModelArtifact) Pending() bool {
	if m == nil {
		return false
	}
	return isPendingPin(m.SHA256, m.URL)
}

func isPendingPin(sha256, url string) bool {
	return sha256 == PendingPinSHA256 || strings.Contains(url, "TODO-PIN-REQUIRED")
}

// Version holds the per-platform artifacts for one pack version.
type Version struct {
	Artifacts map[string]Artifact `json:"artifacts"`
}

// Artifact is one platform's downloadable archive.
type Artifact struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	BinSubdir string `json:"bin_subdir,omitempty"`
}

// Ready is a health check: exactly one of TCP, Command, or HTTP.
type Ready struct {
	TCP            string `json:"tcp,omitempty"`
	Command        string `json:"command,omitempty"`
	HTTP           string `json:"http,omitempty"` // GET this URL (templated); 2xx = ready
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// StopSpec describes graceful shutdown; the driver force-kills as a last resort.
type StopSpec struct {
	Command      string `json:"command,omitempty"`
	Signal       string `json:"signal,omitempty"`
	GraceSeconds int    `json:"grace_seconds,omitempty"`
}

// LoadPacks resolves the effective pack set. Precedence: HEY_PACKS override
// (path or https URL), then <heyHome>/packs.json, then the embedded default.
func LoadPacks(heyHome string) (*PackSet, error) {
	if override := os.Getenv("HEY_PACKS"); override != "" {
		data, err := readPacksOverride(override)
		if err != nil {
			return nil, err
		}
		return parsePacks(data, override)
	}
	userFile := filepath.Join(heyHome, "packs.json")
	if data, err := os.ReadFile(userFile); err == nil {
		return parsePacks(data, userFile)
	}
	return parsePacks(defaultPacksJSON, "embedded default")
}

func readPacksOverride(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") {
		return nil, fmt.Errorf("packs URL must be https: %s", src)
	}
	if strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(src)
		if err != nil {
			return nil, fmt.Errorf("fetch packs %s: %w", src, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch packs %s: HTTP %d", src, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, maxPacksBytes))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("read packs: %w", err)
	}
	return data, nil
}

func parsePacks(data []byte, from string) (*PackSet, error) {
	var ps PackSet
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, fmt.Errorf("parse packs (%s): %w", from, err)
	}
	if ps.HeyPacks > PacksVersion {
		return nil, fmt.Errorf("packs (%s) use format v%d; this hey only understands v%d — update hey", from, ps.HeyPacks, PacksVersion)
	}
	for name, p := range ps.Packs {
		if err := validatePack(name, p); err != nil {
			return nil, fmt.Errorf("packs (%s): %w", from, err)
		}
	}
	return &ps, nil
}

// validDrivers are the drivers this hey build's svc package interprets.
// archive-exec is v0's generic fetch-extract-run pipeline; model-server
// (hey-services-P1, docs/design/services.md §8) reuses that pipeline for the
// server binary and adds one step (EnsureModel) for a second, separately
// pinned artifact.
var validDrivers = map[string]bool{
	DriverArchiveExec: true,
	DriverModelServer: true,
}

func validatePack(name string, p Pack) error {
	if strings.ContainsAny(name, `/\@ `) || name == "" {
		return fmt.Errorf("invalid pack name %q", name)
	}
	if p.Pack != name {
		return fmt.Errorf("pack %q: field \"pack\" is %q, must match its key", name, p.Pack)
	}
	if !validDrivers[p.Driver] {
		return fmt.Errorf("pack %q: unsupported driver %q (this hey only supports %q, %q)", name, p.Driver, DriverArchiveExec, DriverModelServer)
	}
	if len(p.Versions) == 0 {
		return fmt.Errorf("pack %q: no versions", name)
	}
	if p.Start == "" {
		return fmt.Errorf("pack %q: missing start command", name)
	}
	if p.Ready.TCP == "" && p.Ready.Command == "" && p.Ready.HTTP == "" {
		return fmt.Errorf("pack %q: ready needs a tcp, command, or http check", name)
	}
	for ver, v := range p.Versions {
		if len(v.Artifacts) == 0 {
			return fmt.Errorf("pack %q version %q: no artifacts", name, ver)
		}
		for plat, a := range v.Artifacts {
			if !strings.HasPrefix(a.URL, "https://") {
				return fmt.Errorf("pack %q %s/%s: artifact url must be https", name, ver, plat)
			}
			if len(a.SHA256) != 64 {
				return fmt.Errorf("pack %q %s/%s: sha256 must be 64 hex chars", name, ver, plat)
			}
		}
	}
	if p.Driver == DriverModelServer && p.Model == nil && len(p.Models) == 0 {
		return fmt.Errorf("pack %q: driver %q needs a \"model\" or \"models\" artifact", name, DriverModelServer)
	}
	if p.Model != nil && len(p.Models) > 0 {
		return fmt.Errorf("pack %q: set exactly one of \"model\" or \"models\", not both", name)
	}
	if p.Model != nil {
		if err := validateModelArtifact(name, "", *p.Model); err != nil {
			return err
		}
	}
	for tier, m := range p.Models {
		if err := validateModelArtifact(name, tier, m); err != nil {
			return err
		}
	}
	if len(p.Models) > 0 {
		if p.DefaultModel == "" {
			return fmt.Errorf("pack %q: \"models\" needs a \"default_model\"", name)
		}
		if _, ok := p.Models[p.DefaultModel]; !ok {
			return fmt.Errorf("pack %q: default_model %q is not one of its models", name, p.DefaultModel)
		}
	}
	if p.Bridge != nil && p.Bridge.Path == "" {
		return fmt.Errorf("pack %q: bridge needs a path", name)
	}
	return nil
}

func validateModelArtifact(pack, tier string, m ModelArtifact) error {
	label := pack
	if tier != "" {
		label = fmt.Sprintf("%s model %q", pack, tier)
	}
	if !strings.HasPrefix(m.URL, "https://") {
		return fmt.Errorf("pack %q: model url must be https", label)
	}
	if len(m.SHA256) != 64 {
		return fmt.Errorf("pack %q: model sha256 must be 64 hex chars", label)
	}
	if m.Filename == "" {
		return fmt.Errorf("pack %q: model needs a filename", label)
	}
	return nil
}

// Get returns the named pack.
func (ps *PackSet) Get(name string) (Pack, error) {
	p, ok := ps.Packs[name]
	if !ok {
		return Pack{}, fmt.Errorf("unknown pack %q — packs available: %s", name, strings.Join(ps.Names(), ", "))
	}
	return p, nil
}

// Names returns the pack names, sorted.
func (ps *PackSet) Names() []string {
	names := make([]string, 0, len(ps.Packs))
	for n := range ps.Packs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LatestVersion returns the highest version string of the pack (semver-ish,
// falling back to lexical). Explicit pins skip this.
func (p Pack) LatestVersion() string {
	vers := make([]string, 0, len(p.Versions))
	for v := range p.Versions {
		vers = append(vers, v)
	}
	sort.Sort(byVersion(vers))
	if len(vers) == 0 {
		return ""
	}
	return vers[len(vers)-1]
}

// Artifact resolves the artifact for version on the current platform.
func (p Pack) Artifact(version string) (Artifact, string, error) {
	v, ok := p.Versions[version]
	if !ok {
		return Artifact{}, "", fmt.Errorf("pack %q has no version %q", p.Pack, version)
	}
	key := runtime.GOOS + "_" + runtime.GOARCH
	a, ok := v.Artifacts[key]
	if !ok {
		return Artifact{}, "", fmt.Errorf("pack %q %s has no artifact for %s", p.Pack, version, key)
	}
	return a, key, nil
}
