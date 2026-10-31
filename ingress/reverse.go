// reverse.go: the node-dialed data plane. A symmetric NAT never becomes a
// punch candidate — the router's dial lands on a mapping the node's NAT
// rebinds per destination — but none of that matters for an OUTBOUND
// flow. On every punch.begin the agent learns the router's public QUIC
// address; from there it dials out, presents the identity certificate its
// WSS session proved, and serves the same one-stream-per-request protocol
// as the punch listener. The router's reverse listener maps the
// connection back to this session; dispatches then ride direct-quic with
// transparent relay fallback. Dial + serve loop mirrors the punch ladder's
// lifecycle: armed per session, retried within the attempt, re-armed by
// the next punch.begin.
package ingress

import (
	"context"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/identity"
)

// Reverse-dial timing.
const (
	reverseDialTimeout = 5 * time.Second
	reverseAttempts    = 3
	reverseBackoff     = 2 * time.Second
)

// reverseDial opens the node→router data plane and serves it until the
// connection (or session) dies. udpAddr is the router's public QUIC
// endpoint as carried by punch.begin. One attempt chain at a time — the
// atomic guard also tells later punch.begins that a plane is being
// established or live, so they skip re-dialing.
func (c *Client) reverseDial(ctx context.Context, udpAddr string) {
	if !c.cfg.Direct || c.cfg.NoReverseQUIC || udpAddr == "" {
		return
	}
	if !c.revDialing.CompareAndSwap(false, true) {
		return // a plane is live or already dialing
	}
	go func() {
		defer c.revDialing.Store(false)

		cert, err := identity.Certificate(c.cfg.Identity)
		if err != nil {
			c.log.Warn("reverse quic: identity cert", "err", err)
			return
		}
		routerPub, _ := c.routerPub.Load().(string)
		if routerPub == "" {
			return // TOFU pin not settled yet; the next punch.begin re-arms
		}

		backoff := reverseBackoff
		for attempt := 0; attempt < reverseAttempts; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff *= 2
			}
			dctx, cancel := context.WithTimeout(ctx, reverseDialTimeout)
			qconn, err := quic.DialAddr(dctx, udpAddr,
				identity.TLSClientConfig(cert, routerPub),
				&quic.Config{MaxIdleTimeout: quicIdleTimeout, KeepAlivePeriod: quicKeepAlive})
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				c.log.Debug("reverse quic dial failed", "addr", udpAddr, "attempt", attempt+1, "err", err)
				continue
			}
			c.log.Info("reverse QUIC established (node-dialed)", "remote", udpAddr)
			c.serveQUICConn(ctx, qconn) // blocks until conn or session dies
			if ctx.Err() != nil {
				return
			}
			// Connection lost — fall through and retry while the window
			// lasts; the next punch.begin re-arms afterwards.
		}
	}()
}

// serveQUICConn accepts router-opened streams on the node-dialed
// connection — identical serving to the punch listener's accept loop.
func (c *Client) serveQUICConn(ctx context.Context, qconn *quic.Conn) {
	defer qconn.CloseWithError(0, "session ended")
	for {
		stream, err := qconn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go c.serveQUICStream(stream)
	}
}
