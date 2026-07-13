package sign

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerifySingle(t *testing.T) {
	pub, priv := mustKey(t)
	msg := []byte(`{"hey_deploy":1,"id":"x"}`)
	env, err := AppendSignature(nil, SignManifest(msg, priv))
	if err != nil {
		t.Fatal(err)
	}
	trusted := map[string]ed25519.PublicKey{KeyID(pub): pub}
	signers, err := Verify(msg, env, trusted, 1)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(signers) != 1 || signers[0] != KeyID(pub) {
		t.Errorf("signers = %v", signers)
	}
}

func TestTamperFails(t *testing.T) {
	pub, priv := mustKey(t)
	msg := []byte("original manifest")
	env, _ := AppendSignature(nil, SignManifest(msg, priv))
	trusted := map[string]ed25519.PublicKey{KeyID(pub): pub}
	if _, err := Verify([]byte("tampered manifest"), env, trusted, 1); err == nil {
		t.Fatal("tampered bytes must fail verification")
	}
}

func TestUnknownSignerIgnored(t *testing.T) {
	_, priv := mustKey(t)   // signs
	pubB, _ := mustKey(t)   // the only trusted key — a different one
	msg := []byte("m")
	env, _ := AppendSignature(nil, SignManifest(msg, priv))
	trusted := map[string]ed25519.PublicKey{KeyID(pubB): pubB}
	if _, err := Verify(msg, env, trusted, 1); err == nil {
		t.Fatal("a signature from an unpinned key must not count")
	}
}

func TestQuorum(t *testing.T) {
	pubA, privA := mustKey(t)
	pubB, privB := mustKey(t)
	pubC, privC := mustKey(t)
	msg := []byte("quorum manifest")
	trusted := map[string]ed25519.PublicKey{
		KeyID(pubA): pubA, KeyID(pubB): pubB, KeyID(pubC): pubC,
	}

	// One signer, threshold 2 -> not met.
	env, _ := AppendSignature(nil, SignManifest(msg, privA))
	if _, err := Verify(msg, env, trusted, 2); err == nil {
		t.Fatal("1 of 2 must fail")
	}
	// Append a second independent signer -> quorum met.
	env, _ = AppendSignature(env, SignManifest(msg, privB))
	signers, err := Verify(msg, env, trusted, 2)
	if err != nil {
		t.Fatalf("2 of 2: %v", err)
	}
	if len(signers) != 2 {
		t.Errorf("expected 2 distinct signers, got %v", signers)
	}
	// A third makes 3-of-3 fine too.
	env, _ = AppendSignature(env, SignManifest(msg, privC))
	if _, err := Verify(msg, env, trusted, 3); err != nil {
		t.Fatalf("3 of 3: %v", err)
	}
}

func TestAppendDedupesSameKey(t *testing.T) {
	pub, priv := mustKey(t)
	msg := []byte("m")
	env, _ := AppendSignature(nil, SignManifest(msg, priv))
	env, _ = AppendSignature(env, SignManifest(msg, priv)) // same key twice
	// Even re-signed, one key can only count once toward a quorum.
	trusted := map[string]ed25519.PublicKey{KeyID(pub): pub}
	if _, err := Verify(msg, env, trusted, 2); err == nil || !strings.Contains(err.Error(), "quorum") {
		t.Fatalf("one key must count once; got %v", err)
	}
	if signers, err := Verify(msg, env, trusted, 1); err != nil || len(signers) != 1 {
		t.Fatalf("1 of 1: signers=%v err=%v", signers, err)
	}
}

func TestKeyRoundTrips(t *testing.T) {
	pub, priv := mustKey(t)
	if got, err := DecodePublic(EncodePublic(pub)); err != nil || !got.Equal(pub) {
		t.Fatalf("public round-trip: %v", err)
	}
	restored, err := EncodePrivate(priv).Private()
	if err != nil || !restored.Equal(priv) {
		t.Fatalf("private round-trip: %v", err)
	}
	if len(KeyID(pub)) != 16 {
		t.Errorf("key id should be 16 hex chars, got %q", KeyID(pub))
	}
}
