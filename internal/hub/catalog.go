package hub

import (
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/svc"
)

// Service is the seam a local capability implements to appear in the hub's
// /services list (docs/design/services.md §3) — one small interface, the
// same shape internal/pkgreg.Format already establishes as hey's precedent
// for "concrete shared machinery behind a per-ecosystem interface". It does
// not replace svc/contract lifecycle — it is the hub-facing metadata neither
// process manager has a field for: protocol, browser bridge, CORS, MCP.
type Service interface {
	Name() string
	Lifecycle() string // "svc" (internal/svc) or "contract" (internal/state+internal/contract)
	Protocol() string  // "postgres-wire", "http", "openai-http", "mcp"
	Bridge() *svc.Bridge
	CORS() svc.CORSPolicy
	MCP() (endpoint string, ok bool)
}

// svcService adapts an internal/svc pack to Service. P1 ships only
// svc-lifecycle hub services (postgres, vinci); a contract-lifecycle adapter
// is the seam for a future `ai`/`agent` service that instead runs through
// internal/state+internal/contract, per docs/design/services.md §3.
type svcService struct{ pack svc.Pack }

func (s svcService) Name() string         { return s.pack.Pack }
func (s svcService) Lifecycle() string    { return "svc" }
func (s svcService) Protocol() string     { return s.pack.Protocol }
func (s svcService) Bridge() *svc.Bridge  { return s.pack.Bridge }
func (s svcService) CORS() svc.CORSPolicy { return s.pack.CORS }
func (s svcService) MCP() (string, bool)  { return "", s.pack.MCP }

// hubEligible reports whether a pack participates in the hub catalog at
// all — packs with no Protocol set (docs/design/services.md §3) are plain
// internal/svc services with no browser-facing surface.
func hubEligible(s Service) bool { return s.Protocol() != "" }

