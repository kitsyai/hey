package deploy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// syntheticManifest is a hey.deploy.v1 document covering desktop archive,
// an android package and an iOS link — enough to drive selection for every
// route without any product knowledge.
func syntheticManifest() string {
	return `{
  "hey_deploy": 1,
  "id": "main",
  "name": "Synthetic App",
  "version": "0.1.0",
  "channel": "beta",
  "artifacts": [
    {"platform":"macos","arch":"arm64","kind":"archive","format":"zip",
     "url":"https://example.invalid/mac-arm64.zip","sha256":"aa","launch":{"exec":"App.app"},"interface":"window"},
    {"platform":"macos","arch":"x64","kind":"archive","format":"zip",
     "url":"https://example.invalid/mac-x64.zip","sha256":"bb","launch":{"exec":"App.app"},"interface":"window"},
    {"platform":"windows","arch":"x64","kind":"archive","format":"zip",
     "url":"https://example.invalid/win-x64.zip","sha256":"cc","launch":{"exec":"app.exe"},"interface":"window"},
    {"platform":"linux","arch":"x64","kind":"appimage",
     "url":"https://example.invalid/linux.AppImage","sha256":"dd","interface":"window"},
    {"platform":"linux","arch":"arm64","kind":"binary",
     "url":"https://example.invalid/linux-arm64","sha256":"ee","launch":{"exec":"app"},"interface":"hey-contract"},
    {"platform":"android","kind":"package",
     "url":"https://example.invalid/app.apk","sha256":"ff"},
    {"platform":"ios","kind":"link",
     "url":"https://testflight.apple.com/join/xyz"}
  ]
}`
}

func TestFetchAndParse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, syntheticManifest())
	}))
	defer srv.Close()

	// Fetch enforces https; httptest TLS uses a self-signed cert, so parse
	// directly here and cover the https guard separately.
	m, err := Parse([]byte(syntheticManifest()), "test")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.ID != "main" || m.Version != "0.1.0" {
		t.Errorf("unexpected manifest header: %+v", m)
	}
	if len(m.Artifacts) != 7 {
		t.Errorf("want 7 artifacts, got %d", len(m.Artifacts))
	}
}

func TestFetchOverHTTPTest(t *testing.T) {
	// Use a plain (non-TLS) server but exercise the parse-from-body path via a
	// custom client is overkill; instead confirm Fetch rejects http and that the
	// TLS server body parses. We reuse manifestClient against an https URL by
	// pointing its transport at the test server.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, syntheticManifest())
	}))
	defer srv.Close()

	old := manifestClient
	manifestClient = srv.Client()
	defer func() { manifestClient = old }()

	m, err := Fetch(srv.URL) // srv.URL is https://127.0.0.1:port
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if m.ID != "main" {
		t.Errorf("fetched wrong manifest: %+v", m)
	}
}

func TestFetchRejectsHTTP(t *testing.T) {
	if _, err := Fetch("http://example.com/x.json"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http manifest URL should be rejected, got %v", err)
	}
}

func TestSelectDesktopMatchesRuntime(t *testing.T) {
	m, err := Parse([]byte(syntheticManifest()), "test")
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.Select(CurrentPlatform(), CurrentArch())
	// The synthetic manifest covers macos(arm64/x64), windows(x64),
	// linux(x64/arm64) — the common CI matrix. If the host is exotic, selection
	// must error clearly rather than pick wrong.
	switch {
	case err == nil:
		if a.Platform != CurrentPlatform() {
			t.Errorf("selected %s, want %s", a.Platform, CurrentPlatform())
		}
		if !archMatches(a.Arch, CurrentArch()) {
			t.Errorf("selected arch %s does not match %s", a.Arch, CurrentArch())
		}
	default:
		if !strings.Contains(err.Error(), "no artifact for") {
			t.Errorf("unexpected selection error: %v", err)
		}
		t.Logf("host %s/%s not in synthetic manifest (ok): %v", runtime.GOOS, runtime.GOARCH, err)
	}
}

