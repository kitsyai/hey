package pgwire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 (RFC 5802 / RFC 7677), channel binding "n" (none) — the
// mechanism postgres's initdb -A scram-sha-256 requires. Go's stdlib has no
// PBKDF2 (it lives in golang.org/x/crypto, a dependency hey does not carry —
// see the package doc), so pbkdf2HMACSHA256 below hand-rolls RFC 8018's
// straightforward HMAC-iteration construction.

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// pbkdf2HMACSHA256 derives a keyLen-byte key from password+salt with iter
// HMAC-SHA256 iterations (RFC 8018 §5.2). keyLen must be <= sha256.Size
// (32) — SCRAM only ever asks for exactly one block, which is all this
// bridge needs, so the multi-block path is intentionally not implemented.
func pbkdf2HMACSHA256(password, salt []byte, iter, keyLen int) ([]byte, error) {
	if keyLen > sha256.Size {
		return nil, fmt.Errorf("pgwire: pbkdf2 key length %d exceeds one HMAC-SHA256 block", keyLen)
	}
	u := hmacSHA256(password, append(append([]byte{}, salt...), 0, 0, 0, 1))
	t := make([]byte, len(u))
	copy(t, u)
	for i := 1; i < iter; i++ {
		u = hmacSHA256(password, u)
		for k := range t {
			t[k] ^= u[k]
		}
	}
	return t[:keyLen], nil
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// clientNonce returns a fresh, printable random nonce for the SCRAM
// exchange (24 base64 characters is generous entropy for a single handshake).
func clientNonce() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// scramState carries the exchange's intermediate values between the
// first-message and final-message steps.
type scramState struct {
	clientNonce     string
	clientFirstBare string // "n=,r=<clientNonce>"
	saltedPassword  []byte
	authMessage     string
}

// scramClientFirst builds the SASLInitialResponse payload: the mechanism
// name, the length of the bare client-first-message, and the message itself
// (gs2 header "n,," = no channel binding, no authzid).
func scramClientFirst() (payload []byte, state *scramState, err error) {
	nonce, err := clientNonce()
	if err != nil {
		return nil, nil, err
	}
	bare := "n=,r=" + nonce
	gs2 := "n,,"
	msg := gs2 + bare

	var out []byte
	out = append(out, "SCRAM-SHA-256"...)
	out = append(out, 0)
	out = appendUint32(out, uint32(len(msg)))
	out = append(out, msg...)
	return out, &scramState{clientNonce: nonce, clientFirstBare: bare}, nil
}

// scramClientFinal parses the server-first-message (AuthenticationSASLContinue
// payload), derives the salted password, and builds the client-final-message
// (SASLResponse payload) plus the AuthMessage needed to verify the server's
// final signature.
func scramClientFinal(state *scramState, serverFirst []byte, password string) (payload []byte, err error) {
	fields := parseSCRAMFields(string(serverFirst))
	serverNonce := fields["r"]
	saltB64 := fields["s"]
	iterStr := fields["i"]
	if serverNonce == "" || saltB64 == "" || iterStr == "" || !strings.HasPrefix(serverNonce, state.clientNonce) {
		return nil, fmt.Errorf("pgwire: malformed or mismatched SCRAM server-first-message")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("pgwire: bad SCRAM salt: %w", err)
	}
	iter, err := strconv.Atoi(iterStr)
	if err != nil || iter <= 0 {
		return nil, fmt.Errorf("pgwire: bad SCRAM iteration count %q", iterStr)
	}

	saltedPassword, err := pbkdf2HMACSHA256([]byte(password), salt, iter, sha256.Size)
	if err != nil {
		return nil, err
	}
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)

	channelBinding := base64.StdEncoding.EncodeToString([]byte("n,,"))
	clientFinalWithoutProof := "c=" + channelBinding + ",r=" + serverNonce
	authMessage := state.clientFirstBare + "," + string(serverFirst) + "," + clientFinalWithoutProof

	clientSignature := hmacSHA256(storedKey, []byte(authMessage))
	clientProof := xorBytes(clientKey, clientSignature)

	final := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)

	state.saltedPassword = saltedPassword
	state.authMessage = authMessage
	return []byte(final), nil
}

// scramVerifyServerFinal checks AuthenticationSASLFinal's "v=<signature>"
// against the expected ServerSignature, defending against a
// man-in-the-middle that got this far without knowing the password.
func scramVerifyServerFinal(state *scramState, serverFinal []byte) error {
	fields := parseSCRAMFields(string(serverFinal))
	gotB64 := fields["v"]
	if gotB64 == "" {
		return fmt.Errorf("pgwire: SCRAM server-final-message missing signature")
	}
	got, err := base64.StdEncoding.DecodeString(gotB64)
	if err != nil {
		return fmt.Errorf("pgwire: bad SCRAM server signature: %w", err)
	}
	serverKey := hmacSHA256(state.saltedPassword, []byte("Server Key"))
	want := hmacSHA256(serverKey, []byte(state.authMessage))
	if !hmac.Equal(got, want) {
		return fmt.Errorf("pgwire: SCRAM server signature mismatch — possible MITM")
	}
	return nil
}

// parseSCRAMFields splits a comma-separated "k=v,k=v" SCRAM message into a
// map. Values may themselves contain "=" (base64 payloads do), so only the
// first "=" in each field splits key from value.
func parseSCRAMFields(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if idx := strings.IndexByte(part, '='); idx > 0 {
			out[part[:idx]] = part[idx+1:]
		}
	}
	return out
}
