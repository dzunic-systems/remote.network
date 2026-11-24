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
}

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
	ctx, l.cancel = context.WithCancel(ctx)
	go l.readDatagrams(ctx)
	go l.accept(ctx)
	go l.keepalive(ctx)
	l.log.Info("client listener up", "addr", sock.LocalAddr().String())
	return l, nil
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

// serveConn answers a direct connection's streams. For now: direct.ping
// gets a pong; anything else is told the node does not serve direct
// requests yet.
func (l *ClientListener) serveConn(conn *quic.Conn) {
	peer, _ := identity.PeerIdentity(conn.ConnectionState().TLS)
	l.log.Debug("direct connection", "remote", conn.RemoteAddr().String(), "client", peer)
	ctx := conn.Context()
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go l.serveStream(stream)
	}
}

func (l *ClientListener) serveStream(stream *quic.Stream) {
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
	var reply wire.Envelope
	var err error
	switch env.Type {
	case wire.TypeDirectPing:
		reply, err = wire.New(wire.TypeDirectPong, l.nodeID, wire.DirectPongData{
			NodeID: l.nodeID, AgentVersion: l.cfg.AgentVersion, Serving: false,
		})
	default:
		reply, err = wire.NewReq(wire.TypeInferenceError, l.nodeID, env.ReqID, wire.ErrorData{
			Code: "direct_not_serving", Message: "this node accepts direct connections but does not serve direct requests yet",
		})
	}
	if err == nil {
		_ = clientproto.WriteEnvelope(stream, reply, writeWait)
	}
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
	l.info.Store(&wire.DirectClientInfo{Endpoints: endpoints, NAT: obs.NAT, MaxConns: clientMaxConns})
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
