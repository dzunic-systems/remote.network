package ingress

import (
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remote.network/node/identity"
	"remote.network/node/wire"
)

func testClient(home string, cfg Config) *Client {
	if cfg.HomeDir == "" {
		cfg.HomeDir = home
	}
	return &Client{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestVerifyCert: the presented-cert gate — nil cert takes the legacy
// path, a good cert yields the root it chains to, and a cert binding a
// different key than the challenge is refused.
func TestVerifyCert(t *testing.T) {
	rootPub, rootPriv, _ := identity.Generate()
	routerPub, routerPriv, _ := identity.Generate()
	_ = routerPriv
	cert, err := identity.SignRouterCert(rootPriv, routerPub, time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	c := testClient(t.TempDir(), Config{})

	// No cert: legacy path.
	if root, err := c.verifyCert(&wire.ChallengeData{}, routerPub); root != "" || err != nil {
		t.Fatalf("nil cert = %q, %v", root, err)
	}
	// Good cert.
	root, err := c.verifyCert(&wire.ChallengeData{Cert: &cert}, routerPub)
	if err != nil || root != identity.PubString(rootPub) {
		t.Fatalf("good cert = %q, %v", root, err)
	}
	// Cert binds a different key than the challenger.
	_, otherPriv, _ := identity.Generate()
	if _, err := c.verifyCert(&wire.ChallengeData{Cert: &cert}, otherPriv.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("foreign-key binding accepted")
	}
	// Broken cert fails closed.
	bad := cert
	bad.Serial = 99
	if _, err := c.verifyCert(&wire.ChallengeData{Cert: &bad}, routerPub); err == nil {
		t.Fatal("tampered cert accepted")
	}
}

// TestCheckRootPin: TOFU writes the pin, repeats pass, foreign roots and a
// mismatching explicit --router-root are fatal.
func TestCheckRootPin(t *testing.T) {
	home := t.TempDir()
	c := testClient(home, Config{})

	if err := c.checkRootPin("ROOTAAAA"); err != nil {
		t.Fatalf("TOFU: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, routerRootPin))
	if err != nil || string(b) != "ROOTAAAA" {
		t.Fatalf("pin file = %q, %v", b, err)
	}
	if err := c.checkRootPin("ROOTAAAA"); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	err = c.checkRootPin("ROOTBBBB")
	if !errors.Is(err, errPinMismatch) {
		t.Fatalf("foreign root = %v", err)
	}

	// Explicit --router-root wins over the file, both ways.
	c2 := testClient(home, Config{RouterRoot: "ROOTBBBB"})
	if err := c2.checkRootPin("ROOTAAAA"); !errors.Is(err, errPinMismatch) {
		t.Fatalf("explicit root vs file = %v", err)
	}
	if err := c2.checkRootPin("ROOTBBBB"); err != nil {
		t.Fatalf("explicit root match = %v", err)
	}
	var _ ed25519.PublicKey // keep the import honest if the test shrinks
}
