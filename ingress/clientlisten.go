// clientlisten.go: the node's client listener — one long-lived UDP socket and a QUIC listener on it with ALPN
// "remote.client", where direct clients dial the node instead of going
// through a router. Off unless `node start --client-direct`.
//
// In node v0.1.11 the listener makes the node REACHABLE: it learns and reports
// its public address and NAT class, keeps the mapping alive, opens its
// NAT toward a client on a router's direct.offer, and answers direct.ping.
// It serves no inference yet — direct.request (tickets) comes later — so
// opening the port exposes nothing but a handshake and a pong.
package ingress

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/wire"
)

// Client-listener limits and timing.
const (
	clientMaxConns       = 64 // total direct connections
	clientMaxConnsPerIP  = 8
	clientKeepalive      = 25 * time.Second // observe round: refreshes the NAT mapping and the public address
	clientObserveTimeout = 4 * time.Second
	clientInfoTTL        = time.Hour // re-read a router's /direct/info
	offerBurstEvery      = 150 * time.Millisecond
	offerBurstFor        = 3 * time.Second
	offerMaxAddrs        = 4
	offerMaxConcurrent   = 16
)

// ClientListenerConfig configures the client listener.
type ClientListenerConfig struct {
	Identity   ed25519.PrivateKey
	Port       int      // UDP port; 0 = any free port
	RouterURLs []string // the node's --router seeds (ws(s)://…); /direct/info is read from them
	// KnownRouterURLs lists every router the node is attached to (the
	// fleet's directory): the NAT test needs a second router on a
	// different IP. Nil = the seeds only.
	KnownRouterURLs func() []string
	AgentVersion    string
	Log             *slog.Logger
	// HTTPClient reads /direct/info (nil = a 10 s timeout client).
	HTTPClient *http.Client
	// LANAddrs overrides the machine's LAN address discovery (tests).
	LANAddrs func() []net.IP
	// Handler, when set, is the inference core the listener serves
	// direct requests with (the same handler the relayed planes use).
	// nil = the listener only completes handshakes and answers pings —
	// reachability without serving (the heartbeat then reports Serving
	// false, and the router routes direct traffic elsewhere).
	Handler InferenceHandler
	// RouterRoot is the company root public key the listener verifies
	// tickets against (--router-root, base32); empty falls back to the
	// HomeDir's persisted root pin. Neither present = the listener
	// cannot verify a ticket chain, so it serves nothing direct.
	RouterRoot string
	// HomeDir is ~/.remote: the root pin file lives there, and the
	// receipt queue directory is created under it.
	HomeDir string
	// NodeInFlight is the NODE-wide dispatch counter the listener's
	// direct requests count into (shared with every fleet client, so
	// every router's load view includes them). Nil = legacy: only this
	// listener counts.
	NodeInFlight *atomic.Int64
	// Fleet returns the live fleet (for the receipt sink: the ticket's
	// router's session, else any connected one). Nil = receipts queue
	// until one is set.
	Fleet func() *Fleet
}

// serving reports whether this listener answers direct.request streams:
// the heartbeat's Serving fact, the router's only candidate filter.
func (l *ClientListener) serving() bool { return l.cfg.Handler != nil }

// ClientListener is the running listener.
type ClientListener struct {
	cfg    ClientListenerConfig
	log    *slog.Logger
	nodeID string
	sock   *net.UDPConn
	tr     *quic.Transport
	ln     *quic.Listener
	obs    *clientproto.Observer
	cancel context.CancelFunc

	info   atomic.Pointer[wire.DirectClientInfo]
	offers atomic.Int64
	core   *serveCore
	queue  *receiptStore

	directInFlight atomic.Int64

	revMu   sync.Mutex
	revoked map[string]time.Time // ticket id → drop after
	liveMu  sync.Mutex
	live    map[string][]context.CancelFunc // ticket id → running streams
	usedMu  sync.Mutex
	used    map[string]time.Time // used ticket id → keep until

	mu         sync.Mutex
	conns      int
	perIP      map[string]int
	routerInfo map[string]routerUDP // router URL → its punch sockets
}

