// Package sign implements hey's own signature protocol: ed25519 (Go stdlib
// primitive) wrapped in hey's detached ".heysig" envelope. hey owns the format
// and workflow — no external signing tool — so it can evolve toward quorum and
// transparency-log trust. The envelope is a LIST of signatures so a manifest
// grows from a single signer to an M-of-N quorum without any format change.
// See docs/trust-and-signing-v0.md.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	// EnvelopeVersion is the .heysig format version this build writes/reads.
	EnvelopeVersion = 1
	// Alg is the only signature algorithm in v0.
	Alg = "ed25519"
)

// Signature is one party's signature over the exact manifest bytes.
type Signature struct {
	KeyID string `json:"key_id"`
	Alg   string `json:"alg"`
	Sig   string `json:"sig"` // base64
}

// Envelope is the detached ".heysig" document: a list of independent
// signatures. A quorum is assembled by parties appending their own.
type Envelope struct {
	HeySig     int         `json:"hey_sig"`
	Signatures []Signature `json:"signatures"`
	// (later) per-signature transparency-log inclusion proofs.
}

// KeyID is a short, stable id for a public key: the first 8 bytes of its
// SHA-256, hex-encoded (16 chars).
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Generate returns a fresh ed25519 keypair.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePublic / DecodePublic move a public key to/from base64 (registry form).
func EncodePublic(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

func DecodePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// PrivateKeyFile is hey's on-disk private-key format (mode 0600, never shared).
type PrivateKeyFile struct {
	HeyKey int    `json:"hey_key"`
	KeyID  string `json:"key_id"`
	Seed   string `json:"ed25519_seed"` // base64 of the 32-byte seed
}

// EncodePrivate captures a private key for storage.
func EncodePrivate(priv ed25519.PrivateKey) PrivateKeyFile {
	pub := priv.Public().(ed25519.PublicKey)
	return PrivateKeyFile{
		HeyKey: 1,
		KeyID:  KeyID(pub),
		Seed:   base64.StdEncoding.EncodeToString(priv.Seed()),
	}
}

// Private reconstitutes the private key from its file form.
func (f PrivateKeyFile) Private() (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(f.Seed)
	if err != nil {
		return nil, fmt.Errorf("decode private seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("private seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SignManifest signs manifestBytes with priv.
func SignManifest(manifestBytes []byte, priv ed25519.PrivateKey) Signature {
	pub := priv.Public().(ed25519.PublicKey)
	return Signature{
		KeyID: KeyID(pub),
		Alg:   Alg,
		Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifestBytes)),
	}
}

// AppendSignature merges s into an existing .heysig document (or an empty one),
// replacing any prior signature from the same key. Co-signing = each party runs
// `hey sign` independently against the same file.
func AppendSignature(existing []byte, s Signature) ([]byte, error) {
	env := Envelope{HeySig: EnvelopeVersion}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &env); err != nil {
			return nil, fmt.Errorf("parse existing .heysig: %w", err)
		}
	}
	if env.HeySig == 0 {
		env.HeySig = EnvelopeVersion
	}
	kept := env.Signatures[:0]
	for _, x := range env.Signatures {
		if x.KeyID != s.KeyID {
			kept = append(kept, x)
		}
	}
	env.Signatures = append(kept, s)
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Verify checks that at least `threshold` DISTINCT trusted keys have validly
// signed manifestBytes. trusted maps key id -> public key. It returns the
// distinct signer ids that verified (for display), and an error if the quorum
// isn't met. This is the judge-and-jury check: a verdict needs a majority.
func Verify(manifestBytes, envelopeBytes []byte, trusted map[string]ed25519.PublicKey, threshold int) ([]string, error) {
	if threshold < 1 {
		threshold = 1
	}
	var env Envelope
	if err := json.Unmarshal(envelopeBytes, &env); err != nil {
		return nil, fmt.Errorf("parse signature envelope: %w", err)
	}
	if env.HeySig > EnvelopeVersion {
		return nil, fmt.Errorf("signature envelope is v%d, newer than this hey (v%d) — update hey", env.HeySig, EnvelopeVersion)
	}
	seen := map[string]bool{}
	var signers []string
	for _, s := range env.Signatures {
		if s.Alg != Alg || seen[s.KeyID] {
			continue
		}
		pub, ok := trusted[s.KeyID]
		if !ok {
			continue // signed by a key this scope doesn't pin — ignored
		}
		raw, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		if ed25519.Verify(pub, manifestBytes, raw) {
			seen[s.KeyID] = true
			signers = append(signers, s.KeyID)
		}
	}
	if len(signers) < threshold {
		return signers, fmt.Errorf("quorum not met: %d valid signature(s), need %d", len(signers), threshold)
	}
	return signers, nil
}
