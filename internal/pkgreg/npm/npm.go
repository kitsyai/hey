// Package npm is the npm ecosystem's implementation of pkgreg.Format: it
// speaks the couchdb-style npm registry protocol (GET package metadata, GET
// tarballs, PUT to publish) well enough for npm/pnpm to both publish WIP
// packages to it and install through it, while overlaying real upstream
// registries (npmjs, or private scopes on Google Artifact Registry) for
// everything not published locally.
package npm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kitsyai/hey/internal/pkgreg"
)

const formatName = "npm"

// maxPublishBody caps a publish request body. Base64-inflated tarballs can
// run well past their raw size (@heypkv/assets was once 164MB raw), so this
// is generous headroom, not a tight limit.
const maxPublishBody = 1 << 30 // 1GiB

// Format is the npm implementation of pkgreg.Format.
type Format struct {
	cfg *npmrcConfig
}

// New builds the npm Format from the npmrc at path (typically ~/.npmrc — see
// DefaultNpmrcPath). A missing file is fine: everything upstreams to
// registry.npmjs.org.
func New(npmrcPath string) (*Format, error) {
	cfg, err := parseNpmrc(npmrcPath)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", npmrcPath, err)
	}
	return &Format{cfg: cfg}, nil
}

// DefaultNpmrcPath returns ~/.npmrc, the machine's source of truth for scope
// registries and token helpers (per the architecture brief: this file, not
// hey's own config, owns upstream resolution).
func DefaultNpmrcPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".npmrc"), nil
}

// Name implements pkgreg.Format.
func (f *Format) Name() string { return formatName }

// Mount implements pkgreg.Format.
//
// npm is mounted at mux root ("/"), not under a "/npm/" prefix. This is a
// deliberate, format-specific decision, not the default for future formats:
// npm/pnpm registry clients treat the configured --registry URL as the
// literal base for every request (GET <registry>/<pkg>, GET
// <registry>/<pkg>/-/<file>.tgz, PUT <registry>/<pkg>) — there is no
// client-side path-prefix support to redirect them under e.g.
// http://127.0.0.1:4874/npm/. So npm must own root for real npm/pnpm clients
// to work against `hey registry` unmodified. A future format with a client
// that DOES support path-based repository URLs (Maven's <repository><url>,
// Cargo's registry index config) would mount under its own prefix instead
// (e.g. "/maven/") and coexist with npm on the same server/port.
func (f *Format) Mount(mux *http.ServeMux, core *pkgreg.Core) {
	mux.HandleFunc("/", f.handle(core))
}

func (f *Format) handle(core *pkgreg.Core) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pkg, rest, ok := parsePackagePath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		root, err := core.FormatDir(formatName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		switch {
		case len(rest) >= 2 && rest[0] == "-":
			f.handleTarball(core, root, w, r, pkg, rest[len(rest)-1])
		case len(rest) == 0 && r.Method == http.MethodGet:
			f.handleMetadata(core, root, w, r, pkg)
		case len(rest) == 0 && r.Method == http.MethodPut:
			f.handlePublish(root, w, r, pkg)
		default:
			http.NotFound(w, r)
		}
	}
}

// parsePackagePath splits a request path into the npm package name and the
// remaining segments, handling scoped packages (@scope/name is two path
// segments that together name one package).
func parsePackagePath(p string) (pkg string, rest []string, ok bool) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return "", nil, false
	}
	if strings.HasPrefix(segs[0], "@") {
		if len(segs) < 2 || segs[1] == "" {
			return "", nil, false
		}
		return segs[0] + "/" + segs[1], segs[2:], true
	}
	return segs[0], segs[1:], true
}

// scopeOf returns the "@scope" portion of a package name, or "" if unscoped.
func scopeOf(pkg string) string {
	if !strings.HasPrefix(pkg, "@") {
		return ""
	}
	scope, _, _ := strings.Cut(pkg, "/")
	return scope
}

// upstreamTarget is where a package's non-local reads (metadata + tarballs)
// come from: the resolved registry base plus whatever bearer auth it needs.
type upstreamTarget struct {
	base        string
	host        string // Core.BearerToken cache key
	tokenHelper string // "" = no auth needed (e.g. registry.npmjs.org)
}

func (f *Format) resolveUpstream(pkg string) upstreamTarget {
	base := f.cfg.registryFor(scopeOf(pkg))
	return upstreamTarget{
		base:        base,
		host:        hostFor(base),
		tokenHelper: f.cfg.tokenHelperFor(base),
	}
}

// --- GET metadata ------------------------------------------------------------

