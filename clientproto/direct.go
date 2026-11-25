package clientproto

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/identity"
	"remote.network/node/wire"
)

// The direct client→node connection: QUIC,
// TLS 1.3 with both sides presenting identity certificates, ALPN
// wire.DirectClientALPN. The client pins the node key the router named;
// the node accepts any identity key at the TLS layer and authorises
// per stream (tickets).

// ClientListenerTLS is the node side: its identity certificate, any
// well-formed client identity, the client ALPN only.
func ClientListenerTLS(nodeKey ed25519.PrivateKey) (*tls.Config, error) {
	cert, err := identity.Certificate(nodeKey)
	if err != nil {
		return nil, err
	}
	cfg := identity.TLSServerConfigAnyPeer(cert)
	cfg.NextProtos = []string{wire.DirectClientALPN}
	return cfg, nil
}

// clientTLS is the client side: its own identity certificate, the node's
// key pinned.
func clientTLS(clientKey ed25519.PrivateKey, nodePub string) (*tls.Config, error) {
	cert, err := identity.Certificate(clientKey)
	if err != nil {
		return nil, err
	}
	cfg := identity.TLSClientConfig(cert, nodePub)
	cfg.NextProtos = []string{wire.DirectClientALPN}
	return cfg, nil
}

// DirectQUICConfig is both ends' QUIC settings.
func DirectQUICConfig() *quic.Config {
	return &quic.Config{MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, HandshakeIdleTimeout: 5 * time.Second}
}

// ErrNoDirectPath means no endpoint completed a handshake in time.
var ErrNoDirectPath = errors.New("no direct path to the node")

// DialNode races QUIC handshakes to a node's endpoints (happy eyeballs: a
// new attempt every stagger until one completes) over the caller's
// transport — the socket whose public address the router observed, so the
// node's punch burst toward it has opened the way. The node's identity key
// is pinned; the first completed handshake wins, the rest are abandoned.
func DialNode(ctx context.Context, tr *quic.Transport, endpoints []string, clientKey ed25519.PrivateKey, nodePub string) (*quic.Conn, error) {
	if len(endpoints) == 0 {
		return nil, ErrNoDirectPath
	}
	tlsCfg, err := clientTLS(clientKey, nodePub)
	if err != nil {
		return nil, err
	}
	const stagger = 250 * time.Millisecond
	type result struct {
		ep   string
		conn *quic.Conn
		err  error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(endpoints))
	for i, ep := range endpoints {
		addr, err := net.ResolveUDPAddr("udp", ep)
		if err != nil {
			results <- result{ep: ep, err: err}
			continue
		}
		go func(i int, ep string, addr *net.UDPAddr) {
			select {
			case <-time.After(time.Duration(i) * stagger):
			case <-ctx.Done():
				results <- result{ep: ep, err: ctx.Err()}
				return
			}
			conn, err := tr.Dial(ctx, addr, tlsCfg.Clone(), DirectQUICConfig())
			results <- result{ep: ep, conn: conn, err: err}
		}(i, ep, addr)
	}
	// Every endpoint's own failure is kept: the last one alone hides the
	// informative one (a fast key mismatch on the LAN address behind a slow
	// timeout on the public one).
	var errs []string
	for range endpoints {
		r := <-results
		if r.err == nil {
			cancel() // abandon the others
			go func() {
				for range len(endpoints) - 1 {
					if late := <-results; late.conn != nil {
						_ = late.conn.CloseWithError(0, "another path won")
					}
				}
			}()
			return r.conn, nil
		}
		errs = append(errs, r.ep+": "+r.err.Error())
	}
	sort.Strings(errs)
	return nil, fmt.Errorf("%w: %s", ErrNoDirectPath, strings.Join(errs, "; "))
}

// DirectRequestData is the first envelope on a direct stream: the
// router-signed ticket, the client's request id (what the receipt and the
// ledger key by), and the inference request itself (its Model must equal
// the ticket's ModelID — the node-side id the router put in the ticket).
// The type constant lives on the wire (wire cannot import clientproto).
type DirectRequestData struct {
	Ticket    Ticket                    `json:"ticket"`
	RequestID string                    `json:"request_id"`
	Request   wire.InferenceRequestData `json:"request"`
}

// SendDirectRequest writes the request envelope and returns the stream
// for reading the node's chunk/done/error envelopes.
func SendDirectRequest(stream deadlineWriter, d DirectRequestData, wait time.Duration) error {
	env, err := wire.New(wire.TypeDirectRequest, "", d)
	if err != nil {
		return err
	}
	return WriteEnvelope(stream, env, wait)
}

// Ping opens a stream, sends direct.ping and reads the node's answer.
func Ping(ctx context.Context, conn *quic.Conn) (wire.DirectPongData, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return wire.DirectPongData{}, err
	}
	defer stream.Close()
	env, err := wire.New(wire.TypeDirectPing, "", struct{}{})
	if err != nil {
		return wire.DirectPongData{}, err
	}
	if err := WriteEnvelope(stream, env, 5*time.Second); err != nil {
		return wire.DirectPongData{}, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = stream.SetReadDeadline(dl)
	}
	sc := NewEnvelopeScanner(stream)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return wire.DirectPongData{}, err
		}
		return wire.DirectPongData{}, errors.New("node closed the stream without answering")
	}
	var reply wire.Envelope
	if err := json.Unmarshal(sc.Bytes(), &reply); err != nil {
		return wire.DirectPongData{}, err
	}
	if reply.Type != wire.TypeDirectPong {
		return wire.DirectPongData{}, fmt.Errorf("node answered %q", reply.Type)
	}
	var pong wire.DirectPongData
	err = json.Unmarshal(reply.Data, &pong)
	return pong, err
}
