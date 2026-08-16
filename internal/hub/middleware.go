package hub

import (
	"net"
	"net/http"
	"strings"

	"github.com/kitsyai/hey/internal/svc"
)

// RequiredClientHeader is the custom header docs/design/services.md §5 point
// 2 requires on every request: because it is a non-simple header, the Fetch
// spec forces a CORS preflight before any browser can send it, so a plain
// <form> POST or a `fetch(..., {mode:"no-cors"})` — neither of which can set
// custom headers — can never reach a mutating endpoint, blind or otherwise.
const RequiredClientHeader = "X-Hey-Client"

// Envelope wraps next with the security envelope docs/design/services.md §5
// describes as "built once" and applied uniformly to the hub's own endpoints
// and every service's browser-facing surface (the reverse proxy, the
// postgres bridge): Host validation, an origin allow-list (hub default +
// additive per-service CORS), the required custom header, and Private-
// Network-Access preflight support. Loopback-only binding (§5 point 4) is
// enforced by the caller's ListenAndServe address, not here.
func Envelope(cfg *Config, packs *svc.PackSet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// §5 point 5: Host header validation closes the DNS-rebinding gap
		// Origin-checking alone leaves open.
		if !hostAllowed(r.Host) {
			http.Error(w, "hey hub: invalid Host header", http.StatusBadRequest)
			return
		}

		allowList := allowedOrigins(cfg, packs, r.URL.Path)
		origin := r.Header.Get("Origin")
		allowed := origin != "" && originAllowed(origin, allowList)
		if origin != "" && allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			// §5 point 3: PNA preflight, gated by the same Origin allow-list.
			if origin == "" || !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", RequiredClientHeader+", Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// A browser-originated request (Origin present) that isn't allowed is
		// rejected outright. A request with no Origin at all — the desktop
		// app's native fetch, or a non-browser caller like curl — has nothing
		// to check here; §5's remaining defenses (Host validation, the
		// required header below) still apply to it.
		if origin != "" && !allowed {
			http.Error(w, "hey hub: origin not allowed", http.StatusForbidden)
			return
		}

		// §5 point 2: required on every real request, browser or not — the
		// single signal that distinguishes an intentional hey-aware caller
		// from a drive-by request that merely landed on a loopback port.
		if r.Header.Get(RequiredClientHeader) == "" {
			http.Error(w, "hey hub: missing "+RequiredClientHeader+" header", http.StatusBadRequest)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether a Host header names loopback by hostname —
// 127.0.0.1, localhost, or ::1 — regardless of port (the hub and every
// proxied service always listen on 127.0.0.1; docs/design/services.md §6's
// hey.local/<service>.hey names are a P2 addition to this allow-list, not
// implemented here).
func hostAllowed(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}

// allowedOrigins is the hub default allow-list plus, for a request under
// /svc/<name>/..., that service's additive CORS — both the pack's own
// CORS() (docs/design/services.md §3) and the hub.json per-service override
// (§4) — so one service can widen its own access without weakening every
// other service's default.
func allowedOrigins(cfg *Config, packs *svc.PackSet, path string) []string {
	origins := append([]string{}, cfg.CORS.AllowOrigins...)
	name, _ := splitSvcPath(path)
	if name == "" {
		return origins
	}
	if pack, err := packs.Get(name); err == nil {
		origins = append(origins, pack.CORS.AddOrigins...)
	}
	origins = append(origins, cfg.serviceConfig(name).CORS.AddOrigins...)
	return origins
}

// originAllowed reports whether origin matches any entry in list, honoring
// the "https://*.domain" wildcard-subdomain form (an apex like
// "https://heypkv.ai" is a separate, explicit entry — the wildcard never
// matches the apex itself, only a real subdomain).
func originAllowed(origin string, list []string) bool {
	for _, pattern := range list {
		if matchOrigin(origin, pattern) {
			return true
		}
	}
	return false
}

func matchOrigin(origin, pattern string) bool {
	if pattern == origin {
		return true
	}
	const wildcard = "https://*."
	if !strings.HasPrefix(pattern, wildcard) {
		return false
	}
	domain := strings.TrimPrefix(pattern, wildcard)
	const scheme = "https://"
	if !strings.HasPrefix(origin, scheme) {
		return false
	}
	host := strings.TrimPrefix(origin, scheme)
	suffix := "." + domain
	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}
