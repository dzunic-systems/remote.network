package clientproto

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"remote.network/node/identity"
	"remote.network/node/wire"
)

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestReceiptSignVerify(t *testing.T) {
	nodePub, nodeKey := keypair(t)
	otherPub, _ := keypair(t)
	r, err := SignReceipt(nodeKey, wire.UsageReceipt{
		RequestID: "req-1", ModelID: "m", InputTokens: 100, OutputTokens: 42, Status: "ok", TTFTms: 120, TotalMs: 900, At: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.V != wire.ReceiptVersion || r.NodeID != identity.ID(nodePub) {
		t.Fatalf("stamped %+v", r)
	}
	if err := VerifyReceipt(identity.PubString(nodePub), r); err != nil {
		t.Fatalf("valid receipt refused: %v", err)
	}
	tampered := r
	tampered.OutputTokens = 4200
	if err := VerifyReceipt(identity.PubString(nodePub), tampered); !errors.Is(err, ErrReceiptSig) {
		t.Errorf("tampered count: %v, want ErrReceiptSig", err)
	}
	if err := VerifyReceipt(identity.PubString(otherPub), r); err == nil {
		t.Error("another node's key accepted the receipt")
	}
	newer := r
	newer.V = wire.ReceiptVersion + 1
	if err := VerifyReceipt(identity.PubString(nodePub), newer); !errors.Is(err, ErrReceiptVersion) {
		t.Errorf("newer version: %v, want ErrReceiptVersion", err)
	}
	// The receipt survives the wire: what the router decodes still verifies.
	env, err := wire.NewReq(wire.TypeInferenceDone, "", "req-1", wire.InferenceDoneData{OutputTokens: 42, Receipt: &r})
	if err != nil {
		t.Fatal(err)
	}
	var done wire.InferenceDoneData
	if err := json.Unmarshal(env.Data, &done); err != nil {
		t.Fatal(err)
	}
	if done.Receipt == nil || VerifyReceipt(identity.PubString(nodePub), *done.Receipt) != nil {
		t.Errorf("receipt did not survive the envelope: %+v", done.Receipt)
	}
}

type ticketFix struct {
	rootPub   ed25519.PublicKey
	routerKey ed25519.PrivateKey
	cert      identity.RouterCert
	nodeID    string
	now       time.Time
}

func newTicketFix(t *testing.T) ticketFix {
	rootPub, rootKey := keypair(t)
	routerPub, routerKey := keypair(t)
	nodePub, _ := keypair(t)
	cert, err := identity.SignRouterCert(rootKey, routerPub, time.Now().Add(24*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	return ticketFix{rootPub: rootPub, routerKey: routerKey, cert: cert, nodeID: identity.ID(nodePub), now: time.Unix(1_800_000_000, 0)}
}

func (f ticketFix) ticket(t *testing.T, mod func(*Ticket)) Ticket {
	t.Helper()
	clientPub, _ := keypair(t)
	tk := Ticket{
		ID: NewTicketID(), NodeID: f.nodeID, ModelID: "m", ClientPub: identity.PubString(clientPub),
		Account: "acct-hmac", PriceIn: 10, PriceOut: 25, MaxTokens: 2048, MaxReqs: 1,
		NotBefore: f.now.Unix(), NotAfter: f.now.Add(TicketLifetime).Unix(),
	}
	if mod != nil {
		mod(&tk)
	}
	signed, err := SignTicket(f.routerKey, f.cert, tk)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestTicketVerify(t *testing.T) {
	f := newTicketFix(t)
	good := f.ticket(t, nil)
	if err := VerifyTicket(good, f.rootPub, f.nodeID, f.now); err != nil {
		t.Fatalf("valid ticket refused: %v", err)
	}
	if good.RouterID != f.cert.RouterID || good.V != TicketVersion {
		t.Errorf("stamped %+v", good)
	}

	otherRootPub, otherRootKey := keypair(t)
	_ = otherRootPub
	routerPub := f.routerKey.Public().(ed25519.PublicKey)
	foreignCert, err := identity.SignRouterCert(otherRootKey, routerPub, time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := SignTicket(f.routerKey, foreignCert, Ticket{ID: "x", NodeID: f.nodeID, ClientPub: "c", MaxTokens: 1, NotBefore: f.now.Unix(), NotAfter: f.now.Unix() + 60})
	if err != nil {
		t.Fatal(err)
	}

	tampered := good
	tampered.MaxTokens = 1 << 30

	for _, c := range []struct {
		name string
		tk   Ticket
		node string
		at   time.Time
		want error
	}{
		{"another node", good, "someone-else", f.now, ErrTicketNode},
		{"tampered cap", tampered, f.nodeID, f.now, ErrTicketSig},
		{"router not certified by the pinned root", foreign, f.nodeID, f.now, ErrTicketRoot},
		{"expired", good, f.nodeID, f.now.Add(TicketLifetime + TicketSkew + time.Second), ErrTicketExpired},
		{"not yet valid", good, f.nodeID, f.now.Add(-TicketSkew - time.Second), ErrTicketNotYet},
		{"within skew after expiry", good, f.nodeID, f.now.Add(TicketLifetime + TicketSkew - time.Second), nil},
		{"within skew before start", good, f.nodeID, f.now.Add(-TicketSkew + time.Second), nil},
		{"newer version", func() Ticket { x := good; x.V = TicketVersion + 1; return x }(), f.nodeID, f.now, ErrTicketVersion},
		{"no client key", f.ticket(t, func(x *Ticket) { x.ClientPub = "" }), f.nodeID, f.now, ErrTicketMalformed},
		{"no output cap", f.ticket(t, func(x *Ticket) { x.MaxTokens = 0 }), f.nodeID, f.now, ErrTicketMalformed},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := VerifyTicket(c.tk, f.rootPub, c.node, c.at)
			if c.want == nil && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}

	// A router cannot sign with a key its certificate does not certify.
	_, wrongKey := keypair(t)
	if _, err := SignTicket(wrongKey, f.cert, Ticket{ID: "x"}); err == nil {
		t.Error("SignTicket accepted a key the certificate does not certify")
	}
	if a, b := NewTicketID(), NewTicketID(); a == b || len(a) != 26 {
		t.Errorf("ticket ids %q %q", a, b)
	}
}
