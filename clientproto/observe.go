package clientproto

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"time"

	"remote.network/node/wire"
)

// Address observation: a STUN-like echo on
// the routers' punch sockets. A client — or a node's client listener —
// sends an observe request to a router's main and alt UDP sockets; each
// answers with the source address it saw. Two answers tell the sender its
// public address and its NAT class (one mapping for both destinations =
// cone; two = symmetric), without a WSS session, an account, or HTTP.
//
// Anti-amplification: a request is padded to ObserveRequestLen and a
// router answers only requests at least that long, so a reply is never
// larger than the request that caused it (a spoofed source gains nothing).

// Datagram magics. Like wire's probe magic, the first byte must not carry
// QUIC's fixed bit (0x40), so a quic-go Transport hands these datagrams to
// ReadNonQUICPacket instead of swallowing them.
const (
	observeReqMagic   = 0x02
	observeReplyMagic = 0x03
	observeIDLen      = 16
	// ObserveRequestLen is every observe request's size on the wire.
	ObserveRequestLen = 96
	maxAddrLen        = ObserveRequestLen - 1 - observeIDLen
)

// EncodeObserveRequest frames a request with a fresh random id.
func EncodeObserveRequest() (id [observeIDLen]byte, pkt []byte) {
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	pkt = make([]byte, ObserveRequestLen)
	pkt[0] = observeReqMagic
	copy(pkt[1:], id[:])
	return id, pkt
}

// DecodeObserveRequest reports whether b is an observe request (and long
// enough to deserve an answer), returning its id.
func DecodeObserveRequest(b []byte) (id [observeIDLen]byte, ok bool) {
	if len(b) < ObserveRequestLen || b[0] != observeReqMagic {
		return id, false
	}
	copy(id[:], b[1:1+observeIDLen])
	return id, true
}

// EncodeObserveReply answers request id with the observed source address.
// ok is false when the address cannot fit (never for real UDP addresses).
func EncodeObserveReply(id [observeIDLen]byte, observed string) ([]byte, bool) {
	if len(observed) > maxAddrLen {
		return nil, false
	}
	pkt := make([]byte, 0, 1+observeIDLen+len(observed))
	pkt = append(pkt, observeReplyMagic)
	pkt = append(pkt, id[:]...)
	pkt = append(pkt, observed...)
	return pkt, true
}

// DecodeObserveReply parses a reply.
func DecodeObserveReply(b []byte) (id [observeIDLen]byte, observed string, ok bool) {
	if len(b) < 1+observeIDLen+3 || b[0] != observeReplyMagic {
		return id, "", false
	}
	copy(id[:], b[1:1+observeIDLen])
	observed = string(b[1+observeIDLen:])
	if _, err := net.ResolveUDPAddr("udp", observed); err != nil {
		return id, "", false
	}
	return id, observed, true
}

// ClassifyNAT names a NAT from the local socket address and the addresses
// two different router sockets observed for it: open (no translation),
// cone (one mapping for every destination — punchable), symmetric (a
// mapping per destination — relay only). candidate says whether a direct
// path can be attempted.
func ClassifyNAT(local, main, alt string) (nat string, candidate bool) {
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

// Observation is what one observe round learned.
type Observation struct {
	Main, Alt string // the public address each router socket saw
	NAT       string // ClassifyNAT's verdict
	Candidate bool
}

// Observer runs observe rounds over a socket it does not own: the caller
// keeps the socket's single read loop and hands every non-QUIC datagram
// to Deliver (a quic-go Transport allows exactly one ReadNonQUICPacket
// reader, and the node's listener socket is such a transport).
type Observer struct {
	write func(b []byte, to net.Addr) error

	mu      sync.Mutex
	waiting map[[observeIDLen]byte]chan string
}

// NewObserver wraps the socket's send function.
func NewObserver(write func(b []byte, to net.Addr) error) *Observer {
	return &Observer{write: write, waiting: map[[observeIDLen]byte]chan string{}}
}

// Deliver offers one received datagram; true means it was an observe
// reply (consumed), false that the caller should handle it.
func (o *Observer) Deliver(b []byte) bool {
	id, addr, ok := DecodeObserveReply(b)
	if !ok {
		return false
	}
	o.mu.Lock()
	ch := o.waiting[id]
	o.mu.Unlock()
	if ch != nil {
		select {
		case ch <- addr:
		default:
		}
	}
	return true
}

// ErrNoObservation means a router socket never answered within the round.
var ErrNoObservation = errors.New("router did not answer the observe request")

// Observe asks the router's main and alt sockets what they see, resending
// every 200 ms until both answer or ctx ends. local is the socket's own
// address as the NAT classification's "no translation" reference.
func (o *Observer) Observe(ctx context.Context, local string, main, alt *net.UDPAddr) (Observation, error) {
	ask := func(to *net.UDPAddr) ([observeIDLen]byte, []byte, chan string) {
		id, pkt := EncodeObserveRequest()
		ch := make(chan string, 1)
		o.mu.Lock()
		o.waiting[id] = ch
		o.mu.Unlock()
		return id, pkt, ch
	}
	mainID, mainPkt, mainCh := ask(main)
	altID, altPkt, altCh := ask(alt)
	defer func() {
		o.mu.Lock()
		delete(o.waiting, mainID)
		delete(o.waiting, altID)
		o.mu.Unlock()
	}()

	var obs Observation
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if obs.Main == "" {
			_ = o.write(mainPkt, main)
		}
		if obs.Alt == "" {
			_ = o.write(altPkt, alt)
		}
		select {
		case a := <-mainCh:
			obs.Main = a
		case a := <-altCh:
			obs.Alt = a
		case <-tick.C:
		case <-ctx.Done():
			if obs.Main != "" {
				// The main answer alone is still the public address; the
				// class stays unknown.
				obs.NAT = "unknown"
				return obs, nil
			}
			return obs, ErrNoObservation
		}
		if obs.Main != "" && obs.Alt != "" {
			obs.NAT, obs.Candidate = ClassifyNAT(local, obs.Main, obs.Alt)
			// A socket bound to every interface (0.0.0.0:port) never equals
			// what a router observed, so the classifier calls an untranslated
			// path a cone. No translation is: the observed address is one of
			// this machine's own, at the socket's port.
			if obs.NAT == wire.NATCone && unspecifiedHost(local) && isOwnAddr(obs.Main, portOf(local)) {
				obs.NAT, obs.Candidate = wire.NATOpen, true
			}
			return obs, nil
		}
	}
}

// ObserveAnswer is the router's side: given a received datagram and its
// source, the reply to send — or ok=false when the datagram is not a
// (long enough) observe request.
func ObserveAnswer(b []byte, from string) (reply []byte, ok bool) {
	id, ok := DecodeObserveRequest(b)
	if !ok {
		return nil, false
	}
	return EncodeObserveReply(id, from)
}

func unspecifiedHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func portOf(hostport string) string {
	_, p, _ := net.SplitHostPort(hostport)
	return p
}

// isOwnAddr reports whether hostport is one of this machine's interface
// addresses (loopback included) at port.
func isOwnAddr(hostport, port string) bool {
	host, p, err := net.SplitHostPort(hostport)
	if err != nil || p != port {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
			return true
		}
	}
	return false
}
