package hub

import (
	"encoding/json"
	"net/http"

	"github.com/kitsyai/hey/internal/pkgreg"
	"github.com/kitsyai/hey/internal/svc"
)

// NewHandler builds the hub's full HTTP surface — GET /healthz, GET
// /services, GET /services/<name>, and /svc/<name>/* (bridge or reverse
// proxy) — with the security envelope (Envelope) applied once around all of
// it, per docs/design/services.md §5's "built once... applies uniformly...
// to the hub's own endpoints *and* every service's HTTP surface". Logging
// reuses internal/pkgreg.LoggingMiddleware's http.Handler-wrapping idiom —
// the one piece of internal/pkgreg's registry server this hub shares, per
// §5's own note that the registry has no CORS/origin gating to reuse.
func NewHandler(svcDir string, packs *svc.PackSet, cfg *Config, hubPort int, hubVersion string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)
	mux.HandleFunc("/services", servicesHandler(svcDir, packs, cfg, hubPort, hubVersion))
	mux.HandleFunc("/services/", serviceOneHandler(svcDir, packs, cfg, hubPort))
	mux.HandleFunc("/svc/", svcDispatch(svcDir, packs))

	return pkgreg.LoggingMiddleware(Envelope(cfg, packs, mux))
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func servicesHandler(svcDir string, packs *svc.PackSet, cfg *Config, hubPort int, hubVersion string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/services" {
			http.NotFound(w, r)
			return
		}
		cat, err := BuildCatalog(svcDir, packs, cfg, hubPort, hubVersion)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cat.Services == nil {
			cat.Services = []ServiceView{} // "[]" not "null" for an empty catalog
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cat)
	}
}

func serviceOneHandler(svcDir string, packs *svc.PackSet, cfg *Config, hubPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/services/"):]
		if name == "" {
			http.NotFound(w, r)
			return
		}
		view, ok, err := ServiceByName(svcDir, packs, cfg, hubPort, name)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "unknown or not-up service "+name)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(view)
	}
}
