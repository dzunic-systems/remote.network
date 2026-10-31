// Package ingress is the node agent's outbound control channel to a router.
// The WSS path is always on; the direct QUIC path sits behind
// the same contract so the agent never knows which transport serves it.
package ingress

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"remote.network/node/identity"
	"remote.network/node/wire"
)

// Handshake/liveness limits (mirror routerd's).
const (
	handshakeWait = 10 * time.Second
	readWait      = 30 * time.Second
	writeWait     = 10 * time.Second
	maxMessage    = 33 << 20
	reconnectMin  = 1 * time.Second
	reconnectMax  = 10 * time.Second
	stableAfter   = 60 * time.Second // reset backoff after this much uptime

	routerPinFile = "router_key.pin"
	routerRootPin = "router_root.pin"
	stateFile     = "state.json"
)

// InferenceHandler serves router-dispatched requests against the local
// backend (implemented by serve.Runtime). Emit forwards one wire chunk; an
// error from emit aborts the stream.
type InferenceHandler interface {
	HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error)
}

// EmbedHandler is the optional embeddings surface: when the Handler also
// implements it, inference.embed_request dispatches are answered; otherwise
// they get no_handler.
type EmbedHandler interface {
	HandleEmbed(ctx context.Context, req wire.EmbedRequestData) (wire.EmbedDoneData, error)
}

// Config for the WSS control client.
type Config struct {
	RouterURL    string // ws(s)://host/node/v1/connect
	Identity     ed25519.PrivateKey
	Name         string
	AgentVersion string
	HomeDir      string // ~/.remote — pin + state live here
	RouterKey    string // explicit pin (base32); empty = TOFU with persisted pin
	// RouterRoot is the company root public key to pin (--router-root,
	// base32). When set, only routers chaining to THAT root are accepted;
	// empty = TOFU the root from the first certificate a router presents
	// (the same first-contact trust decision the key pin always made).
	// NodeToken, when set, is an rn_nt_… registration token claiming this
	// node for a dashboard account. Sent in hello; a refused claim is a
	// terminal error (no retry loop) — mint a new token or drop the flag.
	RouterRoot string
	// OnDirectory receives the verified discoverable-router set each time
	// the serving router advertises one (router.directory). Entries are
	// pre-filtered: only certs chaining to this node's pinned root. Nil =
	// ignored.
	OnDirectory func(entries []wire.DirectoryEntry)
	NodeToken   string

	// OnReprobe executes a router-initiated re-characterization
	// (node.reprobe): re-list + force-scoped re-probe, returning whether
	// the serving set changed. Nil = reprobe requests are ignored.
	OnReprobe func(all bool, models []string) bool

	Log *slog.Logger

	// Capability returns the advertisement blob sent in hello. Called once
	// per connect so a reconnect refreshes hardware/backend info.
	Capability func() (json.RawMessage, error)

	// Handler serves inference requests; nil rejects them with no_handler.
	Handler InferenceHandler

	// Direct enables the hole-punched QUIC data plane; false keeps
	// the session relay-only.
	Direct bool
	// SimulateSymmetric forces the punch report to claim a symmetric NAT
	// (verification hook for the relay-fallback path).
	SimulateSymmetric bool
	// NoReverseQUIC disables the node-dialed data plane (reverse.go): the
	// outbound QUIC connection to the router's public endpoint that gives
	// symmetric-NAT nodes a direct plane without any hole punching.
	NoReverseQUIC bool
}

// Client maintains the control channel with reconnect + backoff.
type Client struct {
	cfg Config
	log *slog.Logger

	capabilityCache json.RawMessage // last advertised, for `remote node status`

	servingMu sync.Mutex
	serving   []string // canonical model ids reported in heartbeats

	routerPub atomic.Value // string: router's base32 public key, set at auth

	dirMu      sync.Mutex
	rootPubB32 string                // pinned company root (set when a handshake chained)
	directory  []wire.DirectoryEntry // verified discoverable routers (router.directory)

	sendMu   sync.Mutex      // serializes socket writes (heartbeats + inference)
	curConn  *websocket.Conn // live session socket (guarded by sendMu)
	inFlight atomic.Int64
	cancels  sync.Map // req_id → context.CancelFunc for live dispatches

	reprobeMu   sync.Mutex
	lastReprobe time.Time

	punchMu  sync.Mutex
	punchCur *punchAttempt // active hole-punch attempt, if any

	revDialing atomic.Bool // reverse data plane live or dialing (see reverse.go)
}

