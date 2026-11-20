package serve

import (
	"testing"
	"time"
)

// §5 N4: the busy ring counts ErrBusy refusals in the last minute and
// prunes what aged out — a refusal is capacity pressure, counted per
// backend, gone when the minute is.
func TestBusyRing(t *testing.T) {
	r := &busyRing{}
	now := time.Now()
	r.note(now.Add(-2 * time.Minute)) // stale
	r.note(now.Add(-30 * time.Second))
	r.note(now)
	if got := r.count(now); got != 2 {
		t.Fatalf("count = %d, want 2 (the stale refusal pruned)", got)
	}
	// Counts don't consume: the two refusals are still inside their minute.
	if got := r.count(now); got != 2 {
		t.Fatalf("second count = %d, want 2", got)
	}
	// Past the minute they age out.
	if got := r.count(now.Add(61 * time.Second)); got != 0 {
		t.Fatalf("aged count = %d, want 0", got)
	}
}

// LiveBackends reports the agent's own truth per backend: slots, held,
// busy — the fields every backend has regardless of dialect.
func TestLiveBackends(t *testing.T) {
	rt := &Runtime{
		Backends: []BackendEntry{{
			ID: "tf", Kind: "tensorfold", URL: "http://127.0.0.1:8888/v1",
			Models: []ServedModel{{ModelID: "m1", RawID: "raw1"}}, sem: make(chan struct{}, 4),
			busy: &busyRing{},
		}},
	}
	rt.Backends[0].sem <- struct{}{}
	rt.Backends[0].sem <- struct{}{}
	rt.Backends[0].busy.note(time.Now())

	live := rt.LiveBackends()
	if len(live) != 1 {
		t.Fatalf("live = %d", len(live))
	}
	b := live[0]
	if b.Slots != 4 || b.Held != 2 || b.Busy1m != 1 || b.Models[0] != "m1" {
		t.Fatalf("backend live = %+v", b)
	}
}
