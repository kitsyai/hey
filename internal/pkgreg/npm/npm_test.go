package npm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kitsyai/hey/internal/pkgreg"
)

// newTestServer wires a Format against a fake upstream and a temp store,
// returning an httptest server for the registry itself plus the store root
// (npm subtree) for direct inspection.
func newTestServer(t *testing.T, npmrc string, upstream *httptest.Server) (regSrv *httptest.Server, npmRoot string) {
	t.Helper()
	storeRoot := t.TempDir()
	core, err := pkgreg.NewCore(storeRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	if upstream != nil {
		core.Client = upstream.Client()
	}

	npmrcPath := filepath.Join(t.TempDir(), ".npmrc")
	if err := os.WriteFile(npmrcPath, []byte(npmrc), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := New(npmrcPath)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	f.Mount(mux, core)
	regSrv = httptest.NewServer(mux)
	t.Cleanup(regSrv.Close)

	root, err := core.FormatDir(formatName)
	if err != nil {
		t.Fatal(err)
	}
	return regSrv, root
}

func npmrcFor(upstreamURL string) string {
	return fmt.Sprintf("@scoped:registry=%s/\n", upstreamURL)
}

// fakeUpstream serves a minimal couchdb-style npm registry for one package
// ("scoped-pkg" or "unscoped-pkg") plus its tarball, and 404s everything
// else — including the purely-local package name, which must never be
// requested successfully.
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@scoped/pkg":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"name": "@scoped/pkg",
				"dist-tags": {"latest": "1.0.0"},
				"versions": {
					"1.0.0": {
						"name": "@scoped/pkg", "version": "1.0.0",
						"dist": {"tarball": "http://upstream.example/@scoped/pkg/-/pkg-1.0.0.tgz", "shasum": "deadbeef"}
					}
				}
			}`))
		case "/@scoped/pkg/-/pkg-1.0.0.tgz":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("upstream-tarball-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestMetadata_MergesLocalOverUpstream(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, root := newTestServer(t, npmrcFor(upstream.URL), upstream)

	publishVersion(t, srv.URL, "@scoped/pkg", "1.1.0", "latest", "pkg-1.1.0.tgz", []byte("local-tarball-bytes"))
	_ = root

	resp, err := http.Get(srv.URL + "/@scoped/pkg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}

	versions := doc["versions"].(map[string]any)
	if _, ok := versions["1.0.0"]; !ok {
		t.Error("upstream version 1.0.0 missing from merged doc")
	}
	if _, ok := versions["1.1.0"]; !ok {
		t.Error("local version 1.1.0 missing from merged doc")
	}
	distTags := doc["dist-tags"].(map[string]any)
	if distTags["latest"] != "1.1.0" {
		t.Errorf("dist-tags.latest = %v, want 1.1.0 (local is semver-newer)", distTags["latest"])
	}

	// The local version's tarball URL must point back at our server, not
	// wherever the publisher's client happened to compute.
	v110 := versions["1.1.0"].(map[string]any)
	tarballURL := v110["dist"].(map[string]any)["tarball"].(string)
	if tarballURL != srv.URL+"/@scoped/pkg/-/pkg-1.1.0.tgz" {
		t.Errorf("local tarball URL = %q", tarballURL)
	}

	// The upstream version's tarball URL must ALSO be rewritten to route
	// through us (so auth-injected proxying applies uniformly).
	v100 := versions["1.0.0"].(map[string]any)
	upstreamTarballURL := v100["dist"].(map[string]any)["tarball"].(string)
	if upstreamTarballURL != srv.URL+"/@scoped/pkg/-/pkg-1.0.0.tgz" {
		t.Errorf("upstream tarball URL not rewritten: %q", upstreamTarballURL)
	}
}

func TestMetadata_OlderLocalDoesNotOverrideLatestTag(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, _ := newTestServer(t, npmrcFor(upstream.URL), upstream)

	// Upstream's latest is 1.0.0; publish an OLDER local version under
	// "latest" — it must not win the dist-tag, even though it's still
	// installable by exact pin.
	publishVersion(t, srv.URL, "@scoped/pkg", "0.9.0", "latest", "pkg-0.9.0.tgz", []byte("old"))

	resp, err := http.Get(srv.URL + "/@scoped/pkg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	distTags := doc["dist-tags"].(map[string]any)
	if distTags["latest"] != "1.0.0" {
		t.Errorf("dist-tags.latest = %v, want 1.0.0 (local 0.9.0 is older)", distTags["latest"])
	}
	versions := doc["versions"].(map[string]any)
	if _, ok := versions["0.9.0"]; !ok {
		t.Error("older local version should still be present in versions map (pinnable)")
	}
}

func TestMetadata_CustomTagAlwaysExposed(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, _ := newTestServer(t, npmrcFor(upstream.URL), upstream)

	// Published under a custom tag, and OLDER than upstream latest — must
	// still be exposed under its own tag regardless of semver.
	publishVersion(t, srv.URL, "@scoped/pkg", "0.5.0", "canary", "pkg-0.5.0.tgz", []byte("canary-bytes"))

	resp, err := http.Get(srv.URL + "/@scoped/pkg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	distTags := doc["dist-tags"].(map[string]any)
	if distTags["canary"] != "0.5.0" {
		t.Errorf("dist-tags.canary = %v, want 0.5.0", distTags["canary"])
	}
	if distTags["latest"] != "1.0.0" {
		t.Errorf("dist-tags.latest = %v, want unchanged 1.0.0", distTags["latest"])
	}
}

func TestMetadata_PurelyLocalPackage(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, _ := newTestServer(t, npmrcFor(upstream.URL), upstream)

	publishVersion(t, srv.URL, "@scoped/never-existed", "0.0.1-dev.0", "latest", "never-existed-0.0.1-dev.0.tgz", []byte("bytes"))

	resp, err := http.Get(srv.URL + "/@scoped/never-existed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 for a purely-local package", resp.StatusCode)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	versions := doc["versions"].(map[string]any)
	if _, ok := versions["0.0.1-dev.0"]; !ok {
		t.Error("purely-local version missing")
	}
	if doc["name"] != "@scoped/never-existed" {
		t.Errorf("name = %v", doc["name"])
	}
}

func TestMetadata_UnknownPackage404s(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, _ := newTestServer(t, npmrcFor(upstream.URL), upstream)

	resp, err := http.Get(srv.URL + "/@scoped/totally-unpublished")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTarball_LocalVsProxyRouting(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, _ := newTestServer(t, npmrcFor(upstream.URL), upstream)

	publishVersion(t, srv.URL, "@scoped/pkg", "2.0.0", "latest", "pkg-2.0.0.tgz", []byte("local-bytes-xyz"))

	// Local tarball: served from the store, not proxied.
	resp, err := http.Get(srv.URL + "/@scoped/pkg/-/pkg-2.0.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readAll(resp)
	if string(body) != "local-bytes-xyz" {
		t.Errorf("local tarball body = %q", body)
	}

	// Upstream-only tarball: proxied through.
	resp2, err := http.Get(srv.URL + "/@scoped/pkg/-/pkg-1.0.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := readAll(resp2)
	if string(body2) != "upstream-tarball-bytes" {
		t.Errorf("proxied tarball body = %q", body2)
	}

	// Neither local nor upstream: 404.
	resp3, err := http.Get(srv.URL + "/@scoped/pkg/-/does-not-exist.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if resp3.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp3.StatusCode)
	}
}

func TestPublish_HappyPathAnd409(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	srv, root := newTestServer(t, npmrcFor(upstream.URL), upstream)

	status := publishVersion(t, srv.URL, "@scoped/pkg", "9.9.9", "latest", "pkg-9.9.9.tgz", []byte("bytes-1"))
	if status != 201 {
		t.Fatalf("first publish status = %d, want 201", status)
	}
	if _, err := os.Stat(filepath.Join(root, "@scoped", "pkg", "tarballs", "pkg-9.9.9.tgz")); err != nil {
		t.Errorf("tarball not written to store: %v", err)
	}

	// Republishing the SAME version must 409 — local versions are immutable.
	status = publishVersion(t, srv.URL, "@scoped/pkg", "9.9.9", "latest", "pkg-9.9.9.tgz", []byte("bytes-2"))
	if status != 409 {
		t.Fatalf("republish status = %d, want 409", status)
	}

	// A -dev.N bump is a different version and must succeed.
	status = publishVersion(t, srv.URL, "@scoped/pkg", "9.9.9-dev.1", "latest", "pkg-9.9.9-dev.1.tgz", []byte("bytes-3"))
	if status != 201 {
		t.Fatalf("dev-bump publish status = %d, want 201", status)
	}
}

func TestSemverNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.1.0", "1.0.0", true},
		{"1.0.0", "1.1.0", false},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.0-dev.0", true},   // release beats prerelease
		{"1.0.0-dev.1", "1.0.0-dev.0", true},
		{"1.0.0-dev.0", "1.0.0-dev.1", false},
		{"2.0.0", "1.9.9", true},
		{"0.0.1-dev.0", "0.0.1-dev.0", false},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// --- test helpers ------------------------------------------------------------

func publishVersion(t *testing.T, registryURL, pkg, version, tag, filename string, tarball []byte) int {
	t.Helper()
	payload := map[string]any{
		"name":      pkg,
		"dist-tags": map[string]string{tag: version},
		"versions": map[string]any{
			version: map[string]any{
				"name": pkg, "version": version,
				"dist": map[string]string{"shasum": "abc123"},
			},
		},
		"_attachments": map[string]any{
			filename: map[string]any{
				"content_type": "application/octet-stream",
				"data":         base64.StdEncoding.EncodeToString(tarball),
				"length":       len(tarball),
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, registryURL+"/"+pkg, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func readAll(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf, nil
}