func TestSelectNoMatchErrors(t *testing.T) {
	m, _ := Parse([]byte(syntheticManifest()), "test")
	if _, err := m.Select("plan9", "sparc"); err == nil || !strings.Contains(err.Error(), "no artifact for") {
		t.Errorf("expected clear no-match error, got %v", err)
	}
}

func TestUniversalArchMatches(t *testing.T) {
	m, _ := Parse([]byte(`{"hey_deploy":1,"id":"u","version":"1","artifacts":[
		{"platform":"macos","arch":"universal","kind":"archive","url":"https://x/y.zip","sha256":"aa","launch":{"exec":"A.app"}}]}`), "t")
	if _, err := m.Select("macos", "arm64"); err != nil {
		t.Errorf("universal arch should match arm64: %v", err)
	}
	if _, err := m.Select("macos", "x64"); err != nil {
		t.Errorf("universal arch should match x64: %v", err)
	}
}

func TestSelectPackageAndLink(t *testing.T) {
	m, _ := Parse([]byte(syntheticManifest()), "test")
	pkg, err := m.SelectPackage("android")
	if err != nil {
		t.Fatalf("android package: %v", err)
	}
	if pkg.Kind != KindPackage || !strings.HasSuffix(pkg.URL, ".apk") {
		t.Errorf("wrong package artifact: %+v", pkg)
	}
	link, err := m.SelectLink("")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if link.Kind != KindLink || !strings.Contains(link.URL, "testflight") {
		t.Errorf("wrong link artifact: %+v", link)
	}
}

func TestSha256Required(t *testing.T) {
	bad := `{"hey_deploy":1,"id":"x","version":"1","artifacts":[
		{"platform":"macos","arch":"arm64","kind":"archive","url":"https://x/y.zip"}]}`
	if _, err := Parse([]byte(bad), "t"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("archive without sha256 must fail, got %v", err)
	}
	// link needs no sha256.
	okLink := `{"hey_deploy":1,"id":"x","version":"1","artifacts":[
		{"platform":"ios","kind":"link","url":"https://store/app"}]}`
	if _, err := Parse([]byte(okLink), "t"); err != nil {
		t.Errorf("link without sha256 should parse, got %v", err)
	}
}

func TestParseRejectsHTTPArtifactURL(t *testing.T) {
	bad := `{"hey_deploy":1,"id":"x","version":"1","artifacts":[
		{"platform":"macos","arch":"arm64","kind":"archive","url":"http://x/y.zip","sha256":"aa"}]}`
	if _, err := Parse([]byte(bad), "t"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http artifact URL must fail, got %v", err)
	}
}

func TestParseRejectsNewerSchema(t *testing.T) {
	bad := `{"hey_deploy":2,"id":"x","version":"1","artifacts":[
		{"platform":"macos","arch":"arm64","kind":"link","url":"https://x"}]}`
	if _, err := Parse([]byte(bad), "t"); err == nil || !strings.Contains(err.Error(), "update hey") {
		t.Errorf("newer schema should be rejected with update hint, got %v", err)
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in     string
		kind   RefKind
		scope  string
		id     string
		url    string
		app    string
		hasErr bool
	}{
		{in: "@heypkv/main", kind: RefScoped, scope: "heypkv", id: "main"},
		{in: "@heypkv/sub/id", kind: RefScoped, scope: "heypkv", id: "sub/id"},
		{in: "https://cdn.example.com/x.json", kind: RefManifestURL, url: "https://cdn.example.com/x.json"},
		{in: "guten", kind: RefAppName, app: "guten"},
		{in: "http://x/y.json", hasErr: true},
		{in: "@bad", hasErr: true},
		{in: "@scope/", hasErr: true},
	}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if c.hasErr {
			if err == nil {
				t.Errorf("ParseRef(%q) should error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q): %v", c.in, err)
			continue
		}
		if r.Kind != c.kind || r.Scope != c.scope || r.ID != c.id || r.ManifestURL != c.url || r.AppName != c.app {
			t.Errorf("ParseRef(%q) = %+v", c.in, r)
		}
	}
}
