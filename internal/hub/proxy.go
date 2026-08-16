package hub

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/kitsyai/hey/internal/svc"
)

// splitSvcPath splits "/svc/<name>/<rest...>" into name and "/<rest...>"
// (empty rest for a bare "/svc/<name>"). Returns "" for anything not under
// /svc/.
func splitSvcPath(path string) (name, rest string) {
	const prefix = "/svc/"
	if !strings.HasPrefix(path, prefix) {
		return "", ""
	}
	trimmed := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i:]
	}
	return trimmed, ""
}

// svcDispatch is the single entry point for everything under /svc/ — the
// hub's front door for browser-facing traffic (docs/design/services.md's
// architecture overview, §2): a bridge-kind pack (postgres) is routed to its
// bridge handler; every other hub-eligible pack with no Bridge is already
// browser-speakable HTTP/JSON (vinci's openai-http) and gets reverse-proxied
// straight to its instance port, prefix-stripped.
//
// <name> is the *instance* name (svc.Up's key — the hub-visible name), which
// only coincides with its pack's name by default; resolving the instance
// first and reading its recorded Pack field (rather than assuming
// packs.Get(name) directly) is what makes a custom `hey up postgres --name
// pg2` route correctly through /svc/pg2/... instead of 404ing.
func svcDispatch(svcDir string, packs *svc.PackSet) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, _ := splitSvcPath(r.URL.Path)
		inst, err := svc.LoadInstance(instanceDir(svcDir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		pack, err := packs.Get(inst.Pack)
		if err != nil || pack.Protocol == "" {
			http.NotFound(w, r)
			return
		}
		if pack.Bridge != nil {
			switch pack.Bridge.Kind {
			case "http-query-api":
				postgresBridgeHandler(inst, pack).ServeHTTP(w, r)
			default:
				http.Error(w, "hey hub: bridge kind "+pack.Bridge.Kind+" not implemented", http.StatusNotImplemented)
			}
			return
		}
		proxyToInstance(inst).ServeHTTP(w, r)
	}
}

// proxyToInstance reverse-proxies /svc/<name>/<rest> to the running
// instance's 127.0.0.1:<port>/<rest> — the security envelope (Envelope,
// applied once around the whole mux) is this proxy's only gate; the instance
// itself trusts anything arriving on its loopback port, exactly like every
// other internal/svc-managed process today.
func proxyToInstance(inst *svc.Instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !svc.Alive(inst) {
			http.Error(w, fmt.Sprintf("hey hub: %s is not running — try `hey up %s`", inst.Name, inst.Name), http.StatusServiceUnavailable)
			return
		}
		target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", inst.Port)}
		proxy := httputil.NewSingleHostReverseProxy(target)
		r2 := r.Clone(r.Context())
		_, rest := splitSvcPath(r.URL.Path)
		if rest == "" {
			rest = "/"
		}
		r2.URL.Path = rest
		proxy.ServeHTTP(w, r2)
	})
}
