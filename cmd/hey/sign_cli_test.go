package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kitsyai/hey/internal/sign"
)

// TestVerifyCommandTrusted signs a manifest, pins the key in a scope registry,
// and checks `hey verify` accepts it — then rejects a tampered copy.
func TestVerifyCommandTrusted(t *testing.T) {
	dir := t.TempDir()
	pub, priv, err := sign.Generate()
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"hey_deploy":1,"id":"main","version":"1.0.0"}`)
	mPath := filepath.Join(dir, "main.json")
	if err := os.WriteFile(mPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ := sign.AppendSignature(nil, sign.SignManifest(manifest, priv))
	if err := os.WriteFile(mPath+".heysig", env, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := fmt.Sprintf(`{"hey_registry":0,"scopes":{"t":{"manifest_url":"https://x/{id}/{channel}.json","threshold":1,"keys":[{"id":"%s","ed25519":"%s"}]}}}`,
		sign.KeyID(pub), sign.EncodePublic(pub))
	regPath := filepath.Join(dir, "reg.json")
	if err := os.WriteFile(regPath, []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdVerify([]string{mPath, "--scope", "t", "--registry", regPath}); err != nil {
		t.Fatalf("verify a validly signed manifest: %v", err)
	}

	// Tamper the manifest — the existing signature must no longer verify.
	if err := os.WriteFile(mPath, append(manifest, ' '), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdVerify([]string{mPath, "--scope", "t", "--registry", regPath}); err == nil {
		t.Fatal("a tampered manifest must fail verification")
	}
}

// TestUntrustedGate confirms the resolveManifest trust gate: a source with no
// verifiable publisher signature is refused unless the caller opts in.
func TestUntrustedGate(t *testing.T) {
	// Direct URL (no scope), no opt-in → refused.
	err := verifyTrust("", "https://example.com/m.json", []byte("{}"), deployOpts{})
	if err == nil || !strings.Contains(err.Error(), "UNTRUSTED") {
		t.Fatalf("direct URL without --allow-untrusted must be refused, got %v", err)
	}
	// With opt-in → proceeds (a warning, not an error).
	if err := verifyTrust("", "https://example.com/m.json", []byte("{}"), deployOpts{allowUntrusted: true}); err != nil {
		t.Fatalf("--allow-untrusted should proceed, got %v", err)
	}
}