// SetServingModels updates the heartbeat's serving list (from serve.Runtime).
func (c *Client) SetServingModels(ids []string) {
	c.servingMu.Lock()
	c.serving = ids
	c.servingMu.Unlock()
}

// currentConn returns the live session socket (nil when disconnected).
func (c *Client) currentConn() *websocket.Conn {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.curConn
}

func (c *Client) currentServing() []string {
	c.servingMu.Lock()
	defer c.servingMu.Unlock()
	return c.serving
}

// New builds a client.
func New(cfg Config) *Client {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Capability == nil {
		cfg.Capability = func() (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	}
	return &Client{cfg: cfg, log: cfg.Log}
}

// Run connects, authenticates, heartbeats, and reconnects until ctx is done.
// Returns nil on ctx cancellation; a router-key pin mismatch is fatal.
func (c *Client) Run(ctx context.Context) error {
	backoff := reconnectMin
	for {
		uptime, err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, errPinMismatch):
			return err // security: never auto-retry past a key mismatch
		case errors.Is(err, ErrNodeClaim):
			return err // the token was refused — retrying can't help
		case err != nil:
			c.log.Warn("router session ended", "err", err, "reconnect_in", backoff.String())
		}
		c.writeState(false, "")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if uptime >= stableAfter {
			backoff = reconnectMin
		} else if backoff < reconnectMax {
			backoff *= 2
			if backoff > reconnectMax {
				backoff = reconnectMax
			}
		}
	}
}

var errPinMismatch = errors.New("router public key does not match pinned key")

// ErrPinMismatch is the exported face of the pin failure: Run returns it
// instead of retrying (a pin mismatch is a security event, not a network
// hiccup) — callers match it with errors.Is.
var ErrPinMismatch = errPinMismatch

// ErrNodeClaim marks a refused node-token claim (invalid/revoked token, or
// the node already owned by another account) — terminal like a pin
// mismatch, because reconnecting with the same credential repeats it.
var ErrNodeClaim = errors.New("node token rejected")