// StartClientListener binds the socket, starts the QUIC listener, the
// datagram read loop and the keepalive/observe loop.
func StartClientListener(ctx context.Context, cfg ClientListenerConfig) (*ClientListener, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.LANAddrs == nil {
		cfg.LANAddrs = lanAddrs
	}
	tlsCfg, err := clientproto.ClientListenerTLS(cfg.Identity)
	if err != nil {
		return nil, err
	}
	sock, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.Port})
	if err != nil {
		return nil, fmt.Errorf("client listener: bind udp :%d: %w", cfg.Port, err)
	}
	l := &ClientListener{
		cfg:    cfg,
		log:    cfg.Log.With("component", "client-listener"),
		nodeID: identity.ID(cfg.Identity.Public().(ed25519.PublicKey)),
		sock:   sock,
		perIP:  map[string]int{},

		routerInfo: map[string]routerUDP{},
		live:       map[string][]context.CancelFunc{},
	}
	l.tr = &quic.Transport{
		Conn: sock,
		// Address validation (a Retry round trip) for every handshake: a
		// spoofed source cannot make the node send a handshake's worth of
		// bytes at a victim.
		VerifySourceAddress: func(net.Addr) bool { return true },
		ConnContext:         l.admit,
	}
	l.ln, err = l.tr.Listen(tlsCfg, clientproto.DirectQUICConfig())
	if err != nil {
		sock.Close()
		return nil, fmt.Errorf("client listener: quic listen: %w", err)
	}
	l.obs = clientproto.NewObserver(func(b []byte, to net.Addr) error {
		_, err := l.tr.WriteTo(b, to)
		return err
	})
	if cfg.Handler != nil {
		l.core = &serveCore{
			handler:      cfg.Handler,
			identity:     cfg.Identity,
			log:          l.log,
			connInFlight: &l.directInFlight,
			nodeInFlight: cfg.NodeInFlight,
		}
		l.queue = newReceiptStore(filepath.Join(cfg.HomeDir, "receipts"), l.log, l.sendReceipt)
		go l.queue.run(ctx)
		if err := os.MkdirAll(l.queue.dir, 0o700); err == nil {
			l.queue.sweep(ctx) // boot: deliver what the last run queued
		}
	}
	ctx, l.cancel = context.WithCancel(ctx)
	go l.readDatagrams(ctx)
	go l.accept(ctx)
	go l.keepalive(ctx)
	l.log.Info("client listener up", "addr", sock.LocalAddr().String(),
		"serving", l.serving())
	return l, nil
}

// directInFlight is the listener's own dispatch count (Drain's deadline
// waits on the fleet clients' counts; direct requests count node-wide
// through cfg.NodeInFlight).
// sendReceipt delivers one receipt to a router: the ticket's router's
// fleet session first, else any connected one (they share the spine; the
// receipts PK dedupes).
func (l *ClientListener) sendReceipt(routerID string, env wire.Envelope) bool {
	f := l.cfg.Fleet
	if f == nil {
		return false
	}
	fleet := f()
	if fleet == nil {
		return false
	}
	if routerID != "" {
		if c := fleet.ClientForRouter(routerID); c != nil && c.sendRaw(wire.TypeUsageReceipt, env.NodeID, env.Data) == nil {
			return true
		}
	}
	for _, c := range fleet.Clients() {
		if c.sendRaw(wire.TypeUsageReceipt, env.NodeID, env.Data) == nil {
			return true
		}
	}
	return false
}

// AckReceipt records a router's usage.receipt_ack: the queued copy may go.
func (l *ClientListener) AckReceipt(ticketID, requestID string) {
	if l.queue != nil {
		l.queue.noteAcked(ticketID, requestID)
	}
}

// Revoke kills tickets: refuse new streams carrying them and abort any
// running one. Entries age out past the ticket lifetime + skew — the
// lifetime bounds the whole set.
func (l *ClientListener) Revoke(ticketIDs []string) {
	drop := time.Now().Add(clientproto.TicketLifetime + 2*clientproto.TicketSkew)
	l.revMu.Lock()
	if l.revoked == nil {
		l.revoked = map[string]time.Time{}
	}
	for _, id := range ticketIDs {
		l.revoked[id] = drop
	}
	l.revMu.Unlock()
	for _, id := range ticketIDs {
		l.liveMu.Lock()
		cancels := l.live[id]
		delete(l.live, id)
		l.liveMu.Unlock()
		for _, cancel := range cancels {
			cancel() // the running stream dies; the client falls back
		}
	}
	l.log.Info("direct tickets revoked", "tickets", len(ticketIDs))
}

