package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEmbeddedDefaultParses(t *testing.T) {
	r, err := Load("", t.TempDir())
	if err != nil {
		t.Fatalf("embedded default failed to load: %v", err)
	}
	for _, want := range []string{"guten", "djin"} {
		if _, ok := r.Apps[want]; !ok {
			t.Errorf("embedded default missing app %q", want)
		}
	}
	if r.Apps["guten"].Source.TagPrefix != "cli/" {
		t.Errorf("guten tag_prefix = %q, want cli/", r.Apps["guten"].Source.TagPrefix)
	}
	if !r.Apps["djin"].IsUICommand("ui") {
		t.Error("djin should list ui as a UI command")
	}
	if !r.Apps["guten"].IsUICommand("ui") {
		t.Error("guten should list ui as a UI command (since guten cli/v0.3.0)")
	}
}

func TestPrecedenceHomeFileOverEmbedded(t *testing.T) {
	heyHome := t.TempDir()
	custom := `{"hey_registry":0,"apps":{"myapp":{"source":{"type":"github-release","repo":"x/y","tag_prefix":"","asset_template":"a_{version}.{ext}","checksums_asset":"checksums.txt","binary":"a"}}}}`
	if err := os.WriteFile(filepath.Join(heyHome, "registry.json"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load("", heyHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Apps["myapp"]; !ok {
		t.Error("home registry.json should take precedence over embedded")
	}
	if _, ok := r.Apps["guten"]; ok {
		t.Error("embedded registry should not merge into home override")
	}
}

func TestPrecedenceExplicitOverHome(t *testing.T) {
	heyHome := t.TempDir()
	os.WriteFile(filepath.Join(heyHome, "registry.json"), []byte(`{"hey_registry":0,"apps":{"homeapp":{"source":{"type":"github-release","repo":"x/y","tag_prefix":"","asset_template":"a.{ext}","checksums_asset":"c.txt","binary":"a"}}}}`), 0o644)
	explicit := filepath.Join(t.TempDir(), "explicit.json")
	os.WriteFile(explicit, []byte(`{"hey_registry":0,"apps":{"expapp":{"source":{"type":"github-release","repo":"x/y","tag_prefix":"","asset_template":"a.{ext}","checksums_asset":"c.txt","binary":"a"}}}}`), 0o644)
	r, err := Load(explicit, heyHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Apps["expapp"]; !ok {
		t.Error("explicit override should win")
	}
}

func TestRejectsNewerSchema(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"hey_registry":1,"apps":{"a":{"source":{"type":"github-release","repo":"x/y","tag_prefix":"","asset_template":"a.{ext}","checksums_asset":"c.txt","binary":"a"}}}}`), 0o644)
	if _, err := Load(f, t.TempDir()); err == nil || !strings.Contains(err.Error(), "update hey") {
		t.Errorf("newer schema should be rejected with an update hint, got %v", err)
	}
}

func TestRejectsReservedAppName(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"hey_registry":0,"apps":{"ps":{"source":{"type":"github-release","repo":"x/y","tag_prefix":"","asset_template":"a.{ext}","checksums_asset":"c.txt","binary":"a"}}}}`), 0o644)
	if _, err := Load(f, t.TempDir()); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("reserved app name should be rejected, got %v", err)
	}
}

func TestRejectsUnknownSourceType(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"hey_registry":0,"apps":{"a":{"source":{"type":"ftp","repo":"x/y","tag_prefix":"","asset_template":"a.{ext}","checksums_asset":"c.txt","binary":"a"}}}}`), 0o644)
	if _, err := Load(f, t.TempDir()); err == nil || !strings.Contains(err.Error(), "source type") {
		t.Errorf("unknown source type should be rejected, got %v", err)
	}
}

func TestRejectsHTTPRegistryURL(t *testing.T) {
	if _, err := Load("http://example.com/r.json", t.TempDir()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http registry URL should be rejected, got %v", err)
	}
}

func TestEmbeddedDefaultHasHeypkvScope(t *testing.T) {
	r, err := Load("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	url, err := r.ManifestURL("heypkv", "main", "beta")
	if err != nil {
		t.Fatalf("resolve heypkv scope: %v", err)
	}
	if url != "https://cdn.heypkv.ai/hey/main/beta.json" {
		t.Errorf("manifest url = %q", url)
	}
	// Default channel applies when none is given.
	url, err = r.ManifestURL("heypkv", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://cdn.heypkv.ai/hey/main/stable.json" {
		t.Errorf("default-channel url = %q", url)
	}
}

func TestUnknownScopeErrors(t *testing.T) {
	r, _ := Load("", t.TempDir())
	if _, err := r.ManifestURL("nope", "x", ""); err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Errorf("unknown scope should error, got %v", err)
	}
}

func TestScopeOnlyRegistryLoads(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"hey_registry":0,"scopes":{"acme":{"manifest_url":"https://acme.example/{id}/{channel}.json"}}}`), 0o644)
	r, err := Load(f, t.TempDir())
	if err != nil {
		t.Fatalf("scope-only registry should load: %v", err)
	}
	if _, ok := r.Scopes["acme"]; !ok {
		t.Error("acme scope missing")
	}
}

func TestScopeRejectsBadTemplate(t *testing.T) {
	f := filepath.Join(t.TempDir(), "r.json")
	os.WriteFile(f, []byte(`{"hey_registry":0,"scopes":{"acme":{"manifest_url":"https://acme.example/no-placeholders.json"}}}`), 0o644)
	if _, err := Load(f, t.TempDir()); err == nil || !strings.Contains(err.Error(), "placeholders") {
		t.Errorf("scope without placeholders should fail, got %v", err)
	}
}

func TestAssetNameAndTag(t *testing.T) {
	s := Source{
		Repo: "kitsyai/guten", TagPrefix: "cli/",
		AssetTemplate: "guten_{version}_{os}_{arch}.{ext}",
	}
	got := s.AssetName("0.2.7")
	wantExt := "tar.gz"
	if runtime.GOOS == "windows" {
		wantExt = "zip"
	}
	want := "guten_0.2.7_" + runtime.GOOS + "_" + runtime.GOARCH + "." + wantExt
	if got != want {
		t.Errorf("AssetName = %q, want %q", got, want)
	}
	if tag := s.Tag("0.2.7"); tag != "cli/v0.2.7" {
		t.Errorf("Tag = %q, want cli/v0.2.7", tag)
	}
}
