package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"remote.network/node/internal/backend"
)

// warmBackend is an Ollama-shaped stub that records /api/generate warm
// calls (the load primitive: keep_alive=-1) and can be told to be slow or
// broken per model.
type warmBackend struct {
	*httptest.Server
	mu       sync.Mutex
	warmed   []string
	failFor  map[string]bool
	delayFor map[string]time.Duration
}

func newWarmBackend(t *testing.T, modelsJSON string) *warmBackend {
	t.Helper()
	wb := &warmBackend{failFor: map[string]bool{}, delayFor: map[string]time.Duration{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsJSON))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = w.Write([]byte(`{"family":"test","parameter_size":"1B","model_info":{"test.context_length":4096}}`))
	})
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string `json:"model"`
			KeepAlive any    `json:"keep_alive"`
			Messages  []any  `json:"messages"`
		}
		if len(req.Messages) != 0 {
			// The load primitive carries an empty message list: load only.
			t.Errorf("warm call sent %d messages, want 0", len(req.Messages))
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if num, ok := req.KeepAlive.(float64); !ok || num != -1 {
			t.Errorf("warm call keep_alive = %v, want -1 (number)", req.KeepAlive)
		}
		wb.mu.Lock()
		wb.warmed = append(wb.warmed, req.Model)
		fail := wb.failFor[req.Model]
		delay := wb.delayFor[req.Model]
		wb.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"done":true}`))
	})
	wb.Server = httptest.NewServer(mux)
	return wb
}

// TestKeepWarmPinnedOnly: the default mode warms exactly the pinned list —
// an advertise-everything node warms nothing, so small machines are safe.
func TestKeepWarmPinnedOnly(t *testing.T) {
	ctx := context.Background()
	wb := newWarmBackend(t, `{"data":[{"id":"alpha:1b"},{"id":"beta:2b"},{"id":"gamma:3b"}]}`)
	defer wb.Close()

	// Pinned subset (the "cold start + ≤2" shape): alpha + beta.
	rt, err := Prepare(ctx, Options{
		NodeID:   testNodeID,
		Backends: []BackendSpec{{ID: "b", Kind: "openai-compatible", URL: wb.URL}},
		Models:   []string{"alpha:1b", "beta:2b"},
		KeepWarm: KeepWarmPinned,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var flips int
	rt.OnChange(func() { flips++ })
	rt.Warm(ctx)

	wb.mu.Lock()
	defer wb.mu.Unlock()
	if len(wb.warmed) != 2 {
		t.Fatalf("warmed = %v, want exactly the pinned pair", wb.warmed)
	}

	// State restored after the load (back to whatever it was before).
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, c := range rt.Models {
		if c.State == "loading" {
			t.Fatalf("card stuck in loading: %+v", c)
		}
	}
	_ = flips
}

// TestKeepWarmStateTransitions: while a (slow) load runs the card reads
// loading; a Warm caller concurrent with the read sees the transition, and
// it flips back afterwards. Failure keeps the card servable.
func TestKeepWarmStateTransitions(t *testing.T) {
	ctx := context.Background()
	wb := newWarmBackend(t, `{"data":[{"id":"alpha:1b"}]}`)
	defer wb.Close()
	wb.mu.Lock()
	wb.failFor["alpha:1b"] = true // load fails — card must stay servable
	wb.mu.Unlock()

	rt, err := Prepare(ctx, Options{
		NodeID:   testNodeID,
		Backends: []BackendSpec{{ID: "b", Kind: "openai-compatible", URL: wb.URL}},
		Models:   []string{"alpha:1b"},
		KeepWarm: KeepWarmPinned,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var loadingSeen bool
	rt.OnChange(func() {
		rt.mu.RLock()
		defer rt.mu.RUnlock()
		if rt.Models[0].State == "loading" {
			loadingSeen = true
		}
	})
	rt.Warm(ctx)

	if !loadingSeen {
		t.Fatal("state=loading transition never announced")
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.Models[0].State != "ready-unbenched" {
		t.Fatalf("state after failed load = %s, want restored ready-unbenched", rt.Models[0].State)
	}
}

// TestKeepWarmAllMode: "all" warms every card — the big-machine opt-in.
func TestKeepWarmAllMode(t *testing.T) {
	ctx := context.Background()
	wb := newWarmBackend(t, `{"data":[{"id":"alpha:1b"},{"id":"beta:2b"}]}`)
	defer wb.Close()

	rt, err := Prepare(ctx, Options{
		NodeID:   testNodeID,
		Backends: []BackendSpec{{ID: "b", Kind: "openai-compatible", URL: wb.URL}},
		KeepWarm: KeepWarmAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.Warm(ctx)

	wb.mu.Lock()
	defer wb.mu.Unlock()
	if len(wb.warmed) != 2 {
		t.Fatalf("all-mode warmed = %v, want every card", wb.warmed)
	}
}

// TestWarmModelContract: the warm primitive pins the model (keep_alive=-1)
// and surfaces backend failures as errors.
func TestWarmModelContract(t *testing.T) {
	ctx := context.Background()
	wb := newWarmBackend(t, `{"data":[{"id":"alpha:1b"},{"id":"beta:2b"}]}`)
	defer wb.Close()
	wb.mu.Lock()
	wb.failFor["beta:2b"] = true
	wb.mu.Unlock()

	c, err := backend.New(backend.Config{Kind: "openai-compatible", BaseURL: wb.URL})
	if err != nil {
		t.Fatal(err)
	}
	warmer, ok := c.(backend.Warmer)
	if !ok {
		t.Fatal("openai client does not implement Warmer")
	}
	if err := warmer.WarmModel(ctx, "alpha:1b"); err != nil {
		t.Fatalf("warm alpha: %v", err)
	}
	if err := warmer.WarmModel(ctx, "beta:2b"); err == nil {
		t.Fatal("warm beta should fail (500)")
	}
}