// revokedNow reports whether a ticket id is on the revocation set.
func (l *ClientListener) revokedNow(ticketID string) bool {
	l.revMu.Lock()
	defer l.revMu.Unlock()
	drop, ok := l.revoked[ticketID]
	if ok && time.Now().After(drop) {
		delete(l.revoked, ticketID)
		return false
	}
	return ok
}

// noteTicketUsed records a spent ticket (one request per ticket in v1);
// a second use before its expiry+skew is refused.
func (l *ClientListener) noteTicketUsed(t clientproto.Ticket) bool {
	until := time.Unix(t.NotAfter, 0).Add(clientproto.TicketSkew)
	l.usedMu.Lock()
	defer l.usedMu.Unlock()
	if l.used == nil {
		l.used = map[string]time.Time{}
	}
	now := time.Now()
	if deadline, seen := l.used[t.ID]; seen && now.Before(deadline) {
		return false
	}
	// prune occasionally
	if len(l.used) > 1024 {
		for id, deadline := range l.used {
			if now.After(deadline) {
				delete(l.used, id)
			}
		}
	}
	l.used[t.ID] = until
	return true
}

// Close stops the listener.
func (l *ClientListener) Close() {
	l.cancel()
	_ = l.ln.Close()
	_ = l.tr.Close()
}

// Addr is the bound socket address.
func (l *ClientListener) Addr() *net.UDPAddr { return l.sock.LocalAddr().(*net.UDPAddr) }

// Info is the reachability report heartbeats carry (nil until the first
// observe round has finished).
func (l *ClientListener) Info() *wire.DirectClientInfo {
	if i := l.info.Load(); i != nil {
		cp := *i
		return &cp
	}
	return nil
}

// admit enforces the connection caps before a handshake proceeds.
func (l *ClientListener) admit(ctx context.Context, ci *quic.ClientInfo) (context.Context, error) {
	ip := ipOf(ci.RemoteAddr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns >= clientMaxConns || l.perIP[ip] >= clientMaxConnsPerIP {
		return ctx, errors.New("client listener at capacity")
	}
	l.conns++
	l.perIP[ip]++
	// The slot is released when the connection ends, however it ends:
	// quic-go cancels this context on close, including a handshake that
	// fails or is abandoned — such a connection never reaches Accept, so
	// releasing in serveConn alone leaked its slot until the per-IP cap
	// locked that address out.
	addr := ci.RemoteAddr
	go func() {
		<-ctx.Done()
		l.release(addr)
	}()
	return ctx, nil
}

func (l *ClientListener) release(addr net.Addr) {
	ip := ipOf(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conns--
	if l.perIP[ip]--; l.perIP[ip] <= 0 {
		delete(l.perIP, ip)
	}
}

// readDatagrams is the socket's single non-QUIC reader: observe replies go
// to the observer; everything else (a client's punch packets) is dropped —
// its only job was to open the client's own NAT.
func (l *ClientListener) readDatagrams(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		n, _, err := l.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		l.obs.Deliver(buf[:n])
	}
}

func (l *ClientListener) accept(ctx context.Context) {
	for {
		conn, err := l.ln.Accept(ctx)
		if err != nil {
			return
		}
		go l.serveConn(conn)
	}
}

// serveConn answers a direct connection's streams: direct.ping gets a
// pong, direct.request runs the ticket checks and the shared serving
// core, anything else is refused. The TLS peer key is bound to the
// connection — every stream on it carries the same identity.
func (l *ClientListener) serveConn(conn *quic.Conn) {
	peer, _ := identity.PeerIdentity(conn.ConnectionState().TLS)
	l.log.Debug("direct connection", "remote", conn.RemoteAddr().String(), "client", peer)
	ctx := conn.Context()
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go l.serveStream(stream, peer)
	}
}

// serveStream handles one stream. The 10 s read deadline covers the
// request envelope; the serving path clears it — a stream's life is the
// request's (the client's own read deadline governs the answer).
func (l *ClientListener) serveStream(stream *quic.Stream, peer string) {
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	sc := clientproto.NewEnvelopeScanner(stream)
	if !sc.Scan() {
		return
	}
	var env wire.Envelope
	if json.Unmarshal(sc.Bytes(), &env) != nil {
		return
	}
	switch env.Type {
	case wire.TypeDirectPing:
		reply, err := wire.New(wire.TypeDirectPong, l.nodeID, wire.DirectPongData{
			NodeID: l.nodeID, AgentVersion: l.cfg.AgentVersion, Serving: l.serving(),
		})
		if err == nil {
			_ = clientproto.WriteEnvelope(stream, reply, writeWait)
		}
		return
	case wire.TypeDirectRequest:
		l.serveDirectRequest(stream, peer, env)
	default:
		reply, err := wire.NewReq(wire.TypeInferenceError, l.nodeID, env.ReqID, wire.ErrorData{
			Code: "direct_not_serving", Message: "this node accepts direct connections but does not serve this",
		})
		if err == nil {
			_ = clientproto.WriteEnvelope(stream, reply, writeWait)
		}
	}
}

