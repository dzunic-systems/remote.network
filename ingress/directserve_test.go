package ingress

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/wire"
)

// directServeFixture arms a serving listener against a pinned root: a
// router key + certificate, a ticket factory, and a scripted backend.
type directServeFixture struct {
	t         *testing.T
	listener  *ClientListener
	nodeKey   ed25519.PrivateKey
	routerKey ed25519.PrivateKey
	cert      identity.RouterCert
	rootPub   ed25519.PublicKey
	clientKey ed25519.PrivateKey
	clientTr  *quic.Transport
	calls     atomic.Int64
	inflight  atomic.Int64
	// script is the backend; tests swap it after the listener started, so
	// it is read and written under scriptMu (the listener's goroutines
	// read it — a network round trip is no happens-before to the race
	// detector).
	scriptMu sync.Mutex
	script   func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error)
	lastReq  chan wire.InferenceRequestData
}

func newDirectServeFixture(t *testing.T, home string) *directServeFixture {
	t.Helper()
	f := &directServeFixture{t: t, lastReq: make(chan wire.InferenceRequestData, 8)}
	_, rootPriv, _ := identity.Generate()
	f.rootPub = rootPriv.Public().(ed25519.PublicKey)
	_, f.routerKey, _ = identity.Generate()
	cert, err := identity.SignRouterCert(rootPriv, f.routerKey.Public().(ed25519.PublicKey), time.Now().Add(24*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	f.cert = cert
	_, f.nodeKey, _ = identity.Generate()
	_, f.clientKey, _ = identity.Generate()
	f.script = func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		f.calls.Add(1)
		f.lastReq <- req
		for _, c := range []wire.InferenceChunkData{{Content: "hello"}} {
			if err := emit(c); err != nil {
				return wire.InferenceDoneData{}, err
			}
		}
		return wire.InferenceDoneData{InputTokens: 10, OutputTokens: 2}, nil
	}
	l, err := StartClientListener(context.Background(), ClientListenerConfig{
		Identity: f.nodeKey, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		LANAddrs:     func() []net.IP { return nil },
		Handler:      &fixtureHandler{f: f},
		RouterRoot:   identity.PubString(f.rootPub),
		HomeDir:      home,
		NodeInFlight: &f.inflight,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	f.listener = l

	sock, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	f.clientTr = &quic.Transport{Conn: sock}
	t.Cleanup(func() { f.clientTr.Close(); sock.Close() })
	return f
}

// setScript swaps the backend script (safe while the listener runs).
func (f *directServeFixture) setScript(fn func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error)) {
	f.scriptMu.Lock()
	f.script = fn
	f.scriptMu.Unlock()
}

// fixtureHandler adapts the script to InferenceHandler.
type fixtureHandler struct{ f *directServeFixture }

func (h *fixtureHandler) HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	h.f.scriptMu.Lock()
	fn := h.f.script
	h.f.scriptMu.Unlock()
	return fn(ctx, req, emit)
}

// ticket mints a router-signed ticket for this fixture's node and client.
func (f *directServeFixture) ticket(mutate func(*clientproto.Ticket)) clientproto.Ticket {
	f.t.Helper()
	now := time.Now()
	tk := clientproto.Ticket{
		ID:        clientproto.NewTicketID(),
		NodeID:    identity.ID(f.nodeKey.Public().(ed25519.PublicKey)),
		ModelID:   "qwen3:4b",
		ClientPub: identity.PubString(f.clientKey.Public().(ed25519.PublicKey)),
		Account:   "deadbeef",
		PriceIn:   10, PriceOut: 25, MaxTokens: 64, MaxReqs: 1,
		NotBefore: now.Unix(), NotAfter: now.Add(clientproto.TicketLifetime).Unix(),
		RouterID: identity.ID(f.routerKey.Public().(ed25519.PublicKey)),
	}
	if mutate != nil {
		mutate(&tk)
	}
	signed, err := clientproto.SignTicket(f.routerKey, f.cert, tk)
	if err != nil {
		f.t.Fatal(err)
	}
	return signed
}

// directRoundTrip dials the listener, opens a stream, sends the ticketed
// request, and returns every reply envelope (in order).
func (f *directServeFixture) directRoundTrip(tk clientproto.Ticket, req wire.InferenceRequestData, requestID string) []wire.Envelope {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.listener.Addr().Port))
	conn, err := clientproto.DialNode(ctx, f.clientTr, []string{ep}, f.clientKey,
		identity.PubString(f.nodeKey.Public().(ed25519.PublicKey)))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.CloseWithError(0, "done") })
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer stream.Close()
	if err := clientproto.SendDirectRequest(stream, clientproto.DirectRequestData{
		Ticket: tk, RequestID: requestID, Request: req,
	}, time.Second); err != nil {
		f.t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	sc := clientproto.NewEnvelopeScanner(stream)
	var out []wire.Envelope
	for sc.Scan() {
		var env wire.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			continue
		}
		out = append(out, env)
		if env.Type == wire.TypeInferenceDone || env.Type == wire.TypeInferenceError {
			break
		}
	}
	return out
}

