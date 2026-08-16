package svc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodPack = `{
  "hey_packs": 0,
  "packs": {
    "demo": {
      "pack": "demo",
      "driver": "archive-exec",
      "versions": {
        "1.2.0": { "artifacts": { "linux_amd64": { "url": "https://x/y.tar.gz", "sha256": "` + sixtyFour + `" } } },
        "1.10.0": { "artifacts": { "linux_amd64": { "url": "https://x/z.tar.gz", "sha256": "` + sixtyFour + `" } } }
      },
      "start": "{bin}/demo -p {port}",
      "ready": { "tcp": "127.0.0.1:{port}" },
      "stop": { "signal": "term" }
    }
  }
}`

const sixtyFour = "0000000000000000000000000000000000000000000000000000000000000000"

func writePacks(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "packs.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLoadPacksValidAndLatest(t *testing.T) {
	home := writePacks(t, goodPack)
	ps, err := LoadPacks(home)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ps.Get("demo")
	if err != nil {
		t.Fatal(err)
	}
	// Numeric segment comparison: 1.10.0 must beat 1.2.0.
	if got := p.LatestVersion(); got != "1.10.0" {
		t.Errorf("LatestVersion = %q, want 1.10.0", got)
	}
}

func TestValidateRejectsBadPacks(t *testing.T) {
	cases := map[string]string{
		"non-https artifact": strings.Replace(goodPack, "https://x/y.tar.gz", "http://x/y.tar.gz", 1),
		"short sha":          strings.Replace(goodPack, sixtyFour, "abc", 1),
		"unknown driver":     strings.Replace(goodPack, "archive-exec", "docker", 1),
		"key mismatch":       strings.Replace(goodPack, `"pack": "demo"`, `"pack": "other"`, 1),
	}
	for name, body := range cases {
		if _, err := LoadPacks(writePacks(t, body)); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestLoadPacksNewerFormatRejected(t *testing.T) {
	body := strings.Replace(goodPack, `"hey_packs": 0`, `"hey_packs": 99`, 1)
	if _, err := LoadPacks(writePacks(t, body)); err == nil ||
		!strings.Contains(err.Error(), "update hey") {
		t.Errorf("expected newer-format rejection, got %v", err)
	}
}

func TestEmbeddedPacksParse(t *testing.T) {
	// The embedded default pack set must always be valid.
	ps, err := parsePacks(defaultPacksJSON, "embedded")
	if err != nil {
		t.Fatalf("embedded packs.json invalid: %v", err)
	}
	_ = ps
}

const modelServerPackJSON = `{
  "hey_packs": 0,
  "packs": {
    "vinci": {
      "pack": "vinci",
      "driver": "model-server",
      "versions": {
        "1.0.0": { "artifacts": { "linux_amd64": { "url": "https://x/llama-server.zip", "sha256": "` + sixtyFour + `" } } }
      },
      "model": { "url": "https://x/model.gguf", "sha256": "` + sixtyFour + `", "filename": "model.gguf" },
      "start": "{bin}/llama-server --model {model} --port {port}",
      "ready": { "http": "http://127.0.0.1:{port}/health" },
      "protocol": "openai-http"
    }
  }
}`

func TestLoadPacksModelServerDriver(t *testing.T) {
	ps, err := LoadPacks(writePacks(t, modelServerPackJSON))
	if err != nil {
		t.Fatalf("model-server pack should validate: %v", err)
	}
	p, err := ps.Get("vinci")
	if err != nil {
		t.Fatal(err)
	}
	if p.Model == nil || p.Model.Filename != "model.gguf" {
		t.Errorf("model artifact not parsed: %+v", p.Model)
	}
	if p.Protocol != "openai-http" {
		t.Errorf("protocol = %q, want openai-http", p.Protocol)
	}
}

func TestValidatePackModelServerRequiresModel(t *testing.T) {
	body := strings.Replace(modelServerPackJSON,
		`"model": { "url": "https://x/model.gguf", "sha256": "`+sixtyFour+`", "filename": "model.gguf" },`, "", 1)
	if _, err := LoadPacks(writePacks(t, body)); err == nil {
		t.Error("expected validation error: model-server driver without a model artifact")
	}
}

func TestValidatePackAllowsPendingPinShape(t *testing.T) {
	// A pending pin (obviously-fake but structurally valid hash) must not
	// break loading the rest of packs.json — only using it errors, loudly,
	// at up-time (see driver_test.go / model_test.go).
	body := strings.Replace(modelServerPackJSON, `"sha256": "`+sixtyFour+`", "filename"`, `"sha256": "`+PendingPinSHA256+`", "filename"`, 1)
	ps, err := LoadPacks(writePacks(t, body))
	if err != nil {
		t.Fatalf("a pending-pin placeholder should still load: %v", err)
	}
	p, _ := ps.Get("vinci")
	if !p.Model.Pending() {
		t.Error("expected the model artifact to report Pending()")
	}
}

func TestValidatePackBridgeNeedsPath(t *testing.T) {
	body := strings.Replace(goodPack, `"stop": { "signal": "term" }`, `"stop": { "signal": "term" }, "bridge": { "kind": "http-query-api" }`, 1)
	if _, err := LoadPacks(writePacks(t, body)); err == nil {
		t.Error("expected validation error: bridge without a path")
	}
}

func TestValidatePackReadyAcceptsHTTP(t *testing.T) {
	body := strings.Replace(goodPack, `"ready": { "tcp": "127.0.0.1:{port}" }`, `"ready": { "http": "http://127.0.0.1:{port}/health" }`, 1)
	if _, err := LoadPacks(writePacks(t, body)); err != nil {
		t.Errorf("http ready check should validate: %v", err)
	}
}
