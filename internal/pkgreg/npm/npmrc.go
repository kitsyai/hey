package npm

import (
	"bufio"
	"os"
	"strings"
)

// npmrcConfig is the slice of ~/.npmrc that matters for upstream resolution:
// per-scope registry mappings and per-registry token-helper executables.
// This is the ONLY data pulled from .npmrc — the parser matches exactly two
// key shapes (`@scope:registry=` and `//host/path/:tokenHelper=`) and never
// captures or logs any other line, so auth-token lines (`_authToken`,
// `_password`, `_auth`, ...) are never read even in passing.
type npmrcConfig struct {
	scopeRegistry map[string]string // "@heypkv" -> "https://.../heypkv-npm/"
	tokenHelper   map[string]string // "//host/path/" -> absolute exe path
}

// defaultUpstream is where unscoped/unknown-scope packages resolve.
const defaultUpstream = "https://registry.npmjs.org"

// parseNpmrc reads scope-registry and token-helper lines out of an .npmrc
// file. A missing file is not an error (an empty config just upstreams
// everything to npmjs).
func parseNpmrc(path string) (*npmrcConfig, error) {
	cfg := &npmrcConfig{scopeRegistry: map[string]string{}, tokenHelper: map[string]string{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch {
		case strings.HasPrefix(key, "@") && strings.HasSuffix(key, ":registry"):
			scope := strings.TrimSuffix(key, ":registry")
			cfg.scopeRegistry[scope] = val
		case strings.HasPrefix(key, "//") && strings.HasSuffix(key, ":tokenHelper"):
			hostPath := strings.TrimSuffix(key, ":tokenHelper")
			cfg.tokenHelper[hostPath] = val
		default:
			// Deliberately ignored: auth tokens, passwords, always-auth,
			// store-dir, and every other npmrc setting are irrelevant to
			// upstream resolution and must never be captured here.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// registryFor returns the upstream registry base URL for a package scope
// ("@heypkv", or "" for unscoped packages).
func (c *npmrcConfig) registryFor(scope string) string {
	if scope != "" {
		if url, ok := c.scopeRegistry[scope]; ok {
			return strings.TrimSuffix(url, "/")
		}
	}
	return defaultUpstream
}

// tokenHelperFor returns the token-helper executable for a registry base
// URL, matching npm's convention of keying by "//host/path/" — the longest
// matching key wins so a helper scoped to one virtual repo doesn't leak into
// a sibling one under the same host.
func (c *npmrcConfig) tokenHelperFor(registryBase string) string {
	hostPath := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(registryBase, "https:"), "http:"), "/")
	best, bestLen := "", -1
	for key, helper := range c.tokenHelper {
		k := strings.TrimSuffix(key, "/")
		if strings.HasPrefix(hostPath, k) && len(k) > bestLen {
			best, bestLen = helper, len(k)
		}
	}
	return best
}

// hostFor returns the "host" cache key (Core.BearerToken's first argument)
// for a registry base URL — host+path, since a single pkg.dev host can
// front multiple virtual repos each needing their own token-helper/cache
// entry (see the real ~/.npmrc: heypkv-npm and kitsy-npm-virtual share a
// host but not a tokenHelper scope).
func hostFor(registryBase string) string {
	stripped := strings.TrimPrefix(strings.TrimPrefix(registryBase, "https://"), "http://")
	return strings.TrimSuffix(stripped, "/")
}