// session dials, handshakes, then blocks in the read loop. Returns uptime.
func (c *Client) session(ctx context.Context) (time.Duration, error) {
	started := time.Now()

	dialer := &websocket.Dialer{HandshakeTimeout: handshakeWait}
	conn, _, err := dialer.DialContext(ctx, c.cfg.RouterURL, nil)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	nodeID := identity.ID(c.cfg.Identity.Public().(ed25519.PublicKey))

	capability, err := c.cfg.Capability()
	if err != nil {
		conn.Close()
		return 0, err
	}
	c.capabilityCache = capability

	if err := c.send(conn, wire.TypeNodeHello, nodeID, wire.HelloData{
		NodeID:       nodeID,
		PublicKey:    identity.PubString(c.cfg.Identity.Public().(ed25519.PublicKey)),
		Name:         c.cfg.Name,
		AgentVersion: c.cfg.AgentVersion,
		Capability:   capability,
		NodeToken:    c.cfg.NodeToken,
	}); err != nil {
		conn.Close()
		return 0, err
	}

	// Router challenge — verify the router against our pin (TOFU on first use).
	env, err := c.recv(conn, wire.TypeNodeChallenge)
	if err != nil {
		conn.Close()
		return 0, err
	}
	var ch wire.ChallengeData
	if err := json.Unmarshal(env.Data, &ch); err != nil {
		conn.Close()
		return 0, fmt.Errorf("malformed challenge: %w", err)
	}
	routerPub, err := identity.PubFromString(ch.PublicKey)
	nonce, nonceErr := base64.StdEncoding.DecodeString(ch.Nonce)
	if err != nil || nonceErr != nil ||
		!identity.VerifyChallenge(routerPub, nonce, ch.RouterSig) ||
		identity.ID(routerPub) != ch.RouterID {
		conn.Close()
		return 0, errors.New("router challenge invalid")
	}
	// A presented certificate re-anchors trust at the company
	// root — the per-router key pin does not apply, so the company can
	// rotate router keys without touching nodes. No cert: exactly the
	// pre-phase-B path. A cert that fails verification is fatal (the
	// router chose to send it; falling back would mask misconfiguration
	// and downgrade).
	rootPubB32, err := c.verifyCert(&ch, routerPub)
	if err != nil {
		conn.Close()
		return 0, err
	}
	if rootPubB32 != "" {
		if err := c.checkRootPin(rootPubB32); err != nil {
			conn.Close()
			return 0, err
		}
		c.setRootPub(rootPubB32) // directory entries verify against this
	} else if err := c.checkPin(ch.PublicKey); err != nil {
		conn.Close()
		return 0, err
	}

	if err := c.send(conn, wire.TypeNodeChallengeResponse, nodeID, wire.ChallengeResponseData{
		Nonce:     ch.Nonce,
		Signature: identity.SignChallenge(c.cfg.Identity, nonce),
	}); err != nil {
		conn.Close()
		return 0, err
	}

	authEnv, err := c.recv(conn, wire.TypeNodeAuthOK)
	if err != nil {
		conn.Close()
		return 0, fmt.Errorf("auth: %w", err)
	}
	var authOK wire.AuthOKData
	if err := json.Unmarshal(authEnv.Data, &authOK); err != nil {
		conn.Close()
		return 0, fmt.Errorf("malformed auth_ok: %w", err)
	}

	c.log.Info("authenticated with router", "node_id", nodeID, "router_id", authOK.RouterID)
	c.routerPub.Store(ch.PublicKey) // verified against the pin above
	c.sendMu.Lock()
	c.curConn = conn
	c.sendMu.Unlock()
	c.writeState(true, authOK.RouterID)

	interval := time.Duration(authOK.HeartbeatIntervalS) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go c.heartbeatLoop(hbCtx, conn, nodeID, interval)

	// Cancel closes the socket: the read loop only checks ctx between
	// frames, so a quiet router would otherwise hold a cancelled session
	// open for a full read deadline (fleet detach, shutdown).
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close() // wakes ReadMessage with an error
		case <-stopClose:
		}
	}()

	err = c.readLoop(ctx, conn, nodeID)
	stopHB()
	c.stopPunch() // the punch attempt's socket/listener dies with the session
	c.sendMu.Lock()
	c.curConn = nil
	c.sendMu.Unlock()
	conn.Close()
	return time.Since(started), err
}