// ServiceView is one entry in the hub's /services response — docs/design/
// services.md §4's JSON shape, extended with hub_url (the envelope-protected,
// browser-safe address every browser caller should use) alongside the raw
// instance port/url the doc's own example shows for desktop/native callers
// that skip CORS entirely.
type ServiceView struct {
	Name      string `json:"name"`
	Pack      string `json:"pack"`
	Lifecycle string `json:"lifecycle"`
	Protocol  string `json:"protocol"`
	Port      int    `json:"port,omitempty"`
	URL       string `json:"url,omitempty"`        // direct instance URL — desktop/native, no CORS to satisfy
	HubURL    string `json:"hub_url,omitempty"`    // browser-safe, envelope-protected, via the hub's reverse proxy
	BridgeURL string `json:"bridge_url,omitempty"` // wire-protocol services only (postgres)
	// Health is one of "healthy" (process up and its own Ready check passing
	// — the app-side client's "available"), "unhealthy" (process up but not
	// yet passing Ready — "warming", e.g. vinci still loading a multi-GB
	// model into memory), or "stopped" (provisioned but not running). A
	// service that has never been `hey up`'d has no entry at all — the
	// client's "absent" state — rather than a fourth Health value.
	Health    string `json:"health"`
	Version   string `json:"version,omitempty"`
	ModelTier string `json:"model_tier,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Started   string `json:"started,omitempty"`
	MCP       bool   `json:"mcp,omitempty"`
}

// Catalog is the full /services response body.
type Catalog struct {
	HeyHub     int           `json:"hey_hub"`
	HubVersion string        `json:"hub_version"`
	Services   []ServiceView `json:"services"`
}

// BuildCatalog polls the existing lifecycle managers — internal/svc's
// on-disk instances today — and republishes them as one view. The hub is
// never a second source of truth (docs/design/services.md §4): a pack that
// has never been `hey up`'d simply has no instance and does not appear here,
// exactly mirroring `hey svc ls`'s own semantics. Iterating instances (not
// packs) is what makes a custom `hey up postgres --name pg2` show up as its
// own catalog entry keyed by "pg2" — the hub-service name is the *instance*
// name (docs/design/services.md §3's Name() doc: "also the svc.Up... key"),
// which a pack name only coincides with by default.
func BuildCatalog(svcDir string, packs *svc.PackSet, cfg *Config, hubPort int, hubVersion string) (*Catalog, error) {
	insts, err := svc.ListInstances(svcDir)
	if err != nil {
		return nil, err
	}
	cat := &Catalog{HubVersion: hubVersion}
	for _, inst := range insts {
		pack, err := packs.Get(inst.Pack)
		if err != nil {
			continue // the pack that provisioned this instance no longer exists
		}
		svcc := svcService{pack: pack}
		if !hubEligible(svcc) || cfg.serviceConfig(inst.Name).Disabled {
			continue
		}
		cat.Services = append(cat.Services, buildView(inst, svcc, pack, hubPort))
	}
	return cat, nil
}

// ServiceByName returns one service's live view, or ok=false if it is
// unknown to the catalog or has never been provisioned (GET /services/<name>
// 404s in either case — docs/design/services.md §4).
func ServiceByName(svcDir string, packs *svc.PackSet, cfg *Config, hubPort int, name string) (ServiceView, bool, error) {
	inst, err := svc.LoadInstance(instanceDir(svcDir, name))
	if err != nil {
		return ServiceView{}, false, nil // never provisioned — not an error, just absent
	}
	pack, err := packs.Get(inst.Pack)
	if err != nil {
		return ServiceView{}, false, nil
	}
	svcc := svcService{pack: pack}
	if !hubEligible(svcc) || cfg.serviceConfig(name).Disabled {
		return ServiceView{}, false, nil
	}
	return buildView(inst, svcc, pack, hubPort), true, nil
}

func buildView(inst *svc.Instance, s Service, pack svc.Pack, hubPort int) ServiceView {
	health := "stopped"
	if svc.Alive(inst) {
		if probeReady(pack.Ready, inst.Port) {
			health = "healthy"
		} else {
			health = "unhealthy" // process up, not yet passing its Ready check — "warming"
		}
	}
	_, mcpOK := s.MCP() // endpoint reserved for §8's agent-host MCP composition; unused by P1's catalog services
	view := ServiceView{
		Name: inst.Name, Pack: inst.Pack, Lifecycle: s.Lifecycle(), Protocol: s.Protocol(),
		Port: inst.Port, Health: health, Version: inst.Version,
		ModelTier: inst.ModelTier, PID: inst.PID, MCP: mcpOK,
	}
	bridge := s.Bridge()
	if s.Protocol() != "" && bridge == nil {
		// Already browser-speakable HTTP/JSON — direct instance URL plus the
		// hub's envelope-protected reverse proxy.
		view.URL = fmt.Sprintf("http://127.0.0.1:%d", inst.Port)
		view.HubURL = fmt.Sprintf("http://127.0.0.1:%d/svc/%s", hubPort, inst.Name)
	}
	if bridge != nil {
		view.BridgeURL = fmt.Sprintf("http://127.0.0.1:%d%s", hubPort, bridge.Path)
	}
	if !inst.Started.IsZero() {
		view.Started = inst.Started.UTC().Format(time.RFC3339)
	}
	return view
}

// instanceDir returns the svc instance directory for a hub-known name:
// <svcDir>/<name> — the exact layout internal/svc.Up/LoadInstance already use.
func instanceDir(svcDir, name string) string { return filepath.Join(svcDir, name) }

// readyProbeTimeout is deliberately short: the app-side client polls
// /services every 15s (per its own docs/comments), so building the catalog
// must stay fast and side-effect-free — a slow or hung probe against one
// service must never make /services itself slow.
const readyProbeTimeout = 700 * time.Millisecond

// probeReady runs a lightweight version of the pack's own Ready check
// (internal/svc's readyOnce is unexported, and re-running its exact Command
// form on every 15s poll would mean spawning a subprocess — e.g. pg_isready —
// per browser tab per poll; TCP/HTTP checks don't have that cost) to tell a
// process that is merely running from one that is actually ready to serve —
// exactly the "warming" vs "available" distinction the app-side client
// derives from Health. A Command-only Ready check (postgres's pg_isready)
// falls back to a bare TCP dial: not as precise as the real check, but a
// true, side-effect-free signal that the port is at least accepting
// connections, safe to run unconditionally on every catalog build.
func probeReady(ready svc.Ready, port int) bool {
	if ready.HTTP != "" {
		url := strings.ReplaceAll(ready.HTTP, "{port}", strconv.Itoa(port))
		client := http.Client{Timeout: readyProbeTimeout}
		resp, err := client.Get(url)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), readyProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
