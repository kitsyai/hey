package svc

import (
	"slices"
	"strings"
	"testing"
)

// The embedded dsc pack (github.com/kitsyai/dsc) must stay a loopback HTTP
// service on every release platform, open to the sites that use it.
func TestEmbeddedDscPack(t *testing.T) {
	packs, err := LoadPacks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := packs.Get("dsc")
	if err != nil {
		t.Fatal(err)
	}
	if p.Protocol != "http" || p.Bridge != nil || p.Driver != "archive-exec" {
		t.Fatalf("dsc must be a plain proxied HTTP service: %+v", p)
	}
	if !strings.Contains(p.Start, "--port {port}") || !strings.Contains(p.Ready.HTTP, "127.0.0.1:{port}/healthz") {
		t.Fatalf("unexpected start/ready: %q %q", p.Start, p.Ready.HTTP)
	}
	if !slices.Contains(p.CORS.AddOrigins, "https://prashantsinha.com") {
		t.Fatalf("missing prashantsinha.com origin: %v", p.CORS.AddOrigins)
	}
	v := p.Versions[p.LatestVersion()]
	for _, plat := range []string{"windows_amd64", "windows_arm64", "darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		a, ok := v.Artifacts[plat]
		if !ok {
			t.Errorf("no %s artifact", plat)
			continue
		}
		if !strings.HasPrefix(a.URL, "https://github.com/kitsyai/dsc/releases/download/v") {
			t.Errorf("%s: unexpected URL %s", plat, a.URL)
		}
		if a.Pending() || len(a.SHA256) != 64 {
			t.Errorf("%s: checksum not pinned", plat)
		}
	}
}