// refusal runs a round trip and asserts the reply is inference.error with
// the given code and that the backend was never called.
func (f *directServeFixture) refusal(t *testing.T, tk clientproto.Ticket, req wire.InferenceRequestData, requestID, wantCode string) {
	t.Helper()
	before := f.calls.Load()
	out := f.directRoundTrip(tk, req, requestID)
	if len(out) != 1 || out[0].Type != wire.TypeInferenceError {
		t.Fatalf("replies = %+v, want one inference.error", out)
	}
	var e wire.ErrorData
	_ = json.Unmarshal(out[0].Data, &e)
	if e.Code != wantCode {
		t.Fatalf("code = %q (%s), want %q", e.Code, e.Message, wantCode)
	}
	if f.calls.Load() != before {
		t.Fatalf("backend called (%d → %d) on a refused request", before, f.calls.Load())
	}
}

func TestDirectServeRefusals(t *testing.T) {
	f := newDirectServeFixture(t, t.TempDir())
	req := wire.InferenceRequestData{Model: "qwen3:4b", Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}}, MaxTokens: 16}

	// Another node's ticket.
	otherNode := f.ticket(nil)
	_, otherKey, _ := identity.Generate()
	other := f.ticket(func(tk *clientproto.Ticket) { tk.NodeID = identity.ID(otherKey.Public().(ed25519.PublicKey)) })
	_ = otherNode
	f.refusal(t, other, req, "r-other-node", "ticket_invalid")

	// Expired (past the skew).
	expired := f.ticket(func(tk *clientproto.Ticket) {
		tk.NotBefore = time.Now().Add(-2 * time.Hour).Unix()
		tk.NotAfter = time.Now().Add(-1 * time.Hour).Unix()
	})
	f.refusal(t, expired, req, "r-expired", "ticket_expired")

	// Tampered counts.
	tampered := f.ticket(nil)
	tampered.MaxTokens = 99999 // signature no longer matches
	f.refusal(t, tampered, req, "r-tampered", "ticket_invalid")

	// Wrong model.
	f.refusal(t, f.ticket(nil), wire.InferenceRequestData{Model: "other/model", MaxTokens: 16}, "r-model", "ticket_model")

	// Revoked.
	revoked := f.ticket(nil)
	f.listener.Revoke([]string{revoked.ID})
	f.refusal(t, revoked, req, "r-revoked", "ticket_revoked")

	// Reused: the first use passes the checks (and serves), the second is
	// refused without touching the backend again.
	good := f.ticket(nil)
	if out := f.directRoundTrip(good, req, "r-first"); len(out) == 0 || out[len(out)-1].Type != wire.TypeInferenceDone {
		t.Fatalf("first use did not complete: %+v", out)
	}
	f.refusal(t, good, req, "r-second", "ticket_used")
}

// TestDirectServeWrongClientKey: a ticket binds ONE client key; a
// connection from another identity is refused at the check, past the TLS
// handshake.
func TestDirectServeWrongClientKey(t *testing.T) {
	f := newDirectServeFixture(t, t.TempDir())
	// The ticket binds the ORIGINAL client key; the connection dials with
	// an intruder's identity — past the TLS handshake, refused at the
	// ticket check.
	tk := f.ticket(nil)
	holdClient := f.clientKey
	_, intruderKey, _ := identity.Generate()
	f.clientKey = intruderKey
	defer func() { f.clientKey = holdClient }()
	f.refusal(t, tk, wire.InferenceRequestData{Model: "qwen3:4b", MaxTokens: 16}, "r-wrong-client", "ticket_client")
}

// TestDirectServeGoodRequest: the stream carries the chunks and the done
// frame, the receipt carries the ticket id, the node-wide counter moves
// and comes back, and the receipt is queued for the router (no fleet
// connected yet).
func TestDirectServeGoodRequest(t *testing.T) {
	home := t.TempDir()
	f := newDirectServeFixture(t, home)
	tk := f.ticket(nil)
	out := f.directRoundTrip(tk, wire.InferenceRequestData{Model: "qwen3:4b",
		Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}}, MaxTokens: 16}, "req-good")

	if len(out) < 2 || out[0].Type != wire.TypeInferenceChunk || out[len(out)-1].Type != wire.TypeInferenceDone {
		t.Fatalf("stream = %+v", out)
	}
	var done wire.InferenceDoneData
	_ = json.Unmarshal(out[len(out)-1].Data, &done)
	if done.Receipt == nil || done.Receipt.TicketID != tk.ID {
		t.Fatalf("done receipt = %+v (ticket id must ride it)", done.Receipt)
	}
	if f.inflight.Load() != 0 {
		t.Fatalf("node in-flight = %d after completion", f.inflight.Load())
	}
	// The receipt is queued on disk (no fleet to deliver to yet).
	entries, err := os.ReadDir(filepath.Join(home, "receipts"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("receipt not queued: %v %d", err, len(entries))
	}
	// The request's max_tokens rode clamped by the ticket.
	got := <-f.lastReq
	if got.MaxTokens != 16 {
		t.Fatalf("max_tokens = %d", got.MaxTokens)
	}
}

