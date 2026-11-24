package clientproto

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"remote.network/node/identity"
	"remote.network/node/wire"
)

// receiptPayload is the byte string a receipt's signature covers: the
// receipt with Sig blanked. encoding/json writes struct fields in
// declaration order, so the payload is fixed for one ReceiptVersion
// (identity.RouterCert signs the same way).
func receiptPayload(r wire.UsageReceipt) ([]byte, error) {
	r.Sig = ""
	return json.Marshal(r)
}

// SignReceipt stamps the current ReceiptVersion and the node's id onto r
// and signs it with the node's identity key.
func SignReceipt(node ed25519.PrivateKey, r wire.UsageReceipt) (wire.UsageReceipt, error) {
	r.V = wire.ReceiptVersion
	r.NodeID = identity.ID(node.Public().(ed25519.PublicKey))
	payload, err := receiptPayload(r)
	if err != nil {
		return wire.UsageReceipt{}, err
	}
	r.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(node, payload))
	return r, nil
}

// Receipt verification failures. ErrReceiptVersion means "a node newer
// than this verifier" — not forgery; callers may treat it as unverified.
var (
	ErrReceiptVersion = errors.New("receipt version not supported")
	ErrReceiptSig     = errors.New("receipt signature does not verify")
)

// VerifyReceipt checks r against the node's public key (base32, as the
// node's WSS session and the nodes table carry it): the version, that the
// receipt names that node, and the signature.
func VerifyReceipt(nodePub string, r wire.UsageReceipt) error {
	if r.V != wire.ReceiptVersion {
		return fmt.Errorf("%w: v%d", ErrReceiptVersion, r.V)
	}
	pub, err := identity.PubFromString(nodePub)
	if err != nil {
		return fmt.Errorf("bad node key: %w", err)
	}
	if identity.ID(pub) != r.NodeID {
		return fmt.Errorf("receipt names node %q, key is %q", r.NodeID, identity.ID(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(r.Sig)
	if err != nil {
		return ErrReceiptSig
	}
	payload, err := receiptPayload(r)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return ErrReceiptSig
	}
	return nil
}