// readLoop consumes router→node messages until error/ctx-done. Inference
// dispatches spawn their own goroutine; everything else is handled inline.
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn, nodeID string) error {
	conn.SetReadLimit(maxMessage)
	_ = conn.SetReadDeadline(time.Now().Add(readWait))
	// Router keepalive pings must extend the read deadline (gorilla's
	// default ping handler only pongs); otherwise a quiet router — no
	// dispatches — looks like a dead one at readWait. Our heartbeats cover
	// the router's read deadline in the other direction.
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
	})
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readWait))
	})
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		var env wire.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.V > wire.Version {
			c.log.Warn("dropping malformed envelope", "err", err)
			continue
		}
		switch env.Type {
		case wire.TypeInferenceRequest:
			var req wire.InferenceRequestData
			if err := json.Unmarshal(env.Data, &req); err != nil {
				c.log.Warn("malformed inference.request", "req_id", env.ReqID, "err", err)
				continue
			}
			go c.serveInference(ctx, conn, nodeID, env.ReqID, req)
		case wire.TypeEmbedRequest:
			var req wire.EmbedRequestData
			if err := json.Unmarshal(env.Data, &req); err != nil {
				c.log.Warn("malformed inference.embed_request", "req_id", env.ReqID, "err", err)
				continue
			}
			go c.serveEmbed(ctx, conn, nodeID, env.ReqID, req)
		case wire.TypeInferenceCancel:
			if cancel, ok := c.cancels.LoadAndDelete(env.ReqID); ok && env.ReqID != "" {
				cancel.(context.CancelFunc)()
			}
		case wire.TypePunchBegin:
			var d wire.PunchBeginData
			if err := json.Unmarshal(env.Data, &d); err != nil {
				continue
			}
			go c.punchBegin(ctx, conn, nodeID, d)
		case wire.TypePunchObserved:
			var d wire.PunchObservedData
			if err := json.Unmarshal(env.Data, &d); err != nil {
				continue
			}
			c.routeObserved(d)
		case wire.TypeNodeError:
			var d wire.ErrorData
			_ = json.Unmarshal(env.Data, &d)
			c.log.Warn("router error", "code", d.Code, "message", d.Message)
		case wire.TypeRouterDirectory:
			var d wire.DirectoryData
			if err := json.Unmarshal(env.Data, &d); err != nil {
				c.log.Warn("malformed router.directory", "err", err)
				continue
			}
			c.noteDirectory(d)
		case wire.TypeNodeDrain:
			c.log.Info("router requested drain", "data", string(env.Data))
		case wire.TypeNodeReprobe:
			var d wire.ReprobeData
			if err := json.Unmarshal(env.Data, &d); err != nil {
				continue
			}
			go c.handleReprobe(d)
		default:
			c.log.Debug("ignoring router message", "type", env.Type)
		}
	}
}

// serveInference drives one relay-dispatched request to completion.
// Cancellation (consumer disconnect) arrives as inference.cancel → ctx,
// which also stops the backend generation.
func (c *Client) serveInference(ctx context.Context, conn *websocket.Conn, nodeID, reqID string, req wire.InferenceRequestData) {
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.cancels.Store(reqID, cancel)
	defer c.cancels.Delete(reqID)

	if c.cfg.Handler == nil {
		c.sendReqError(conn, nodeID, reqID, "no_handler", "node agent has no serving backend")
		return
	}

	done, code, err := c.runHandler(ictx, reqID, req, func(chunk wire.InferenceChunkData) error {
		return c.sendReq(conn, nodeID, reqID, wire.TypeInferenceChunk, chunk)
	})
	if code != "" {
		c.sendReqError(conn, nodeID, reqID, code, err.Error())
		return
	}
	_ = c.sendReq(conn, nodeID, reqID, wire.TypeInferenceDone, done)
}

// serveEmbed drives one relay-dispatched embedding batch: one
// round trip, terminal embed_done or inference.error. The embedder counts
// toward in-flight so drain and the router's load view see it.
func (c *Client) serveEmbed(ctx context.Context, conn *websocket.Conn, nodeID, reqID string, req wire.EmbedRequestData) {
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.cancels.Store(reqID, cancel)
	defer c.cancels.Delete(reqID)

	eh, ok := c.cfg.Handler.(EmbedHandler)
	if !ok {
		c.sendReqError(conn, nodeID, reqID, "no_handler", "node agent cannot embed")
		return
	}

	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	done, err := eh.HandleEmbed(ictx, req)
	if err != nil {
		code := "backend_error"
		if errors.Is(err, wire.ErrBusy) {
			code = "busy"
		}
		c.log.Warn("embed failed", "req_id", reqID, "model", req.Model, "code", code, "err", err)
		c.sendReqError(conn, nodeID, reqID, code, err.Error())
		return
	}
	_ = c.sendReq(conn, nodeID, reqID, wire.TypeEmbedDone, done)
}

// SendCapabilityUpdate pushes a fresh advertisement over the live control
// channel: the router re-reads the cards without a reconnect.
// Best-effort — no live session or a failed write just returns false; the
// next reconnect re-advertises via hello anyway.
func (c *Client) SendCapabilityUpdate() bool {
	conn := c.currentConn()
	if conn == nil {
		return false
	}
	nodeID := identity.ID(c.cfg.Identity.Public().(ed25519.PublicKey))
	capability, err := c.cfg.Capability()
	if err != nil {
		return false
	}
	c.capabilityCache = capability
	return c.send(conn, wire.TypeNodeCapabilityUpdate, nodeID, wire.CapabilityUpdateData{
		Capability: capability,
	}) == nil
}