// serveDirectRequest runs one ticketed request: the checks, in order,
// each refusing with a stable code before any backend work; then the
// shared serving core streams the answer, the receipt carries the ticket
// id, and the queue walks it to a router.
func (l *ClientListener) serveDirectRequest(stream *quic.Stream, peer string, env wire.Envelope) {
	refuse := func(reqID, code, message string) {
		reply, err := wire.NewReq(wire.TypeInferenceError, l.nodeID, reqID,
			wire.ErrorData{Code: code, Message: message})
		if err == nil {
			_ = clientproto.WriteEnvelope(stream, reply, writeWait)
		}
	}
	if l.core == nil {
		refuse(env.ReqID, "direct_not_serving", "this node accepts direct connections but does not serve direct requests")
		return
	}
	var d clientproto.DirectRequestData
	if err := json.Unmarshal(env.Data, &d); err != nil || d.RequestID == "" {
		refuse(env.ReqID, "ticket_invalid", "malformed direct request")
		return
	}
	t := d.Ticket
	// The chain: pinned root → router certificate → ticket, for THIS node,
	// within the validity window.
	root, ok := l.rootPub()
	if !ok {
		refuse(d.RequestID, "ticket_invalid", "this node has no company root pinned — it cannot verify tickets")
		return
	}
	if err := clientproto.VerifyTicket(t, root, l.nodeID, time.Now()); err != nil {
		code := "ticket_invalid"
		if errors.Is(err, clientproto.ErrTicketExpired) {
			code = "ticket_expired"
		}
		l.log.Info("direct request refused", "request_id", d.RequestID, "code", code, "err", err)
		refuse(d.RequestID, code, "ticket refused: "+err.Error())
		return
	}
	// The TLS peer IS the ticket's client: the key the router bound to
	// this permission must be the key on this connection.
	if t.ClientPub != peer {
		refuse(d.RequestID, "ticket_client", "ticket was issued to another client key")
		return
	}
	// The request asks for the ticket's model, nothing else.
	if d.Request.Model != t.ModelID {
		refuse(d.RequestID, "ticket_model",
			fmt.Sprintf("ticket is for model %q, request names %q", t.ModelID, d.Request.Model))
		return
	}
	// One request per ticket.
	if !l.noteTicketUsed(t) {
		refuse(d.RequestID, "ticket_used", "ticket was already used")
		return
	}
	// Not revoked (the router's push; the lifetime bounds a lost push).
	if l.revokedNow(t.ID) {
		refuse(d.RequestID, "ticket_revoked", "ticket was revoked")
		return
	}
	// The ticket's output cap is what the reservation covered: the
	// request's ask (or a default) never exceeds it.
	if t.MaxTokens > 0 && (d.Request.MaxTokens <= 0 || d.Request.MaxTokens > t.MaxTokens) {
		d.Request.MaxTokens = t.MaxTokens
	}
	// The stream's life is the request's; a revoke cancels it mid-flight.
	sctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.liveMu.Lock()
	l.live[t.ID] = append(l.live[t.ID], cancel)
	l.liveMu.Unlock()
	defer func() {
		l.liveMu.Lock()
		delete(l.live, t.ID)
		l.liveMu.Unlock()
	}()

	_ = stream.SetReadDeadline(time.Time{}) // the answer now governs
	start := time.Now()
	done, code, err := l.core.run(sctx, d.RequestID, d.Request, func(chunk wire.InferenceChunkData) error {
		reply, werr := wire.NewReq(wire.TypeInferenceChunk, l.nodeID, d.RequestID, chunk)
		if werr != nil {
			return werr
		}
		return clientproto.WriteEnvelope(stream, reply, writeWait)
	}, t.ID)
	// The receipt is queued (on disk) BEFORE the consumer hears the end:
	// the answer is out, and the record that bills it must not depend on
	// this process surviving the next write. A request cut off after
	// output carries a failed_mid_stream receipt (serveCore.run).
	if done.Receipt != nil {
		l.queue.enqueue(*done.Receipt, t.RouterID)
	}
	if code != "" {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		refuse(d.RequestID, code, msg)
		return
	}
	if err == nil {
		reply, werr := wire.NewReq(wire.TypeInferenceDone, l.nodeID, d.RequestID, done)
		if werr == nil {
			werr = clientproto.WriteEnvelope(stream, reply, writeWait)
		}
		if werr != nil {
			l.log.Warn("direct answer write failed", "request_id", d.RequestID, "err", werr)
		}
	}
	l.log.Info("direct request served", "request_id", d.RequestID, "model", d.Request.Model,
		"ticket", t.ID, "ms", time.Since(start).Milliseconds())
}

