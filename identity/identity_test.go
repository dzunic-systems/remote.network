package identity

import (
	"path/filepath"
	"testing"
)

func TestIDStableAndDerivedFromPub(t *testing.T) {
	pub, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if got := ID(pub); len(got) != IDLen {
		t.Fatalf("id length = %d, want %d", len(got), IDLen)
	}
	other, _, _ := Generate()
	if ID(pub) == ID(other) {
		t.Fatal("collision between distinct keys")
	}
}

func TestPubStringRoundtrip(t *testing.T) {
	pub, _, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	back, err := PubFromString(PubString(pub))
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(pub) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestLoadOrCreatePersistsSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "node.key")
	priv1, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	priv2, created, err := LoadOrCreate(path)
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if string(priv1) != string(priv2) {
		t.Fatal("seed not persisted")
	}
}

func TestChallengeSignVerify(t *testing.T) {
	pub, priv, _ := Generate()
	nonceB64, nonce := RandomNonce()
	sig := SignChallenge(priv, nonce)
	if !VerifyChallenge(pub, nonce, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyChallenge(pub, nonce, sig+"x") {
		t.Fatal("garbage signature accepted")
	}
	if nonceB64 == "" {
		t.Fatal("empty nonce")
	}
}