// handleReprobe executes a router-initiated re-probe: re-characterize per
// the request's scope (ForceReprobe keeps the cache discipline — only what
// is forced re-probes), then push the fresh cards. Rate-limited to one
// execution per minute: a re-probe re-benches, and a noisy router must not
// turn the node into a bench farm.
func (c *Client) handleReprobe(d wire.ReprobeData) {
	c.reprobeMu.Lock()
	if time.Since(c.lastReprobe) < time.Minute {
		c.reprobeMu.Unlock()
		c.log.Warn("node.reprobe rate limited (min interval 1m)")
		return
	}
	c.lastReprobe = time.Now()
	c.reprobeMu.Unlock()

	if c.cfg.OnReprobe == nil {
		c.log.Warn("node.reprobe ignored — no reprobe handler registered")
		return
	}
	changed := c.cfg.OnReprobe(d.All, d.Models)
	c.log.Info("router-initiated re-probe done", "all", d.All, "models", d.Models, "changed", changed)
	if changed {
		c.SendCapabilityUpdate()
	}
}

// runHandler is the shared request core for both data planes (WSS relay and
// direct QUIC): concurrency slot, handler invocation, busy mapping. The
// non-empty code return means "terminal error with that wire code".
func (c *Client) runHandler(ctx context.Context, reqID string, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (done wire.InferenceDoneData, code string, err error) {
	if c.cfg.Handler == nil {
		return wire.InferenceDoneData{}, "no_handler", errors.New("node agent has no serving backend")
	}
	c.inFlight.Add(1)
	defer c.inFlight.Add(-1)

	done, err = c.cfg.Handler.HandleInference(ctx, req, emit)
	if err != nil {
		code = "backend_error"
		if errors.Is(err, wire.ErrBusy) {
			code = "busy"
		}
		c.log.Warn("inference failed", "req_id", reqID, "model", req.Model, "code", code, "err", err)
	}
	return done, code, err
}

func (c *Client) heartbeatLoop(ctx context.Context, conn *websocket.Conn, nodeID string, interval time.Duration) {
	// One beat immediately: the router's serving set (and the reaper) should
	// never wait a full interval after connect.
	if err := c.sendHeartbeat(conn, nodeID); err != nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.sendHeartbeat(conn, nodeID); err != nil {
				return // readLoop notices the dead socket and tears down
			}
		}
	}
}

func (c *Client) sendHeartbeat(conn *websocket.Conn, nodeID string) error {
	return c.send(conn, wire.TypeNodeHeartbeat, nodeID, wire.HeartbeatData{
		InFlight:      int(c.inFlight.Load()),
		ServingModels: c.currentServing(),
	})
}

