// Package hub implements hey's discovery hub (docs/design/services.md §4):
// a loopback-only HTTP server that polls the existing lifecycle managers
// (internal/svc instances today; internal/state+internal/contract procs are
// the seam for future contract-native services) and republishes them as one
// /services view, applies the security envelope (§5) once for every
// service's browser-facing surface, and reverse-proxies /svc/<name>/* to
// each service's instance port. The hub owns no lifecycle state of its own —
// it is a view, never a second source of truth (§4 "Registration
// mechanism").
package hub

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/svc"
)

//go:embed hub.json
var defaultConfigJSON []byte

// ConfigVersion is the hub-config format version this build understands.
const ConfigVersion = 0

// DefaultPort is 8525 — chosen by exclusion from every other port hey uses
// or relies on; see docs/design/services.md §4 "Port" for the full reasoning
// (User Ports range, outside internal/svc's 5432-5600, distinct from
// registry's 4874 and crow's 9999).
const DefaultPort = 8525

const maxConfigBytes = 1 << 20 // 1 MB

// Config is the hub's own config file, ~/.hey/hub.json (docs/design/
// services.md §4 "Config file & `hey config`").
type Config struct {
	HeyHub   int                      `json:"hey_hub"`
	Port     int                      `json:"port"`
	CORS     CORSConfig               `json:"cors"`
	Services map[string]ServiceConfig `json:"services,omitempty"`
}

// CORSConfig is the hub's default origin allow-list.
type CORSConfig struct {
	AllowOrigins []string `json:"allow_origins"`
}

// ServiceConfig is a per-service override: additive CORS origins (on top of
// both the hub default and the pack's own CORS()) and an opt-out.
type ServiceConfig struct {
	CORS     svc.CORSPolicy `json:"cors,omitempty"`
	Disabled bool           `json:"disabled,omitempty"`
}

// LoadConfig resolves the effective hub config. Precedence, identical to
// packs.json/registry.json: HEY_HUB_CONFIG (path or https URL) -> <heyHome>/
// hub.json -> the embedded default.
func LoadConfig(heyHome string) (*Config, error) {
	if override := os.Getenv("HEY_HUB_CONFIG"); override != "" {
		data, err := readConfigOverride(override)
		if err != nil {
			return nil, err
		}
		return parseConfig(data, override)
	}
	userFile := filepath.Join(heyHome, "hub.json")
	if data, err := os.ReadFile(userFile); err == nil {
		return parseConfig(data, userFile)
	}
	return parseConfig(defaultConfigJSON, "embedded default")
}

func readConfigOverride(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") {
		return nil, fmt.Errorf("hub config URL must be https: %s", src)
	}
	if strings.HasPrefix(src, "https://") {
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(src)
		if err != nil {
			return nil, fmt.Errorf("fetch hub config %s: %w", src, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch hub config %s: HTTP %d", src, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, fmt.Errorf("read hub config: %w", err)
	}
	return data, nil
}

func parseConfig(data []byte, from string) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse hub config (%s): %w", from, err)
	}
	if c.HeyHub > ConfigVersion {
		return nil, fmt.Errorf("hub config (%s) uses format v%d; this hey only understands v%d — update hey", from, c.HeyHub, ConfigVersion)
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if len(c.CORS.AllowOrigins) == 0 {
		return nil, fmt.Errorf("hub config (%s): cors.allow_origins must not be empty", from)
	}
	c.CORS.AllowOrigins = append(c.CORS.AllowOrigins, devOrigins()...)
	return &c, nil
}

// DevOriginsEnv is a comma-separated list of extra allowed origins appended
// to every loaded hub config — the knob for local dev frontends on ports the
// embedded default doesn't know about (e.g. HEY_HUB_DEV_ORIGINS=
// "http://localhost:4603,http://127.0.0.1:4603"). The loopback port-wildcard
// forms "http://localhost:*" and "http://127.0.0.1:*" are also accepted here
// (and in hub.json) to allow every localhost port during development — see
// matchOrigin.
const DevOriginsEnv = "HEY_HUB_DEV_ORIGINS"

func devOrigins() []string {
	raw := os.Getenv(DevOriginsEnv)
	if raw == "" {
		return nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// ResolvePort returns the port the hub should actually bind: HEY_HUB_PORT
// (env) takes precedence over cfg.Port, which itself already defaulted to
// DefaultPort — the same override discipline as HEY_REGISTRY/HEY_PACKS
// (docs/design/services.md §4).
func ResolvePort(cfg *Config) (int, error) {
	if s := os.Getenv("HEY_HUB_PORT"); s != "" {
		p, err := strconv.Atoi(s)
		if err != nil || p <= 0 || p > 65535 {
			return 0, fmt.Errorf("invalid HEY_HUB_PORT %q", s)
		}
		return p, nil
	}
	return cfg.Port, nil
}

// serviceConfig returns the per-service override for name, the zero value if
// none is configured.
func (c *Config) serviceConfig(name string) ServiceConfig {
	if c.Services == nil {
		return ServiceConfig{}
	}
	return c.Services[name]
}
