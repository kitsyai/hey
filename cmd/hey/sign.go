package main

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kitsyai/hey/internal/home"
	"github.com/kitsyai/hey/internal/sign"
)

// cmdKeygen creates an ed25519 keypair for a publisher/attestor: the private
// key is saved 0600 under ~/.hey/keys (or --out), and the public half is
// printed as a ready-to-paste registry scope key.
func cmdKeygen(args []string) error {
	out := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--out":
			if i+1 >= len(args) {
				return fmt.Errorf("--out needs a value")
			}
			i++
			out = args[i]
		default:
			return fmt.Errorf("unknown flag %q (usage: hey keygen [--out <dir>])", args[i])
		}
	}
	if out == "" {
		d, err := home.Dir()
		if err != nil {
			return err
		}
		out = filepath.Join(d, "keys")
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return err
	}
	pub, priv, err := sign.Generate()
	if err != nil {
		return err
	}
	kid := sign.KeyID(pub)
	data, err := json.MarshalIndent(sign.EncodePrivate(priv), "", "  ")
	if err != nil {
		return err
	}
	keyPath := filepath.Join(out, "hey-"+kid+".key")
	if err := os.WriteFile(keyPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("generated ed25519 key %s\n", kid)
	fmt.Printf("private key saved to %s (mode 0600 — keep it secret, never share it)\n\n", keyPath)
	fmt.Println("Pin the public half in a registry scope's \"keys\" (and set \"threshold\"):")
	pin, _ := json.MarshalIndent(map[string]string{"id": kid, "ed25519": sign.EncodePublic(pub)}, "  ", "  ")
	fmt.Printf("  %s\n", pin)
	return nil
}

// cmdSign appends this key's signature to <manifest>.heysig. Each trust party
// runs it independently to assemble a quorum.
func cmdSign(args []string) error {
	var keyFile string
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--key":
			if i+1 >= len(args) {
				return fmt.Errorf("--key needs a value")
			}
			i++
			keyFile = args[i]
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: hey sign <manifest.json> [--key <privkey>]")
	}
	manifest := pos[0]
	priv, err := loadSigningKey(keyFile)
	if err != nil {
		return err
	}
	mb, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	sigPath := manifest + ".heysig"
	existing, _ := os.ReadFile(sigPath)
	s := sign.SignManifest(mb, priv)
	merged, err := sign.AppendSignature(existing, s)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sigPath, merged, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s -> %s (key %s)\n", manifest, sigPath, s.KeyID)
	return nil
}

// loadSigningKey loads a private key from keyFile, or the sole key in
// ~/.hey/keys when keyFile is empty.
func loadSigningKey(keyFile string) (ed25519.PrivateKey, error) {
	if keyFile == "" {
		d, err := home.Dir()
		if err != nil {
			return nil, err
		}
		keysDir := filepath.Join(d, "keys")
		entries, _ := os.ReadDir(keysDir)
		var found []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".key") {
				found = append(found, filepath.Join(keysDir, e.Name()))
			}
		}
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("no signing key in %s — run `hey keygen` or pass --key <file>", keysDir)
		case 1:
			keyFile = found[0]
		default:
			return nil, fmt.Errorf("multiple keys in %s — choose one with --key <file>", keysDir)
		}
	}
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	var pkf sign.PrivateKeyFile
	if err := json.Unmarshal(data, &pkf); err != nil {
		return nil, fmt.Errorf("parse key file %s: %w", keyFile, err)
	}
	return pkf.Private()
}

// cmdVerify checks that a manifest's .heysig meets a scope's signing quorum —
// the same check hey runs automatically on install.
func cmdVerify(args []string) error {
	var scope, registryOverride string
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--scope":
			if i+1 >= len(args) {
				return fmt.Errorf("--scope needs a value")
			}
			i++
			scope = args[i]
		case "--registry":
			if i+1 >= len(args) {
				return fmt.Errorf("--registry needs a value")
			}
			i++
			registryOverride = args[i]
		default:
			pos = append(pos, args[i])
		}
	}
	if len(pos) != 1 || scope == "" {
		return fmt.Errorf("usage: hey verify <manifest.json> --scope <name> [--registry <path|url>]")
	}
	manifest := pos[0]
	mb, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	sigBytes, err := os.ReadFile(manifest + ".heysig")
	if err != nil {
		return fmt.Errorf("read %s.heysig: %w", manifest, err)
	}
	reg, err := loadRegistry(registryOverride)
	if err != nil {
		return err
	}
	keys, threshold, signed, err := reg.ScopeTrust(scope)
	if err != nil {
		return err
	}
	if !signed {
		return fmt.Errorf("scope @%s pins no keys — nothing to verify against", scope)
	}
	signers, err := sign.Verify(mb, sigBytes, keys, threshold)
	if err != nil {
		return err
	}
	fmt.Printf("OK: %s verifies for @%s — signed by %s (%d of %d required)\n",
		manifest, scope, strings.Join(signers, ", "), len(signers), threshold)
	return nil
}
