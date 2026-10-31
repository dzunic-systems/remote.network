package ingress

import (
	"context"
	"slices"
	"testing"
)

// The node CLI sets the serving list before Run — no client exists yet —
// and directory attaches arrive after the last change. Every client the
// fleet creates must start with the current list, or the router sees a
// session that heartbeats an empty serving set.
func TestFleetServingListSeedsNewClients(t *testing.T) {
	f := NewFleet(FleetConfig{})
	f.SetServingModels([]string{"m1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the attached client's Run exits at once; only its config matters
	if err := f.attach(ctx, "ws://127.0.0.1:1/node/v1/connect", true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	var got []string
	for _, a := range f.clients {
		a.client.servingMu.Lock()
		got = a.client.serving
		a.client.servingMu.Unlock()
	}
	f.mu.Unlock()
	if !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("new client serving = %v, want [m1]", got)
	}
}