// TestDirectServeCapIsNotTheAsk (field, 2026-11-26): the ticket's cap is
// a billing ceiling, never handed to the backend as max_tokens — with no
// ask the router sets it to the whole window, and a backend handed that
// answered nothing. No ask, or an ask at or past the cap: the backend
// gets none (its default, as on the relay path).
func TestDirectServeCapIsNotTheAsk(t *testing.T) {
	for _, ask := range []int{0, 64, 9999} {
		f := newDirectServeFixture(t, t.TempDir())
		f.directRoundTrip(f.ticket(nil), wire.InferenceRequestData{Model: "qwen3:4b",
			Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}}, MaxTokens: ask}, "req-cap")
		if got := <-f.lastReq; got.MaxTokens != 0 {
			t.Fatalf("ask %d: backend max_tokens = %d, want 0 (the backend's default)", ask, got.MaxTokens)
		}
	}
}

// TestDirectServeStopsAtTheCap: the cap is kept by the node itself — a
// backend that streams past it is stopped at cap chunks, and the answer
// ends normally with finish "length".
func TestDirectServeStopsAtTheCap(t *testing.T) {
	f := newDirectServeFixture(t, t.TempDir())
	f.setScript(func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		for i := 0; i < 200; i++ {
			if err := emit(wire.InferenceChunkData{Content: "x"}); err != nil {
				return wire.InferenceDoneData{}, err
			}
		}
		return wire.InferenceDoneData{InputTokens: 10, OutputTokens: 200}, nil
	})
	envs := f.directRoundTrip(f.ticket(nil), wire.InferenceRequestData{Model: "qwen3:4b",
		Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}}}, "req-stop")
	content, finish, done := 0, "", false
	for _, env := range envs {
		switch env.Type {
		case wire.TypeInferenceChunk:
			var c wire.InferenceChunkData
			_ = json.Unmarshal(env.Data, &c)
			if c.Content != "" {
				content++
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		case wire.TypeInferenceDone:
			done = true
		case wire.TypeInferenceError:
			t.Fatalf("the cap ended in an error: %s", env.Data)
		}
	}
	if content != 64 || finish != "length" || !done {
		t.Fatalf("content chunks = %d (want the ticket's 64), finish = %q, done = %v", content, finish, done)
	}
}

// TestDirectServeBusy: the backend at capacity answers busy — the
// client's cue to try the next candidate.
func TestDirectServeBusy(t *testing.T) {
	f := newDirectServeFixture(t, t.TempDir())
	f.setScript(func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		return wire.InferenceDoneData{}, wire.ErrBusy
	})
	f.refusal(t, f.ticket(nil), wire.InferenceRequestData{Model: "qwen3:4b", MaxTokens: 16}, "r-busy", "busy")
}

// TestDirectRevokeAbortsRunningStream: a revoke mid-generation cancels
// the request's context (the backend stops) and the stream dies.
func TestDirectRevokeAbortsRunningStream(t *testing.T) {
	f := newDirectServeFixture(t, t.TempDir())
	started := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	f.setScript(func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		close(started)
		select {
		case <-ctx.Done():
			cancelled <- struct{}{}
			return wire.InferenceDoneData{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return wire.InferenceDoneData{}, nil
		}
	})
	// Drive the request from its own goroutine: it will not complete.
	tk := f.ticket(nil)
	streamErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ep := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.listener.Addr().Port))
		conn, err := clientproto.DialNode(ctx, f.clientTr, []string{ep}, f.clientKey,
			identity.PubString(f.nodeKey.Public().(ed25519.PublicKey)))
		if err != nil {
			streamErr <- err
			return
		}
		defer conn.CloseWithError(0, "done")
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			streamErr <- err
			return
		}
		defer stream.Close()
		if err := clientproto.SendDirectRequest(stream, clientproto.DirectRequestData{
			Ticket: tk, RequestID: "req-revoke",
			Request: wire.InferenceRequestData{Model: "qwen3:4b", MaxTokens: 16},
		}, time.Second); err != nil {
			streamErr <- err
			return
		}
		_ = stream.SetReadDeadline(time.Now().Add(6 * time.Second))
		sc := clientproto.NewEnvelopeScanner(stream)
		for sc.Scan() {
		}
		streamErr <- sc.Err()
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("backend never started")
	}
	f.listener.Revoke([]string{tk.ID})
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("revoke did not cancel the running request")
	}
}