// rootPub is the company root this listener verifies ticket chains
// against: the explicit flag, else the persisted pin (the same pin the
// WSS handshake TOFU'd).
func (l *ClientListener) rootPub() (ed25519.PublicKey, bool) {
	want := l.cfg.RouterRoot
	if want == "" && l.cfg.HomeDir != "" {
		if b, err := os.ReadFile(filepath.Join(l.cfg.HomeDir, routerRootPin)); err == nil {
			want = strings.TrimSpace(string(b))
		}
	}
	if want == "" {
		return nil, false
	}
	pub, err := identity.PubFromString(want)
	if err != nil {
		return nil, false
	}
	return pub, true
}

// keepalive runs an observe round now and every clientKeepalive: it keeps
// the NAT mapping of the listener socket fresh and the reported public
// address current.
func (l *ClientListener) keepalive(ctx context.Context) {
	t := time.NewTicker(clientKeepalive)
	defer t.Stop()
	for {
		l.observeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (l *ClientListener) observeOnce(ctx context.Context) {
	routers, err := l.routerUDPs(ctx)
	if err != nil {
		l.log.Warn("client listener: no router observe endpoint", "err", err)
		return
	}
	local := l.sock.LocalAddr().String()
	octx, cancel := context.WithTimeout(ctx, clientObserveTimeout)
	defer cancel()
	obs, err := l.obs.Observe(octx, local, routers[0].main, routers[0].alt)
	if err != nil {
		l.log.Warn("client listener: observe failed", "err", err)
		return
	}
	// The first round compares two ports of ONE router: it catches a NAT
	// that maps per destination port, not one that maps per destination
	// IP (address-dependent mapping), which defeats hole punching just the
	// same. Asking a second router, on another IP, catches that too.
	mapping := "one router"
	if obs.Candidate && len(routers) > 1 {
		octx2, cancel2 := context.WithTimeout(ctx, clientObserveTimeout)
		cross, err := l.obs.Observe(octx2, local, routers[0].main, routers[1].main)
		cancel2()
		obs, mapping = combineNAT(obs, cross, err)
	}
	port := l.Addr().Port
	endpoints := []string{obs.Main}
	for _, ip := range l.cfg.LANAddrs() {
		ep := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		if ep != obs.Main {
			endpoints = append(endpoints, ep)
		}
	}
	prev := l.info.Load()
	l.info.Store(&wire.DirectClientInfo{Endpoints: endpoints, NAT: obs.NAT, MaxConns: clientMaxConns,
		Serving: l.serving()})
	if prev == nil || prev.Endpoints[0] != obs.Main || prev.NAT != obs.NAT {
		l.log.Info("client listener reachable", "public", obs.Main, "nat", obs.NAT,
			"candidate", obs.Candidate, "tested_against", mapping)
	}
}

// combineNAT folds the cross-router round into the first round's verdict:
// a mapping that differs between two router IPs is address-dependent —
// symmetric for hole punching. mapping says what the verdict rests on.
func combineNAT(first, cross clientproto.Observation, crossErr error) (clientproto.Observation, string) {
	switch {
	case crossErr != nil:
		return first, "one router (second router did not answer)"
	case cross.NAT == wire.NATSymmetric:
		first.NAT, first.Candidate = wire.NATSymmetric, false
		return first, "two routers: address-dependent mapping"
	default:
		return first, "two routers"
	}
}

// routerUDP is one router's punch sockets.
type routerUDP struct {
	main, alt *net.UDPAddr
	at        time.Time
}

// routerUDPs returns up to two routers' punch sockets on DIFFERENT IPs —
// seeds first, then the routers the fleet attached — each read from
// GET /api/v1/direct/info and cached for clientInfoTTL. One router is
// enough to run; two make the NAT test stronger.
func (l *ClientListener) routerUDPs(ctx context.Context) ([]routerUDP, error) {
	urls := append([]string{}, l.cfg.RouterURLs...)
	if l.cfg.KnownRouterURLs != nil {
		urls = append(urls, l.cfg.KnownRouterURLs()...)
	}
	var out []routerUDP
	seenIP := map[string]bool{}
	seenURL := map[string]bool{}
	var lastErr error = errors.New("no router configured")
	for _, u := range urls {
		if len(out) == 2 {
			break
		}
		if seenURL[u] {
			continue
		}
		seenURL[u] = true
		l.mu.Lock()
		r, ok := l.routerInfo[u]
		l.mu.Unlock()
		if !ok || time.Since(r.at) > clientInfoTTL {
			main, alt, err := fetchDirectInfo(ctx, l.cfg.HTTPClient, u)
			if err != nil {
				lastErr = err
				continue
			}
			r = routerUDP{main: main, alt: alt, at: time.Now()}
			l.mu.Lock()
			l.routerInfo[u] = r
			l.mu.Unlock()
		}
		ip := r.main.IP
		if seenIP[ip.String()] {
			continue
		}
		// The second router must speak the same IP family: an IPv4 and an
		// IPv6 observation always differ, which would make every network
		// look symmetric.
		if len(out) == 1 && (out[0].main.IP.To4() == nil) != (ip.To4() == nil) {
			continue
		}
		seenIP[ip.String()] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, lastErr
	}
	return out, nil
}

// fetchDirectInfo reads a router's /api/v1/direct/info. routerURL may be
// the node's WebSocket URL (ws(s)://host/node/v1/connect) or an HTTP base.
func fetchDirectInfo(ctx context.Context, hc *http.Client, routerURL string) (*net.UDPAddr, *net.UDPAddr, error) {
	base := RouterHTTPBase(routerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/direct/info", nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s/api/v1/direct/info: %s", base, resp.Status)
	}
	var info struct {
		UDPAddr    string `json:"udp_addr"`
		AltUDPAddr string `json:"alt_udp_addr"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, nil, err
	}
	main, err := net.ResolveUDPAddr("udp", info.UDPAddr)
	if err != nil {
		return nil, nil, err
	}
	alt, err := net.ResolveUDPAddr("udp", info.AltUDPAddr)
	if err != nil {
		return nil, nil, err
	}
	return main, alt, nil
}

// RouterHTTPBase turns a router URL (ws://, wss://, http://, https://,
// with or without the /node/v1/connect path) into its HTTP base.
func RouterHTTPBase(u string) string {
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, "/node/v1/connect")
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	}
	return u
}

// Offer opens the NAT toward a client about to dial (a router's
// direct.offer): a short burst of small datagrams from the listener socket
// to each client address. Bounded — a few addresses, a few seconds, a cap
// on concurrent offers — so a router cannot turn the node into a sprayer.
func (l *ClientListener) Offer(d wire.DirectOfferData) {
	if d.NotAfter > 0 && time.Now().Unix() > d.NotAfter {
		return
	}
	if l.offers.Add(1) > offerMaxConcurrent {
		l.offers.Add(-1)
		l.log.Warn("direct offer dropped: too many in flight")
		return
	}
	var addrs []*net.UDPAddr
	for _, a := range d.ClientAddrs {
		if len(addrs) == offerMaxAddrs {
			break
		}
		if ua, err := net.ResolveUDPAddr("udp", a); err == nil {
			addrs = append(addrs, ua)
		}
	}
	go func() {
		defer l.offers.Add(-1)
		pkt := []byte{punchOpenMagic}
		end := time.Now().Add(offerBurstFor)
		for time.Now().Before(end) {
			for _, a := range addrs {
				_, _ = l.tr.WriteTo(pkt, a)
			}
			time.Sleep(offerBurstEvery)
		}
	}()
}

// punchOpenMagic is the one-byte datagram a burst sends: never QUIC (no
// fixed bit), never an observe or probe frame; receivers drop it.
const punchOpenMagic = 0x04

func ipOf(a net.Addr) string {
	if ua, ok := a.(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// lanAddrs lists the machine's private unicast IPv4 addresses (the
// endpoints a client on the same network can reach directly).
func lanAddrs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil && ip4.IsPrivate() {
				out = append(out, ip4)
			}
		}
	}
	return out
}