func (f *Format) handleMetadata(core *pkgreg.Core, root string, w http.ResponseWriter, r *http.Request, pkg string) {
	local, err := loadPackage(root, pkg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	target := f.resolveUpstream(pkg)
	bearer, err := core.BearerToken(target.host, target.tokenHelper)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	var upstreamBytes []byte
	body, err := core.FetchBytes(target.base+"/"+pkg, bearer)
	switch {
	case err == nil:
		upstreamBytes = body
	case pkgreg.IsNotFound(err):
		upstreamBytes = nil // purely-local (or genuinely unpublished) package — not an error
	default:
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if upstreamBytes == nil && len(local.Versions) == 0 {
		http.NotFound(w, r)
		return
	}

	selfBase := "http://" + r.Host
	merged, err := mergeMetadata(upstreamBytes, pkg, local, selfBase)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(merged)
}

// mergeMetadata overlays local's versions onto upstream's metadata document
// (local wins per version), rewrites every version's dist.tarball to point
// back at this server (so every tarball fetch — local or upstream — is
// routed through us, which matters for auth-injected proxying), and merges
// dist-tags: a local "latest" only overrides when it is semver-newer, but a
// version published under any other explicit tag always gets that tag.
func mergeMetadata(upstreamBytes []byte, pkg string, local *localPackage, selfBase string) ([]byte, error) {
	doc := map[string]any{}
	if upstreamBytes != nil {
		if err := json.Unmarshal(upstreamBytes, &doc); err != nil {
			return nil, fmt.Errorf("parse upstream metadata for %s: %w", pkg, err)
		}
	}
	if _, ok := doc["name"]; !ok {
		doc["name"] = pkg
	}
	versions, ok := doc["versions"].(map[string]any)
	if !ok {
		versions = map[string]any{}
	}
	distTags, ok := doc["dist-tags"].(map[string]any)
	if !ok {
		distTags = map[string]any{}
	}

	// Every version — upstream and local — gets a tarball URL that routes
	// back through this server.
	rewriteTarball := func(v map[string]any) {
		dist, ok := v["dist"].(map[string]any)
		if !ok {
			dist = map[string]any{}
			v["dist"] = dist
		}
		if url, ok := dist["tarball"].(string); ok && url != "" {
			dist["tarball"] = selfBase + "/" + pkg + "/-/" + path.Base(strings.SplitN(url, "?", 2)[0])
		}
	}
	for _, v := range versions {
		if vm, ok := v.(map[string]any); ok {
			rewriteTarball(vm)
		}
	}

	for version, lv := range local.Versions {
		var vdoc map[string]any
		if err := json.Unmarshal(lv.Doc, &vdoc); err != nil {
			return nil, fmt.Errorf("parse local version %s@%s: %w", pkg, version, err)
		}
		dist, ok := vdoc["dist"].(map[string]any)
		if !ok {
			dist = map[string]any{}
			vdoc["dist"] = dist
		}
		dist["tarball"] = selfBase + "/" + pkg + "/-/" + lv.Tarball
		versions[version] = vdoc // local wins

		tag := lv.Tag
		if tag == "" {
			tag = "latest"
		}
		if tag == "latest" {
			if existing, ok := distTags["latest"].(string); !ok || newer(version, existing) {
				distTags["latest"] = version
			}
		} else {
			distTags[tag] = version
		}
	}

	doc["versions"] = versions
	doc["dist-tags"] = distTags
	return json.Marshal(doc)
}

// --- GET tarball --------------------------------------------------------------

func (f *Format) handleTarball(core *pkgreg.Core, root string, w http.ResponseWriter, r *http.Request, pkg, filename string) {
	if p, ok := localTarball(root, pkg, filename); ok {
		serveLocalFile(w, r, p)
		return
	}

	target := f.resolveUpstream(pkg)
	bearer, err := core.BearerToken(target.host, target.tokenHelper)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	url := target.base + "/" + pkg + "/-/" + filename
	if err := core.ProxyGet(w, url, bearer); err != nil {
		if pkgreg.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
}

func serveLocalFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

// --- PUT publish ---------------------------------------------------------------

// publishPayload is the shape of an `npm publish`/`pnpm publish` PUT body.
type publishPayload struct {
	Name        string                     `json:"name"`
	DistTags    map[string]string          `json:"dist-tags"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Attachments map[string]struct {
		Data string `json:"data"`
	} `json:"_attachments"`
}

func (f *Format) handlePublish(root string, w http.ResponseWriter, r *http.Request, pkg string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPublishBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var payload publishPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid publish payload: "+err.Error())
		return
	}
	if len(payload.Versions) != 1 {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("expected exactly one version in publish payload, got %d", len(payload.Versions)))
		return
	}
	var version string
	var versionDoc json.RawMessage
	for v, d := range payload.Versions {
		version, versionDoc = v, d
	}
	if len(payload.Attachments) != 1 {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("expected exactly one tarball attachment, got %d", len(payload.Attachments)))
		return
	}
	var filename, dataB64 string
	for name, att := range payload.Attachments {
		filename, dataB64 = name, att.Data
	}

	tag := "latest"
	for t, v := range payload.DistTags {
		if v == version {
			tag = t
			break
		}
	}

	existing, err := loadPackage(root, pkg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, dup := existing.Versions[version]; dup {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf(
			"cannot publish %s@%s over the previously published version — local registry versions are immutable, publish a new version (e.g. a -dev.N suffix)",
			pkg, version))
		return
	}

	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, dataB64)
	tarball, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid tarball attachment: "+err.Error())
		return
	}
	if err := os.MkdirAll(tarballsDir(root, pkg), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(tarballPath(root, pkg, filename), tarball, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	existing.Name = pkg
	existing.Versions[version] = localVersion{
		Doc: versionDoc, Tarball: filename, Tag: tag, Published: time.Now(),
	}
	if err := savePackage(root, existing); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "id": pkg, "success": true,
	})
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