// TestReceiptQueueDeliversOnReconnect: with no router up, the receipt
// queues; the sink coming alive (a reconnect) delivers it; the ack
// deletes the file.
func TestReceiptQueueDeliversOnReconnect(t *testing.T) {
	dir := t.TempDir()
	var delivered atomic.Int64
	var live atomic.Bool
	sink := func(routerID string, env wire.Envelope) bool {
		if !live.Load() {
			return false
		}
		delivered.Add(1)
		return true
	}
	q := newReceiptStore(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), sink)
	r, _ := clientproto.SignReceipt(signKey(t), wire.UsageReceipt{
		TicketID: "tk1", RequestID: "rq1", NodeID: "node-a", Status: "ok", At: time.Now().Unix(),
	})
	q.enqueue(r, "router-1")
	if n := countJSONs(t, dir); n != 1 {
		t.Fatalf("queued files = %d, want 1 (sink is down)", n)
	}
	live.Store(true)
	q.sweep(context.Background())
	if delivered.Load() != 1 {
		t.Fatalf("delivered = %d after the sweep", delivered.Load())
	}
	// Still on disk: the ack deletes it.
	if n := countJSONs(t, dir); n != 1 {
		t.Fatalf("files = %d before the ack, want 1", n)
	}
	q.noteAcked("tk1", "rq1")
	if n := countJSONs(t, dir); n != 0 {
		t.Fatalf("files = %d after the ack, want 0", n)
	}
}

func countJSONs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

func signKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// queuedReceipts reads every receipt the listener queued on disk.
func queuedReceipts(t *testing.T, home string) []wire.UsageReceipt {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "receipts"))
	if err != nil {
		return nil
	}
	var out []wire.UsageReceipt
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(home, "receipts", e.Name()))
		if err != nil {
			continue
		}
		var q queuedReceipt
		if json.Unmarshal(b, &q) == nil {
			out = append(out, q.Receipt)
		}
	}
	return out
}

// TestDirectServeCutOffStillBills (review F3): output that reached the
// client is billed even when the request ends in an error — the node
// queues a signed failed_mid_stream receipt for what it sent. On the
// direct path the router saw no stream; without this an answer cut just
// before done would be free.
func TestDirectServeCutOffStillBills(t *testing.T) {
	home := t.TempDir()
	f := newDirectServeFixture(t, home)
	f.setScript(func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		f.calls.Add(1)
		for _, c := range []wire.InferenceChunkData{{Content: "one two "}, {Content: "three four "}, {Content: "five"}} {
			if err := emit(c); err != nil {
				return wire.InferenceDoneData{}, err
			}
		}
		return wire.InferenceDoneData{}, errors.New("backend died mid-answer")
	})
	tk := f.ticket(nil)
	out := f.directRoundTrip(tk, wire.InferenceRequestData{Model: "qwen3:4b",
		Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}}, MaxTokens: 16}, "req-cut")
	if len(out) == 0 || out[len(out)-1].Type != wire.TypeInferenceError {
		t.Fatalf("stream = %+v, want chunks then inference.error", out)
	}
	recs := queuedReceipts(t, home)
	if len(recs) != 1 {
		t.Fatalf("queued receipts = %d, want 1", len(recs))
	}
	r := recs[0]
	// 3 chunks, 23 bytes → max(3, ceil(23/4)=6) = 6 tokens.
	if r.Status != "failed_mid_stream" || r.TicketID != tk.ID || r.OutputTokens != 6 {
		t.Fatalf("receipt = %+v, want failed_mid_stream with 6 output tokens", r)
	}
	if err := clientproto.VerifyReceipt(identity.PubString(f.nodeKey.Public().(ed25519.PublicKey)), r); err != nil {
		t.Fatalf("cut-off receipt does not verify: %v", err)
	}
}

// TestDirectServePreTokenFailureNoReceipt: nothing sent → nothing billed,
// no receipt (the pre-token rule every path shares).
func TestDirectServePreTokenFailureNoReceipt(t *testing.T) {
	home := t.TempDir()
	f := newDirectServeFixture(t, home)
	f.setScript(func(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
		f.calls.Add(1)
		return wire.InferenceDoneData{}, errors.New("backend down")
	})
	tk := f.ticket(nil)
	_ = f.directRoundTrip(tk, wire.InferenceRequestData{Model: "qwen3:4b", MaxTokens: 16}, "req-pre")
	if recs := queuedReceipts(t, home); len(recs) != 0 {
		t.Fatalf("pre-token failure queued %d receipts", len(recs))
	}
}
