// quic.go: the node side of the hole punch. On punch.begin the agent
// opens ONE UDP socket, probes both router sockets (creating its NAT
// mapping and letting the router observe it), classifies its NAT from the
// router's punch.observed reply, and — when a candidate — listens for
// QUIC on that same socket so the router's dial lands on the fresh mapping.
// Every inference request then rides its own QUIC stream with the same
// envelope protocol as the relay.
package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"

	"remote.network/node/identity"
	"remote.network/node/wire"
)

// Punch timing (node side of the ladder).
const (
	probeInterval   = 150 * time.Millisecond // burst while waiting for observed
	probeWindow     = 6 * time.Second        // give up if unobserved by then
	holdInterval    = 1 * time.Second        // slow probes while router dials
	holdWindow      = 15 * time.Second
	quicIdleTimeout = 30 * time.Second
	quicKeepAlive   = 10 * time.Second
)

// punchAttempt is one begin→observed→classify→listen cycle.
type punchAttempt struct {
	nonce    string
	observed chan wire.PunchObservedData // router replies land here
	cancel   context.CancelFunc          // stops probes, closes socket/listener
	done     atomic.Bool                 // report sent
}

// punchBegin starts (or replaces) the punch cycle for this session.
func (c *Client) punchBegin(ctx context.Context, conn *websocket.Conn, nodeID string, d wire.PunchBeginData) {
	c.stopPunch()
	if !c.cfg.Direct {
		c.sendPunchReport(conn, nodeID, d.Nonce, wire.NATOpen, false, "direct disabled by flag")
		return
	}

	mainAddr, err1 := net.ResolveUDPAddr("udp", d.UDPAddr)
	altAddr, err2 := net.ResolveUDPAddr("udp", d.AltUDPAddr)
	if err1 != nil || err2 != nil {
		c.log.Warn("punch.begin: bad router addresses", "main", d.UDPAddr, "alt", d.AltUDPAddr)
		return
	}

	// The router's public QUIC endpoint is also the destination for the
	// node-dialed data plane (reverse.go): an outbound flow needs no NAT
	// traversal, so symmetric nodes — never punch candidates — get the
	// same direct plane. Arming it here is unconditional (Direct on): if
	// the punch dial also lands, UpgradeQUIC's swap settles the race.
	c.reverseDial(ctx, d.UDPAddr)
	sock, err := net.ListenUDP("udp", nil) // the ONE socket: probes + QUIC
	if err != nil {
		c.log.Warn("punch: udp socket", "err", err)
		return
	}

	pctx, cancel := context.WithCancel(ctx)
	attempt := &punchAttempt{nonce: d.Nonce, observed: make(chan wire.PunchObservedData, 4), cancel: cancel}
	c.punchMu.Lock()
	c.punchCur = attempt
	c.punchMu.Unlock()

	go c.punchCycle(pctx, attempt, sock, conn, nodeID, mainAddr, altAddr)
}

// stopPunch tears down any live attempt.
func (c *Client) stopPunch() {
	c.punchMu.Lock()
	p := c.punchCur
	c.punchCur = nil
	c.punchMu.Unlock()
	if p != nil {
		p.cancel()
	}
}

// routeObserved delivers a punch.observed envelope to the live attempt.
func (c *Client) routeObserved(d wire.PunchObservedData) {
	c.punchMu.Lock()
	p := c.punchCur
	c.punchMu.Unlock()
	if p != nil && p.nonce == d.Nonce {
		select {
		case p.observed <- d:
		default:
		}
	}
}

