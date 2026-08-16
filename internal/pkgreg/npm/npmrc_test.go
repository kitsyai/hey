package npm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureNpmrc mirrors the real machine's ~/.npmrc shape but with fake,
// obviously-non-secret values — no real tokens ever, per the brief.
const fixtureNpmrc = `store-dir=C:\.pnpmg
fetch-timeout=300000
# BEGIN TEST MANAGED REGISTRY
@heypkv:registry=https://asia-south1-npm.pkg.dev/heypkv-packages/heypkv-npm/
//asia-south1-npm.pkg.dev/heypkv-packages/heypkv-npm/:tokenHelper=/fake/bin/token-helper
//asia-south1-npm.pkg.dev/heypkv-packages/heypkv-npm/:always-auth=true
@kitsy:registry=https://asia-south1-npm.pkg.dev/heypkv-packages/kitsy-npm-virtual/
//asia-south1-npm.pkg.dev/heypkv-packages/kitsy-npm-virtual/:tokenHelper=/fake/bin/token-helper
//asia-south1-npm.pkg.dev/heypkv-packages/kitsy-npm-virtual/:always-auth=true
//registry.npmjs.org/:_authToken=sk-should-never-be-read-or-logged
//registry.npmjs.org/:_password=should-never-be-read-either
# END TEST MANAGED REGISTRY
`

func writeFixtureNpmrc(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".npmrc")
	if err := os.WriteFile(path, []byte(fixtureNpmrc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseNpmrc_ScopeRegistry(t *testing.T) {
	path := writeFixtureNpmrc(t)
	cfg, err := parseNpmrc(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.registryFor("@heypkv"); got != "https://asia-south1-npm.pkg.dev/heypkv-packages/heypkv-npm" {
		t.Errorf("registryFor(@heypkv) = %q", got)
	}
	if got := cfg.registryFor("@kitsy"); got != "https://asia-south1-npm.pkg.dev/heypkv-packages/kitsy-npm-virtual" {
		t.Errorf("registryFor(@kitsy) = %q", got)
	}
	if got := cfg.registryFor("@unknown-scope"); got != defaultUpstream {
		t.Errorf("registryFor(@unknown-scope) = %q, want npmjs default", got)
	}
	if got := cfg.registryFor(""); got != defaultUpstream {
		t.Errorf("registryFor(\"\") = %q, want npmjs default", got)
	}
}

func TestParseNpmrc_TokenHelper(t *testing.T) {
	path := writeFixtureNpmrc(t)
	cfg, err := parseNpmrc(path)
	if err != nil {
		t.Fatal(err)
	}
	base := cfg.registryFor("@heypkv")
	if got := cfg.tokenHelperFor(base); got != "/fake/bin/token-helper" {
		t.Errorf("tokenHelperFor(@heypkv base) = %q", got)
	}
	// npmjs needs no auth: no tokenHelper line for it.
	if got := cfg.tokenHelperFor(defaultUpstream); got != "" {
		t.Errorf("tokenHelperFor(npmjs) = %q, want empty (no auth)", got)
	}
}

func TestParseNpmrc_NeverCapturesAuthLines(t *testing.T) {
	path := writeFixtureNpmrc(t)
	cfg, err := parseNpmrc(path)
	if err != nil {
		t.Fatal(err)
	}
	// The parser only recognizes two key shapes; _authToken/_password lines
	// must never surface anywhere in the parsed config.
	for scope, url := range cfg.scopeRegistry {
		if containsSecretish(url) {
			t.Errorf("scopeRegistry[%s] leaked a secret-shaped value: %q", scope, url)
		}
	}
	for host, helper := range cfg.tokenHelper {
		if containsSecretish(helper) {
			t.Errorf("tokenHelper[%s] leaked a secret-shaped value: %q", host, helper)
		}
	}
}

func containsSecretish(s string) bool {
	return strings.Contains(s, "sk-") || strings.Contains(s, "should-never-be")
}

func TestParseNpmrc_MissingFile(t *testing.T) {
	cfg, err := parseNpmrc(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing npmrc should not be an error: %v", err)
	}
	if got := cfg.registryFor("@anything"); got != defaultUpstream {
		t.Errorf("registryFor with empty config = %q, want npmjs default", got)
	}
}

func TestHostFor(t *testing.T) {
	if got := hostFor("https://registry.npmjs.org"); got != "registry.npmjs.org" {
		t.Errorf("hostFor(npmjs) = %q", got)
	}
	if got := hostFor("https://asia-south1-npm.pkg.dev/heypkv-packages/heypkv-npm"); got != "asia-south1-npm.pkg.dev/heypkv-packages/heypkv-npm" {
		t.Errorf("hostFor(gar) = %q", got)
	}
}
