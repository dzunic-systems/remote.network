package ingress

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/wire"
)

func TestRouterHTTPBase(t *testing.T) {
	for in, want := range map[string]string{
		"wss://spine.remote.network/node/v1/connect": "https://spine.remote.network",
		"ws://127.0.0.1:8080/node/v1/connect/":       "http://127.0.0.1:8080",
		"https://spine.remote.network":               "https://spine.remote.network",
	} {
		if got := RouterHTTPBase(in); got != want {
			t.Errorf("RouterHTTPBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientListenerAdmitCaps(t *testing.T) {
	l := &ClientListener{perIP: map[string]int{}}
	a := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 1}
	for i := 0; i < clientMaxConnsPerIP; i++ {
		if _, err := l.admit(context.Background(), &quic.ClientInfo{RemoteAddr: a}); err != nil {
			t.Fatalf("conn %d refused: %v", i, err)
		}
	}
	if _, err := l.admit(context.Background(), &quic.ClientInfo{RemoteAddr: a}); err == nil {
		t.Error("per-IP cap not enforced")
	}
	l.release(a)
	if _, err := l.admit(context.Background(), &quic.ClientInfo{RemoteAddr: a}); err != nil {
		t.Errorf("released slot not reusable: %v", err)
	}
}

// TestClientListenerRefusesInference: until tickets land, a request on a direct
// stream is answered with direct_not_serving — nothing reaches a backend.
func TestClientListenerRefusesInference(t *testing.T) {
	_, nodeKey, _ := identity.Generate()
	l, err := StartClientListener(context.Background(), ClientListenerConfig{
		Identity: nodeKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		LANAddrs: func() []net.IP { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	sock, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	tr := &quic.Transport{Conn: sock}
	defer tr.Close()
	_, clientKey, _ := identity.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep := net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Addr().Port))
	conn, err := clientproto.DialNode(ctx, tr, []string{ep}, clientKey, identity.PubString(nodeKey.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := wire.NewReq(wire.TypeInferenceRequest, "", "r1", wire.InferenceRequestData{Model: "m"})
	if err := clientproto.WriteEnvelope(stream, env, time.Second); err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	sc := clientproto.NewEnvelopeScanner(stream)
	if !sc.Scan() {
		t.Fatalf("no answer: %v", sc.Err())
	}
	var reply wire.Envelope
	_ = json.Unmarshal(sc.Bytes(), &reply)
	var e wire.ErrorData
	_ = json.Unmarshal(reply.Data, &e)
	if reply.Type != wire.TypeInferenceError || e.Code != "direct_not_serving" {
		t.Errorf("reply = %s %+v", reply.Type, e)
	}
}

// TestClientListenerReleasesAbortedHandshakes: handshakes the client
// abandons (here: it rejects the node's key) never reach Accept; their
// connection slots must still come back, or one address is locked out
// after clientMaxConnsPerIP attempts.
func TestClientListenerReleasesAbortedHandshakes(t *testing.T) {
	_, nodeKey, _ := identity.Generate()
	l, err := StartClientListener(context.Background(), ClientListenerConfig{
		Identity: nodeKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		LANAddrs: func() []net.IP { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ep := net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Addr().Port))
	sock, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	tr := &quic.Transport{Conn: sock}
	defer tr.Close()
	_, clientKey, _ := identity.Generate()
	wrongPub, _, _ := identity.Generate()
	for i := 0; i < 2*clientMaxConnsPerIP; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if _, err := clientproto.DialNode(ctx, tr, []string{ep}, clientKey, identity.PubString(wrongPub)); err == nil {
			t.Fatal("dial with the wrong pinned key succeeded")
		}
		cancel()
	}
	// Slots come back as the server notices each close.
	deadline := time.Now().Add(3 * time.Second)
	for {
		l.mu.Lock()
		n := l.perIP["127.0.0.1"]
		l.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still held for 127.0.0.1 after aborted handshakes", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := clientproto.DialNode(ctx, tr, []string{ep}, clientKey, identity.PubString(nodeKey.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatalf("a good dial after %d aborted ones was refused: %v", 2*clientMaxConnsPerIP, err)
	}
	conn.CloseWithError(0, "")
}

func TestCombineNAT(t *testing.T) {
	cone := clientproto.Observation{Main: "1.2.3.4:5", Alt: "1.2.3.4:5", NAT: wire.NATCone, Candidate: true}
	if got, m := combineNAT(cone, clientproto.Observation{NAT: wire.NATSymmetric}, nil); got.NAT != wire.NATSymmetric || got.Candidate || m != "two routers: address-dependent mapping" {
		t.Errorf("address-dependent: %+v %q", got, m)
	}
	if got, m := combineNAT(cone, clientproto.Observation{NAT: wire.NATCone, Candidate: true}, nil); got.NAT != wire.NATCone || !got.Candidate || m != "two routers" {
		t.Errorf("cone confirmed: %+v %q", got, m)
	}
	if got, _ := combineNAT(cone, clientproto.Observation{}, clientproto.ErrNoObservation); got.NAT != wire.NATCone {
		t.Errorf("second router silent: %+v", got)
	}
}

// TestRouterUDPsPicksTwoIPsOneFamily: the NAT test's second router is on a
// different IP of the same family; seeds come first; duplicates skipped.
func TestRouterUDPsPicksTwoIPsOneFamily(t *testing.T) {
	serve := func(main, alt string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"udp_addr": main, "alt_udp_addr": alt})
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	a := serve("198.51.100.1:39081", "198.51.100.1:39082")
	sameIP := serve("198.51.100.1:49081", "198.51.100.1:49082")
	v6 := serve("[2001:db8::1]:39081", "[2001:db8::1]:39082")
	b := serve("203.0.113.9:39081", "203.0.113.9:39082")
	l := &ClientListener{cfg: ClientListenerConfig{
		RouterURLs:      []string{a},
		KnownRouterURLs: func() []string { return []string{a, sameIP, v6, b} },
		HTTPClient:      http.DefaultClient,
	}, routerInfo: map[string]routerUDP{}}
	got, err := l.routerUDPs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].main.String() != "198.51.100.1:39081" || got[1].main.String() != "203.0.113.9:39081" {
		t.Errorf("routers = %v", got)
	}
}