// punchCycle probes, classifies, reports, and (when candidate) serves QUIC.
func (c *Client) punchCycle(ctx context.Context, attempt *punchAttempt, sock *net.UDPConn, conn *websocket.Conn, nodeID string, mainAddr, altAddr *net.UDPAddr) {
	local := sock.LocalAddr().String()

	// Burst probes at both sockets until observed (or window end).
	obs, ok := c.probeBurst(ctx, attempt, sock, nodeID, mainAddr, altAddr)
	if !ok {
		sock.Close()
		c.sendPunchReport(conn, nodeID, attempt.nonce, "unknown", false, "router never observed probes")
		return
	}

	natType, candidate := classifyNAT(local, obs.MainAddr, obs.AltAddr)
	if c.cfg.SimulateSymmetric {
		natType, candidate = wire.NATSymmetric, false
	}
	c.log.Info("punch observed", "nat", natType, "local", local,
		"observed_main", obs.MainAddr, "observed_alt", obs.AltAddr, "candidate", candidate)

	if !candidate {
		sock.Close()
		c.sendPunchReport(conn, nodeID, attempt.nonce, natType, false, "NAT policy: relay-only")
		return
	}

	// Candidate: listen for the router's dial on the SAME socket, keep the
	// mapping warm with slow probes while it dials, then report readiness.
	cert, err := identity.Certificate(c.cfg.Identity)
	if err != nil {
		sock.Close()
		c.log.Warn("punch: identity cert", "err", err)
		c.sendPunchReport(conn, nodeID, attempt.nonce, natType, false, "cert error")
		return
	}
	routerPub, _ := c.routerPub.Load().(string)
	transport := &quic.Transport{Conn: sock}
	listener, err := transport.Listen(
		identity.TLSServerConfig(cert, routerPub),
		&quic.Config{MaxIdleTimeout: quicIdleTimeout, KeepAlivePeriod: quicKeepAlive},
	)
	if err != nil {
		sock.Close()
		c.log.Warn("punch: quic listen", "err", err)
		c.sendPunchReport(conn, nodeID, attempt.nonce, natType, false, "quic listen failed")
		return
	}

	// Keep the NAT mapping fresh through the router's dial ladder.
	go c.holdProbes(ctx, attempt, sock, nodeID, mainAddr, altAddr)

	c.sendPunchReport(conn, nodeID, attempt.nonce, natType, true, "")

	go c.acceptQUIC(ctx, listener)
	c.log.Info("direct QUIC listener armed", "local", local, "nat", natType)
}

// probeBurst sends paired probes until the router reports observations.
func (c *Client) probeBurst(ctx context.Context, attempt *punchAttempt, sock *net.UDPConn, nodeID string, mainAddr, altAddr *net.UDPAddr) (wire.PunchObservedData, bool) {
	deadline := time.After(probeWindow)
	seq := 0
	for {
		select {
		case <-ctx.Done():
			return wire.PunchObservedData{}, false
		case obs := <-attempt.observed:
			return obs, true
		case <-deadline:
			return wire.PunchObservedData{}, false
		case <-time.After(probeInterval):
			seq++
			payload := wire.EncodeProbe(wire.Probe{NodeID: nodeID, Nonce: attempt.nonce, Seq: seq})
			_, _ = sock.WriteToUDP(payload, mainAddr)
			_, _ = sock.WriteToUDP(payload, altAddr)
		}
	}
}

// holdProbes keeps the mapping alive at low rate while the router dials.
func (c *Client) holdProbes(ctx context.Context, attempt *punchAttempt, sock *net.UDPConn, nodeID string, mainAddr, altAddr *net.UDPAddr) {
	t := time.NewTicker(holdInterval)
	defer t.Stop()
	seq := 10_000 // separate range from the burst, for log grepping
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(holdWindow):
			return // dial window over; QUIC keepalives own the mapping now
		case <-t.C:
			seq++
			payload := wire.EncodeProbe(wire.Probe{NodeID: nodeID, Nonce: attempt.nonce, Seq: seq})
			_, _ = sock.WriteToUDP(payload, mainAddr)
			_, _ = sock.WriteToUDP(payload, altAddr)
		}
	}
}

// acceptQUIC serves direct connections: one stream per inference request,
// same envelopes as the relay, same handler core.
func (c *Client) acceptQUIC(ctx context.Context, listener *quic.Listener) {
	defer listener.Close()
	for {
		qconn, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, quic.ErrServerClosed) {
				c.log.Debug("quic accept ended", "err", err)
			}
			return
		}
		c.log.Info("direct QUIC connection accepted", "remote", qconn.RemoteAddr().String())
		go func() {
			for {
				stream, err := qconn.AcceptStream(ctx)
				if err != nil {
					return
				}
				go c.serveQUICStream(stream)
			}
		}()
	}
}

