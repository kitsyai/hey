// Package pkgreg is the format-agnostic core of hey's local package
// registry (`hey registry`): a loopback-only HTTP server that overlays
// locally-published, work-in-progress packages on top of a real upstream
// registry, so teammates can publish and consume WIP packages exactly as
// they would through the real thing.
//
// The core owns everything that is the same regardless of package
// ecosystem: the store root, the HTTP server lifecycle (pidfile-based
// background start/stop, mirroring crow's ergonomics), a streaming
// reverse-proxy with auth injection, a short-lived bearer-token cache for
// upstream auth helpers, and one-line request logging. Each ecosystem
// (npm today; maven/cargo/etc. later) implements the Format interface and
// mounts its own routes — see internal/pkgreg/npm for the first, and so
// far only, implementation.
package pkgreg

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Format is the seam a package ecosystem implements to plug into the
// registry server. Route registration, publish-protocol handling, and
// metadata-merge semantics are all format-specific and stay behind this
// interface; everything else (proxying, auth, storage root, logging) is
// concrete code in this package used BY every Format.
type Format interface {
	// Name identifies the format ("npm", eventually "maven", "cargo", ...).
	// It also names the format's store subdirectory: <store>/<Name>/...
	Name() string

	// Mount registers this format's routes on mux. Where a format's routes
	// live in the URL space is a per-format decision (see each
	// implementation's Mount for why) — the core does not prescribe a
	// prefix scheme, since npm-style clients require root-relative URLs
	// while path-prefixed formats can coexist under distinct prefixes.
	Mount(mux *http.ServeMux, core *Core)

	// List returns every locally-published package@version this format
	// knows about, for `hey registry ls`/`status`.
	List(storeRoot string) ([]PublishedEntry, error)

	// Clear drops locally-published versions matching pkg/version (both
	// empty clears everything for this format; version empty clears every
	// version of pkg) and returns the count removed, for `hey registry
	// clear`.
	Clear(storeRoot, pkg, version string) (int, error)
}

// PublishedEntry is one locally-published package version, reported by a
// Format's List for the `ls`/`status` commands.
type PublishedEntry struct {
	Format  string
	Package string
	Version string
}

// Core is the shared state every Format's handlers use: where local
// packages are stored, how to reach and authenticate to upstreams, and how
// requests get logged.
type Core struct {
	StoreRoot string // e.g. ~/.hey/registry
	Port      int

	Client *http.Client // upstream HTTP client; overridable in tests

	tokens tokenCache
}

// NewCore builds a Core rooted at storeRoot, creating it if needed.
func NewCore(storeRoot string, port int) (*Core, error) {
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create registry store %s: %w", storeRoot, err)
	}
	return &Core{
		StoreRoot: storeRoot,
		Port:      port,
		Client:    &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// FormatDir returns <store>/<format>, creating it if needed. This is the
// namespacing from the architecture directive: every format gets its own
// store subtree so a future maven/cargo format needs no migration.
func (c *Core) FormatDir(format string) (string, error) {
	dir := filepath.Join(c.StoreRoot, format)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// --- upstream auth: token helper exec + cache -----------------------------

// tokenCache caches bearer tokens per host for ~5 minutes so every request
// doesn't re-exec the token helper.
type tokenCache struct {
	mu      sync.Mutex
	entries map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

const tokenTTL = 5 * time.Minute

// BearerToken returns a bearer token for host, executing tokenHelper (an
// absolute path to an executable that prints the token to stdout) if the
// cached value has expired or doesn't exist. An empty tokenHelper means the
// upstream needs no auth (e.g. registry.npmjs.org) and returns "", nil.
// Formats are the only callers: a format's upstream resolver decides which
// host+helper pair a given request needs; this function just execs+caches.
func (c *Core) BearerToken(host, tokenHelper string) (string, error) {
	if tokenHelper == "" {
		return "", nil
	}
	c.tokens.mu.Lock()
	if c.tokens.entries == nil {
		c.tokens.entries = map[string]cachedToken{}
	}
	if e, ok := c.tokens.entries[host]; ok && time.Now().Before(e.expires) {
		c.tokens.mu.Unlock()
		return e.token, nil
	}
	c.tokens.mu.Unlock()

	out, err := exec.Command(tokenHelper).Output()
	if err != nil {
		return "", fmt.Errorf("token helper %s for %s: %w", tokenHelper, host, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("token helper %s for %s: empty token", tokenHelper, host)
	}

	c.tokens.mu.Lock()
	c.tokens.entries[host] = cachedToken{token: token, expires: time.Now().Add(tokenTTL)}
	c.tokens.mu.Unlock()
	return token, nil
}

// --- streaming reverse proxy -----------------------------------------------

// ProxyGet streams an upstream GET response straight through to w: no
// buffering of the body, so a 164MB tarball never sits in memory.
// notFound is called with true when upstream answers 404 (formats treat
// missing-upstream as "purely local" rather than an error); any other
// non-2xx status is surfaced as an error.
func (c *Core) ProxyGet(w http.ResponseWriter, url, bearer string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &upstreamStatusError{url: url, status: resp.StatusCode}
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, resp.Body)
	return err
}

// FetchBytes GETs url (with optional bearer auth) and returns the full body.
// Used for upstream metadata documents, which are JSON and expected to be
// small relative to tarballs. Returns notFoundErr{} (see IsNotFound) on a
// 404 so callers can treat a purely-local package as normal, not an error.
func (c *Core) FetchBytes(url, bearer string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &upstreamStatusError{url: url, status: http.StatusNotFound}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &upstreamStatusError{url: url, status: resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32MB cap on metadata docs
}

type upstreamStatusError struct {
	url    string
	status int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d", e.url, e.status)
}

// IsNotFound reports whether err is an upstream 404 (see FetchBytes/ProxyGet).
func IsNotFound(err error) bool {
	e, ok := err.(*upstreamStatusError)
	return ok && e.status == http.StatusNotFound
}

// --- request logging --------------------------------------------------------

// LoggingMiddleware wraps h with a clear one-line log per request:
// "METHOD path -> status (duration)".
func LoggingMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