// checkPin enforces router identity: explicit flag pin > persisted TOFU pin.
// First contact persists the key and logs the fingerprint.
func (c *Client) checkPin(routerPubB32 string) error {
	pinPath := filepath.Join(c.cfg.HomeDir, routerPinFile)
	want := c.cfg.RouterKey
	if want == "" {
		b, err := os.ReadFile(pinPath)
		if err == nil {
			want = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if want != "" {
		if want != routerPubB32 {
			return fmt.Errorf("%w: pinned=%s got=%s (%s)", errPinMismatch, short(want), short(routerPubB32), pinPath)
		}
		return nil
	}
	if err := os.MkdirAll(c.cfg.HomeDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(pinPath, []byte(routerPubB32), 0o600); err != nil {
		return err
	}
	c.log.Info("TOFU: pinned router key", "router_fingerprint", short(routerPubB32), "pin_file", pinPath)
	return nil
}

// setRootPub records the company root the current handshake chained to —
// the trust anchor router.directory entries are verified against.
func (c *Client) setRootPub(rootPubB32 string) {
	c.dirMu.Lock()
	c.rootPubB32 = rootPubB32
	c.dirMu.Unlock()
}

// Directory returns the verified discoverable-router set the serving
// router last advertised (the fleet attaches sessions from it).
func (c *Client) Directory() []wire.DirectoryEntry {
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	return append([]wire.DirectoryEntry(nil), c.directory...)
}

// noteDirectory records the discoverable router fleet the serving router
// advertised. Only entries whose cert chains to THIS node's pinned root
// are kept — URL and key alone are data, never trust — and a node that
// never pinned a root keeps nothing (its --router list is the whole world
// until it does).
func (c *Client) noteDirectory(d wire.DirectoryData) {
	c.dirMu.Lock()
	root := c.rootPubB32
	c.dirMu.Unlock()
	if root == "" {
		c.log.Info("router.directory ignored — no pinned company root yet",
			"entries", len(d.Routers))
		return
	}
	kept := make([]wire.DirectoryEntry, 0, len(d.Routers))
	for _, e := range d.Routers {
		if e.Cert == nil {
			continue // an uncertified entry has no chain to this root
		}
		pub, certRoot, err := identity.VerifyRouterCert(*e.Cert)
		if err != nil || identity.PubString(certRoot) != root {
			c.log.Warn("router.directory entry refused", "router_id", e.RouterID, "err", err)
			continue
		}
		if identity.ID(pub) != e.RouterID || identity.PubString(pub) != e.PublicKey || e.URL == "" {
			c.log.Warn("router.directory entry malformed", "router_id", e.RouterID)
			continue
		}
		kept = append(kept, e)
	}
	c.dirMu.Lock()
	c.directory = kept
	c.dirMu.Unlock()
	c.log.Info("router.directory", "kept", len(kept), "of", len(d.Routers))
	if c.cfg.OnDirectory != nil {
		c.cfg.OnDirectory(kept)
	}
}

// RouterID returns the serving router's identity id once the handshake
// proved it ("" before the first auth).
func (c *Client) RouterID() string {
	if pub, ok := c.routerPub.Load().(string); ok {
		if pk, err := identity.PubFromString(pub); err == nil {
			return identity.ID(pk)
		}
	}
	return ""
}

// RootPub returns the pinned company root this client's handshake chained
// to ("" when none did — the legacy key-pin path).
func (c *Client) RootPub() string {
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	return c.rootPubB32
}

// verifyCert validates a presented certificate (nil = none configured) and
// returns the root public key it chains to. Beyond signature and expiry it
// checks that the certified key IS the key that signed the challenge — a
// valid cert for a different router must not launder this handshake.
func (c *Client) verifyCert(ch *wire.ChallengeData, routerPub ed25519.PublicKey) (string, error) {
	if ch.Cert == nil {
		return "", nil
	}
	certPub, rootPub, err := identity.VerifyRouterCert(*ch.Cert)
	if err != nil {
		return "", fmt.Errorf("router certificate invalid: %w", err)
	}
	if !certPub.Equal(routerPub) {
		return "", errors.New("router certificate binds a different key than the challenge")
	}
	return identity.PubString(rootPub), nil
}

// checkRootPin enforces the company-root pin: --router-root wins outright;
// otherwise the persisted root pin must match; otherwise first contact
// TOFUs — the same first-contact trust decision the key pin always made,
// now at the level the whole certified fleet shares.
func (c *Client) checkRootPin(rootPubB32 string) error {
	pinPath := filepath.Join(c.cfg.HomeDir, routerRootPin)
	want := c.cfg.RouterRoot
	if want == "" {
		b, err := os.ReadFile(pinPath)
		if err == nil {
			want = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if want != "" {
		if want != rootPubB32 {
			return fmt.Errorf("%w: pinned_root=%s got_root=%s (%s)",
				errPinMismatch, short(want), short(rootPubB32), pinPath)
		}
		return nil
	}
	if err := os.MkdirAll(c.cfg.HomeDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(pinPath, []byte(rootPubB32), 0o600); err != nil {
		return err
	}
	c.log.Info("TOFU: pinned company root — certified routers are trusted from here",
		"root_fingerprint", short(rootPubB32), "pin_file", pinPath)
	return nil
}

func (c *Client) writeState(connected bool, routerID string) {
	nodeID := identity.ID(c.cfg.Identity.Public().(ed25519.PublicKey))
	state := map[string]any{
		"node_id":    nodeID,
		"router_url": c.cfg.RouterURL,
		"router_id":  routerID,
		"connected":  connected,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
		"capability": c.capabilityCache,
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(c.cfg.HomeDir, 0o700)
	_ = os.WriteFile(filepath.Join(c.cfg.HomeDir, stateFile), b, 0o600)
}

func (c *Client) send(conn *websocket.Conn, typ, nodeID string, data any) error {
	return c.sendReq0(conn, typ, nodeID, "", data)
}

// sendReq sends an inference envelope over the session socket,
// correlated by req_id.
func (c *Client) sendReq(conn *websocket.Conn, nodeID, reqID, typ string, data any) error {
	return c.sendReq0(conn, typ, nodeID, reqID, data)
}

func (c *Client) sendReqError(conn *websocket.Conn, nodeID, reqID, code, message string) {
	_ = c.sendReq(conn, nodeID, reqID, wire.TypeInferenceError, wire.ErrorData{Code: code, Message: message})
}

// sendReq0 writes one envelope. sendMu serializes writes so heartbeats and
// concurrent inference responses never interleave frames (gorilla permits
// exactly one concurrent writer).
func (c *Client) sendReq0(conn *websocket.Conn, typ, nodeID, reqID string, data any) error {
	env, err := wire.NewReq(typ, nodeID, reqID, data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	return conn.WriteMessage(websocket.TextMessage, b)
}

func (c *Client) recv(conn *websocket.Conn, want string) (wire.Envelope, error) {
	_ = conn.SetReadDeadline(time.Now().Add(handshakeWait))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return wire.Envelope{}, err
	}
	var env wire.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return wire.Envelope{}, err
	}
	if env.V != wire.Version {
		return wire.Envelope{}, fmt.Errorf("wire version %d, want %d", env.V, wire.Version)
	}
	if env.Type == wire.TypeNodeAuthFail {
		var d wire.AuthFailData
		_ = json.Unmarshal(env.Data, &d)
		if d.Code == "node_token" {
			return wire.Envelope{}, fmt.Errorf("%w: %s", ErrNodeClaim, d.Reason)
		}
		return wire.Envelope{}, fmt.Errorf("auth rejected: %s", d.Reason)
	}
	if env.Type != want {
		return wire.Envelope{}, fmt.Errorf("expected %s, got %s", want, env.Type)
	}
	return env, nil
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

// Drain performs the graceful-stop sequence (finish in-flight ≤
// deadline): stop advertising models (the scheduler stops picking this
// node on the next heartbeat), tell the router we are draining, then wait
// for live requests to finish. A second signal should cancel the ctx for
// a hard stop.
func (c *Client) Drain(ctx context.Context, deadline time.Duration) {
	c.SetServingModels(nil)
	// Best-effort notice; the heartbeat's empty serving list is the real
	// scheduler signal.
	_ = c.sendRaw(wire.TypeNodeDraining, c.currentNodeID(), nil)

	end := time.Now().Add(deadline)
	for {
		n := c.inFlight.Load()
		if n == 0 {
			c.log.Info("drain complete")
			return
		}
		if time.Now().After(end) {
			c.log.Warn("drain deadline hit with requests in flight", "in_flight", n)
			return
		}
		select {
		case <-ctx.Done():
			c.log.Warn("drain interrupted", "in_flight", n)
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// currentNodeID derives the identity id (cached key math is cheap).
func (c *Client) currentNodeID() string {
	return identity.ID(c.cfg.Identity.Public().(ed25519.PublicKey))
}

// sendRaw writes a bare envelope without req correlation. sendReq0 does
// the write serialization — no lock held here (double-locking sendMu would
// deadlock every writer).
func (c *Client) sendRaw(typ, nodeID string, data any) error {
	c.sendMu.Lock()
	conn := c.curConn
	c.sendMu.Unlock()
	if conn == nil {
		return errors.New("no session")
	}
	return c.sendReq0(conn, typ, nodeID, "", data)
}