// serveQUICStream serves one request over one stream. Router-side cancel
// resets the stream, which cancels stream.Context() → generation stops.
func (c *Client) serveQUICStream(stream *quic.Stream) {
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 64*1024), 2<<20)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			c.log.Warn("quic stream: request read failed", "err", err)
		} else {
			c.log.Info("quic stream: clean EOF before any request bytes")
		}
		return
	}
	var env wire.Envelope
	if json.Unmarshal(sc.Bytes(), &env) != nil ||
		(env.Type != wire.TypeInferenceRequest && env.Type != wire.TypeEmbedRequest) {
		c.log.Warn("quic stream: not an inference request", "type", env.Type)
		return
	}

	writeEnv := func(typ string, reqID string, v any) error {
		out, err := wire.NewReq(typ, "", reqID, v)
		if err != nil {
			return err
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		_ = stream.SetWriteDeadline(time.Now().Add(writeWait))
		_, err = stream.Write(append(b, '\n'))
		return err
	}

	// The stream's context dies on reset/close — that is the cancel path.
	sctx := stream.Context()
	if env.Type == wire.TypeEmbedRequest {
		c.serveQUICEmbed(sctx, stream, env, writeEnv)
		return
	}

	var req wire.InferenceRequestData
	if json.Unmarshal(env.Data, &req) != nil {
		c.log.Warn("quic stream: bad request data", "req_id", env.ReqID)
		return
	}
	done, code, err := c.runHandler(sctx, env.ReqID, req, func(chunk wire.InferenceChunkData) error {
		return writeEnv(wire.TypeInferenceChunk, env.ReqID, chunk)
	})
	if code != "" {
		if werr := writeEnv(wire.TypeInferenceError, env.ReqID, wire.ErrorData{Code: code, Message: err.Error()}); werr != nil {
			c.log.Warn("quic stream: error envelope write failed", "req_id", env.ReqID, "err", werr)
		}
	} else if werr := writeEnv(wire.TypeInferenceDone, env.ReqID, done); werr != nil {
		c.log.Warn("quic stream: done envelope write failed", "req_id", env.ReqID, "err", werr)
	}
	// FIN — clean end. Never CancelWrite here: a RESET discards response
	// data the peer may not have received yet.
	_ = stream.Close()
}

// serveQUICEmbed is the direct-plane embedding leg: same
// handler, terminal embed_done or inference.error, then FIN.
func (c *Client) serveQUICEmbed(sctx context.Context, stream *quic.Stream, env wire.Envelope, writeEnv func(typ, reqID string, v any) error) {
	var req wire.EmbedRequestData
	if json.Unmarshal(env.Data, &req) != nil {
		c.log.Warn("quic stream: bad embed request data", "req_id", env.ReqID)
		return
	}
	eh, ok := c.cfg.Handler.(EmbedHandler)
	if !ok {
		_ = writeEnv(wire.TypeInferenceError, env.ReqID, wire.ErrorData{Code: "no_handler", Message: "node agent cannot embed"})
		_ = stream.Close()
		return
	}
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	done, err := eh.HandleEmbed(sctx, req)
	if err != nil {
		code := "backend_error"
		if errors.Is(err, wire.ErrBusy) {
			code = "busy"
		}
		if werr := writeEnv(wire.TypeInferenceError, env.ReqID, wire.ErrorData{Code: code, Message: err.Error()}); werr != nil {
			c.log.Warn("quic stream: embed error write failed", "req_id", env.ReqID, "err", werr)
		}
	} else if werr := writeEnv(wire.TypeEmbedDone, env.ReqID, done); werr != nil {
		c.log.Warn("quic stream: embed done write failed", "req_id", env.ReqID, "err", werr)
	}
	_ = stream.Close()
}

// sendPunchReport reports the node's verdict over the WSS control channel.
func (c *Client) sendPunchReport(conn *websocket.Conn, nodeID, nonce, natType string, candidate bool, detail string) {
	err := c.send(conn, wire.TypePunchReport, nodeID, wire.PunchReportData{
		Nonce: nonce, NATType: natType, Candidate: candidate, Detail: detail,
	})
	if err != nil {
		c.log.Debug("punch.report undeliverable", "err", err)
	} else {
		c.log.Info("punch reported", "nat", natType, "candidate", candidate, "detail", detail)
	}
}

// classifyNAT decides candidacy from what the router observed:
//
//	local == observed (both)  → open (no translation; same network)
//	observed main == alt      → cone → candidate (router dials observed main)
//	otherwise                 → symmetric → relay fallback (by policy)
func classifyNAT(local, main, alt string) (string, bool) {
	if main == "" || alt == "" {
		return "unknown", false
	}
	if main == local && alt == local {
		return wire.NATOpen, true
	}
	if main == alt {
		return wire.NATCone, true
	}
	return wire.NATSymmetric, false
}
