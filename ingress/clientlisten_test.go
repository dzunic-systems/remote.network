package ingress

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net"
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
