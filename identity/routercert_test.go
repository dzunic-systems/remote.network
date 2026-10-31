package identity

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

// TestRouterCert: the sign/verify matrix — a good chain verifies and
// returns both keys; tampering, expiry, and identity mismatches all fail.
func TestRouterCert(t *testing.T) {
	rootPub, rootPriv, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	routerPub, routerPriv, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	_ = routerPriv

	cert, err := SignRouterCert(rootPriv, routerPub, time.Now().Add(24*time.Hour), 7)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Serial != 7 || cert.RouterID != ID(routerPub) || cert.RootID != ID(rootPub) {
		t.Fatalf("cert fields wrong: %+v", cert)
	}
	gotRouter, gotRoot, err := VerifyRouterCert(cert)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !gotRouter.Equal(routerPub) || !gotRoot.Equal(rootPub) {
		t.Fatal("verify returned different keys than signed")
	}

	// No expiry is legal.
	noExp, err := SignRouterCert(rootPriv, routerPub, time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyRouterCert(noExp); err != nil {
		t.Fatalf("no-expiry cert rejected: %v", err)
	}

	// Expired.
	expired, _ := SignRouterCert(rootPriv, routerPub, time.Now().Add(-time.Minute), 1)
	if _, _, err := VerifyRouterCert(expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired cert: %v", err)
	}

	// Tampered payload (serial flipped after signing).
	tampered := cert
	tampered.Serial = 8
	if _, _, err := VerifyRouterCert(tampered); err == nil {
		t.Fatal("tampered cert verified")
	}

	// RootKey swapped to another root: the signature was made by the
	// original root, so it cannot verify against the swapped-in key.
	_, otherRootPriv, _ := Generate()
	otherRootPub := otherRootPriv.Public().(ed25519.PublicKey)
	foreignRootKey := cert
	foreignRootKey.RootKey = PubString(otherRootPub)
	foreignRootKey.RootID = ID(otherRootPub)
	if _, _, err := VerifyRouterCert(foreignRootKey); err == nil {
		t.Fatal("cert with swapped root_key verified")
	}

	// Router id does not match the certified key.
	mismatched, _ := SignRouterCert(rootPriv, routerPub, time.Now().Add(time.Hour), 1)
	mismatched.RouterID = ID(routerPub)[:25] + "x"
	if _, _, err := VerifyRouterCert(mismatched); err == nil {
		t.Fatal("id/key mismatch verified")
	}

	// RootID disagrees with RootKey (the two must always agree).
	badRoot := cert
	badRoot.RootID = ID(rootPub)[:25] + "x"
	if _, _, err := VerifyRouterCert(badRoot); err == nil {
		t.Fatal("root_id/root_key mismatch verified")
	}
}

// TestRouterCertRotation: the headline scenario — a router rotates its
// identity key; the NEW key's certificate still chains to the same root,
// so a node that pinned only the root accepts it without any re-pinning.
func TestRouterCertRotation(t *testing.T) {
	_, rootPriv, _ := Generate()
	_, oldPriv, _ := Generate()
	_, newPriv, _ := Generate()

	oldCert, _ := SignRouterCert(rootPriv, oldPriv.Public().(ed25519.PublicKey), time.Now().Add(time.Hour), 1)
	newCert, _ := SignRouterCert(rootPriv, newPriv.Public().(ed25519.PublicKey), time.Now().Add(time.Hour), 2)

	_, oldRoot, err := VerifyRouterCert(oldCert)
	if err != nil {
		t.Fatal(err)
	}
	_, newRoot, err := VerifyRouterCert(newCert)
	if err != nil {
		t.Fatal(err)
	}
	if !oldRoot.Equal(newRoot) {
		t.Fatal("rotated key chained to a different root")
	}
	var _ ed25519.PublicKey = oldRoot
}
