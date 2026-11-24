package clientproto

import (
	"context"
	"net"
	"testing"
	"time"
)

// fakeRouterSocket answers observe requests like a router punch socket.
func fakeRouterSocket(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if reply, ok := ObserveAnswer(buf[:n], from.String()); ok {
				if len(reply) > n {
					panic("reply larger than request")
				}
				_, _ = conn.WriteToUDP(reply, from)
			}
		}
	}()
	return conn
}

func TestObserveOverLoopback(t *testing.T) {
	main, alt := fakeRouterSocket(t), fakeRouterSocket(t)
	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	obs := NewObserver(func(b []byte, to net.Addr) error { _, err := sock.WriteTo(b, to); return err })
	go func() { // the caller's single read loop
		buf := make([]byte, 1500)
		for {
			n, _, err := sock.ReadFromUDP(buf)
			if err != nil {
				return
			}
			obs.Deliver(buf[:n])
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := obs.Observe(ctx, sock.LocalAddr().String(),
		main.LocalAddr().(*net.UDPAddr), alt.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	if got.Main != sock.LocalAddr().String() || got.NAT != "open" || !got.Candidate {
		t.Errorf("observation = %+v", got)
	}
}

func TestObserveTimesOutWithoutRouter(t *testing.T) {
	sock, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer sock.Close()
	dead, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // never answers
	defer dead.Close()
	obs := NewObserver(func(b []byte, to net.Addr) error { _, err := sock.WriteTo(b, to); return err })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a := dead.LocalAddr().(*net.UDPAddr)
	if _, err := obs.Observe(ctx, sock.LocalAddr().String(), a, a); err != ErrNoObservation {
		t.Errorf("err = %v, want ErrNoObservation", err)
	}
}

func TestObserveNoAmplification(t *testing.T) {
	_, pkt := EncodeObserveRequest()
	if _, ok := ObserveAnswer(pkt[:ObserveRequestLen-1], "1.2.3.4:5"); ok {
		t.Error("a short request was answered")
	}
	reply, ok := ObserveAnswer(pkt, "[2001:db8:ffff:ffff:ffff:ffff:ffff:ffff]:65535")
	if !ok || len(reply) > len(pkt) {
		t.Errorf("longest IPv6 reply: ok=%v len=%d > request %d", ok, len(reply), len(pkt))
	}
	if pkt[0]&0x40 != 0 || reply[0]&0x40 != 0 {
		t.Error("a magic byte carries QUIC's fixed bit")
	}
	if _, _, ok := DecodeObserveReply(append([]byte{observeReplyMagic}, make([]byte, observeIDLen)...)); ok {
		t.Error("a reply without an address decoded")
	}
}

func TestClassifyNAT(t *testing.T) {
	for _, c := range []struct {
		local, main, alt, nat string
		cand                  bool
	}{
		{"10.0.0.2:5", "10.0.0.2:5", "10.0.0.2:5", "open", true},
		{"10.0.0.2:5", "1.2.3.4:9", "1.2.3.4:9", "cone", true},
		{"10.0.0.2:5", "1.2.3.4:9", "1.2.3.4:10", "symmetric", false},
		{"10.0.0.2:5", "", "1.2.3.4:10", "unknown", false},
	} {
		if nat, cand := ClassifyNAT(c.local, c.main, c.alt); nat != c.nat || cand != c.cand {
			t.Errorf("%+v → %s %v", c, nat, cand)
		}
	}
}
