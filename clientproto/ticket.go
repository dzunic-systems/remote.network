package clientproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"remote.network/node/identity"
)

// TicketVersion is the Ticket shape this build signs and verifies (see
// wire.ReceiptVersion for why a new field bumps it).
const TicketVersion = 1

// TicketLifetime is how long a ticket may START a request — short,
// so a suspended account's tickets die within minutes even if the revoke
// push is lost. A request may run past it.
const TicketLifetime = 120 * time.Second

// TicketSkew is the clock disagreement VerifyTicket tolerates each way.
const TicketSkew = 30 * time.Second

// Ticket is a router's signed permission for one client key to spend on
// one node. The node verifies the chain —
// pinned company root → RouterCert → router key → ticket — so any
// certified router can issue it, and the node needs no spine access.
type Ticket struct {
	V          int                 `json:"v"`
	ID         string              `json:"id"` // 128-bit random, base32
	RouterID   string              `json:"router_id"`
	RouterCert identity.RouterCert `json:"router_cert"`
	NodeID     string              `json:"node_id"`
	ModelID    string              `json:"model_id"`   // the pool id the node advertises
	ClientPub  string              `json:"client_pub"` // must equal the QUIC peer's key
	Account    string              `json:"account"`    // opaque to the node (an HMAC the router can map back)
	SelfUse    bool                `json:"self_use,omitempty"`
	PriceIn    int                 `json:"price_in_per_1m"` // micro-USD, as priced at issue
	PriceOut   int                 `json:"price_out_per_1m"`
	MaxTokens  int                 `json:"max_out_tokens"`
	MaxReqs    int                 `json:"max_requests"`
	NotBefore  int64               `json:"nbf"` // unix seconds
	NotAfter   int64               `json:"exp"`
	ConvKey    string              `json:"conv,omitempty"`
	Sig        string              `json:"sig"` // base64 std Ed25519 by the router key
}

// NewTicketID is a fresh 128-bit ticket id.
func NewTicketID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

func ticketPayload(t Ticket) ([]byte, error) {
	t.Sig = ""
	return json.Marshal(t)
}

// SignTicket stamps the version and the issuing router's identity (from
// its certificate) and signs with the router's key, which must be the key
// the certificate certifies.
func SignTicket(routerKey ed25519.PrivateKey, cert identity.RouterCert, t Ticket) (Ticket, error) {
	if identity.PubString(routerKey.Public().(ed25519.PublicKey)) != cert.PublicKey {
		return Ticket{}, errors.New("router key is not the certified key")
	}
	t.V = TicketVersion
	t.RouterID = cert.RouterID
	t.RouterCert = cert
	payload, err := ticketPayload(t)
	if err != nil {
		return Ticket{}, err
	}
	t.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(routerKey, payload))
	return t, nil
}

// Ticket verification failures, distinct so a node can log (and a test
// can assert) which rule refused.
var (
	ErrTicketVersion   = errors.New("ticket version not supported")
	ErrTicketRoot      = errors.New("ticket router is not certified by the pinned root")
	ErrTicketSig       = errors.New("ticket signature does not verify")
	ErrTicketNode      = errors.New("ticket is for another node")
	ErrTicketNotYet    = errors.New("ticket not yet valid")
	ErrTicketExpired   = errors.New("ticket expired")
	ErrTicketMalformed = errors.New("ticket malformed")
)

// VerifyTicket checks a ticket for the node nodeID at time now: version,
// the router certificate against the pinned root, the router's signature,
// the node, and the validity window (±TicketSkew). Per-request rules —
// the TLS peer key, the model, the request cap, revocation — are the
// serving node's, on top of this.
func VerifyTicket(t Ticket, rootPub ed25519.PublicKey, nodeID string, now time.Time) error {
	if t.V != TicketVersion {
		return fmt.Errorf("%w: v%d", ErrTicketVersion, t.V)
	}
	if t.ID == "" || t.ClientPub == "" || t.NotAfter <= t.NotBefore {
		return ErrTicketMalformed
	}
	routerPub, certRoot, err := identity.VerifyRouterCert(t.RouterCert)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTicketRoot, err)
	}
	if !certRoot.Equal(rootPub) || t.RouterCert.RouterID != t.RouterID {
		return ErrTicketRoot
	}
	sig, err := base64.StdEncoding.DecodeString(t.Sig)
	if err != nil {
		return ErrTicketSig
	}
	payload, err := ticketPayload(t)
	if err != nil {
		return err
	}
	if !ed25519.Verify(routerPub, payload, sig) {
		return ErrTicketSig
	}
	if t.NodeID != nodeID {
		return ErrTicketNode
	}
	if now.Add(TicketSkew).Unix() < t.NotBefore {
		return ErrTicketNotYet
	}
	if now.Add(-TicketSkew).Unix() > t.NotAfter {
		return ErrTicketExpired
	}
	return nil
}
