package ingress

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"remote.network/node/wire"
)

// blockingHandler holds every dispatch until the test releases it — the
// same runHandler core the WSS and QUIC data planes share, so the counts
// move exactly as a real dispatch would.
type blockingHandler struct{ release chan struct{} }

func (h *blockingHandler) HandleInference(ctx context.Context, req wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	select {
	case <-h.release:
		return wire.InferenceDoneData{}, nil
	case <-ctx.Done():
		return wire.InferenceDoneData{}, ctx.Err()
	}
}

// TestNodeInFlightIsFleetWide is §8's N0 case: a fleet of two clients
// (two routers, one shared NodeInFlight counter) holding 2 and 3
// dispatches. BOTH heartbeats must carry in_flight=5 — a spine must see
// the load the other spine is creating — with each client's own share in
// in_flight_conn and scope "node". Negative control: without the shared
// counter the first heartbeat would read 2 (the per-connection count the
// pre-N0 agent sent, the F0 bug).
func TestNodeInFlightIsFleetWide(t *testing.T) {
	shared := &atomic.Int64{}
	h := &blockingHandler{release: make(chan struct{})}
	mk := func() *Client {
		c := testClient(t.TempDir(), Config{NodeInFlight: shared, Handler: h})
		return c
	}
	a, b := mk(), mk()

	hold := func(c *Client, n int64) {
		for i := 0; i < int(n); i++ {
			go c.runHandler(context.Background(), "r", wire.InferenceRequestData{Model: "m"}, func(wire.InferenceChunkData) error { return nil })
		}
		waitCounter(t, &c.inFlight, n)
	}
	hold(a, 2)
	hold(b, 3)

	for _, c := range []*Client{a, b} {
		hb := c.heartbeatData()
		if hb.InFlight != 5 || hb.InFlightScope != wire.InFlightScopeNode {
			t.Fatalf("heartbeat in_flight=%d scope=%q, want 5/node (F0: a per-client counter would read 2)", hb.InFlight, hb.InFlightScope)
		}
	}
	if got := a.heartbeatData().InFlightConn; got != 2 {
		t.Fatalf("client A in_flight_conn=%d, want 2", got)
	}
	if got := b.heartbeatData().InFlightConn; got != 3 {
		t.Fatalf("client B in_flight_conn=%d, want 3", got)
	}

	// The wire shape: scope and conn count survive the JSON round trip an
	// old router's decoder performs (it ignores the new fields; a new
	// router must find them).
	blob, err := json.Marshal(a.heartbeatData())
	if err != nil {
		t.Fatal(err)
	}
	var back wire.HeartbeatData
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.InFlight != 5 || back.InFlightConn != 2 || back.InFlightScope != wire.InFlightScopeNode {
		t.Fatalf("round trip: %+v", back)
	}

	// Release: both counters drain to zero (no leaked dispatch).
	close(h.release)
	waitCounter(t, &a.inFlight, 0)
	waitCounter(t, shared, 0)
}

// TestInflightScopeConnWithoutSharedCounter pins the legacy shape: a
// client built without NodeInFlight reports its own count and says so —
// a new router reading it must treat in_flight as connection-scoped, not
// mistake it for the node total.
func TestInflightScopeConnWithoutSharedCounter(t *testing.T) {
	c := testClient(t.TempDir(), Config{})
	c.inFlight.Store(3)
	hb := c.heartbeatData()
	if hb.InFlight != 3 || hb.InFlightScope != wire.InFlightScopeConn || hb.InFlightConn != 0 {
		t.Fatalf("legacy heartbeat = %+v, want in_flight 3, scope conn", hb)
	}
}

func waitCounter(t *testing.T, c *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Load() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("counter stuck at %d, want %d", c.Load(), want)
}
