// Package identity provides Ed25519 node/router identity: seed files,
// stable public IDs, and challenge signing. The wire format for keys is
// lowercase base32 (no padding); IDs are base32(sha256(pub))[0:26].
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// IDLen is the length of the human-facing node/router identifier.
const IDLen = 26

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Generate returns a new Ed25519 keypair.
func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// ID derives the stable public identifier: base32(sha256(pub))[0:26].
func ID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return b32.EncodeToString(h[:])[:IDLen]
}

// PubString encodes a public key for the wire or config files.
func PubString(pub ed25519.PublicKey) string { return b32.EncodeToString(pub) }

// PubFromString decodes PubString output.
func PubFromString(s string) (ed25519.PublicKey, error) {
	raw, err := b32.DecodeString(strings.ToLower(strings.TrimSpace(s)))
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key: %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// LoadOrCreate reads the 32-byte seed at path, creating a new keypair if
// absent. The file (and its directory) is created 0600/0700 — private keys
// are never world-readable and never committed to git.
func LoadOrCreate(path string) (ed25519.PrivateKey, bool, error) {
	seed, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(seed) != ed25519.SeedSize {
			return nil, false, fmt.Errorf("%s: seed is %d bytes, want %d", path, len(seed), ed25519.SeedSize)
		}
		return ed25519.NewKeyFromSeed(seed), false, nil
	case errors.Is(err, os.ErrNotExist):
		_, priv, err := Generate()
		if err != nil {
			return nil, false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, false, err
		}
		if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
			return nil, false, err
		}
		return priv, true, nil
	default:
		return nil, false, err
	}
}

// RandomNonce returns 32 fresh random bytes plus their base64 wire encoding.
func RandomNonce() (string, []byte) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(b), b
}

// SignChallenge signs a raw nonce, base64-encoded for the wire.
func SignChallenge(priv ed25519.PrivateKey, nonce []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, nonce))
}

// VerifyChallenge checks a base64 signature over a raw nonce.
func VerifyChallenge(pub ed25519.PublicKey, nonce []byte, sigB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, nonce, sig)
}
