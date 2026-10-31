// routercert.go: company-root-signed router certificates. A root keypair (offline — the operator's machine, never a router)
// signs each router's Ed25519 identity key. A node that has pinned the
// ROOT public key accepts any router carrying a valid chain — router key
// rotation stops being a node-side event. The cert is self-contained
// (root public key rides inside), so it is also the unit router discovery
// (router.directory) advertises: verify against the pinned root, dial.
package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// RouterCert binds one router identity key to the company root. Signature
// covers every field except Signature itself (payload()).
type RouterCert struct {
	RouterID  string `json:"router_id"`  // identity.ID of PublicKey
	PublicKey string `json:"public_key"` // base32 of the router's Ed25519 public key
	RootKey   string `json:"root_key"`   // base32 of the root's PUBLIC key
	RootID    string `json:"root_id"`    // identity.ID of RootKey — must agree
	ExpiresAt string `json:"expires_at"` // RFC3339 UTC; empty = never expires
	Serial    int64  `json:"serial"`
	Signature string `json:"signature"` // base64 std of the root's Ed25519 signature
}

// payload is the deterministic byte string the root signs: the cert with
// the signature blanked. encoding/json emits struct fields in declaration
// order, so the payload is stable for a fixed shape.
func (c RouterCert) payload() ([]byte, error) {
	c.Signature = ""
	return json.Marshal(c)
}

// SignRouterCert issues a certificate for one router identity key.
// expiresAt zero means no expiry — discouraged for anything long-lived.
func SignRouterCert(root ed25519.PrivateKey, routerPub ed25519.PublicKey,
	expiresAt time.Time, serial int64) (RouterCert, error) {
	cert := RouterCert{
		RouterID:  ID(routerPub),
		PublicKey: PubString(routerPub),
		RootKey:   PubString(root.Public().(ed25519.PublicKey)),
		RootID:    ID(root.Public().(ed25519.PublicKey)),
		Serial:    serial,
	}
	if !expiresAt.IsZero() {
		cert.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	}
	payload, err := cert.payload()
	if err != nil {
		return RouterCert{}, err
	}
	sig := ed25519.Sign(root, payload)
	cert.Signature = base64.StdEncoding.EncodeToString(sig)
	return cert, nil
}

// VerifyRouterCert checks a certificate's internal consistency and the
// root's signature. It returns the certified router public key and the
// root public key — the CALLER decides whether it trusts that root (pin
// comparison, TOFU, or a configured expectation). Errors cover: bad
// encoding, RootKey/RootID disagreement, bad signature, expired.
func VerifyRouterCert(cert RouterCert) (routerPub, rootPub ed25519.PublicKey, err error) {
	rootPub, err = PubFromString(cert.RootKey)
	if err != nil {
		return nil, nil, fmt.Errorf("bad root key: %w", err)
	}
	if ID(rootPub) != cert.RootID {
		return nil, nil, fmt.Errorf("root_id %q does not match root_key", cert.RootID)
	}
	routerPub, err = PubFromString(cert.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("bad router key: %w", err)
	}
	if ID(routerPub) != cert.RouterID {
		return nil, nil, fmt.Errorf("router_id %q does not match public_key", cert.RouterID)
	}
	if cert.ExpiresAt != "" {
		exp, err := time.Parse(time.RFC3339, cert.ExpiresAt)
		if err != nil {
			return nil, nil, fmt.Errorf("bad expires_at: %w", err)
		}
		if time.Now().After(exp) {
			return nil, nil, fmt.Errorf("certificate expired %s", cert.ExpiresAt)
		}
	}
	payload, err := cert.payload()
	if err != nil {
		return nil, nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(cert.Signature)
	if err != nil {
		return nil, nil, fmt.Errorf("bad signature encoding: %w", err)
	}
	if !ed25519.Verify(rootPub, payload, sig) {
		return nil, nil, fmt.Errorf("signature does not verify against the embedded root key")
	}
	return routerPub, rootPub, nil
}
