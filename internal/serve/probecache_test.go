package serve

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"remote.network/node/catalog"
	"remote.network/node/internal/backend"
	"remote.network/node/internal/mapping"
)

// countingProber is a fake backend whose model listing is mutable and whose
// per-model probe calls are counted — the cache's whole contract is "the
// count must not grow for known models".
type countingProber struct {
	fakeBackend
	models []string
	calls  map[string]int
}

func newCountingProber(models ...string) *countingProber {
	return &countingProber{models: models, calls: map[string]int{}}
}

func (c *countingProber) ProbeModels(ctx context.Context) ([]string, error) { return c.models, nil }

func (c *countingProber) ProbeMeta(ctx context.Context, raw string) (backend.Meta, error) {
	c.calls[raw]++
	return backend.Meta{Family: "test", Ctx: 4096}, nil
}

func cacheTestRuntime(t *testing.T, dir string, fb *countingProber) *Runtime {
	t.Helper()
	cache := LoadProbeCache(filepath.Join(dir, "probe-cache.json"))
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	return &Runtime{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cache: cache,
		opts:  Options{NodeID: "ndrxwzjm2wszualstnxsoiz2uv", Home: dir, SkipBench: true, cache: cache},
		cat:   cat,
		m:     mapping.New(cat, nil),
		Backends: []BackendEntry{{
			ID: "b", Client: fb, Models: []ServedModel{}, sem: make(chan struct{}, 1),
		}},
	}
}

func TestProbeCacheSkipsKnownModels(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fb := newCountingProber("m1:7b", "m2:3b")
	rt := cacheTestRuntime(t, dir, fb)
	spec := BackendSpec{ID: "b"}

	// First characterization probes each model exactly once.
	rt.materializeCard(ctx, spec, fb, "openai-compatible", "m1:7b")
	rt.materializeCard(ctx, spec, fb, "openai-compatible", "m2:3b")
	if fb.calls["m1:7b"] != 1 || fb.calls["m2:3b"] != 1 {
		t.Fatalf("calls = %v, want one each", fb.calls)
	}

	// Same process, same models: cache hits, zero new probes.
	rt.materializeCard(ctx, spec, fb, "openai-compatible", "m1:7b")
	rt.materializeCard(ctx, spec, fb, "openai-compatible", "m2:3b")
	if fb.calls["m1:7b"] != 1 || fb.calls["m2:3b"] != 1 {
		t.Fatalf("cache miss within process: %v", fb.calls)
	}

	// "Restart": a fresh Runtime over the same home directory — persisted
	// cache still prevents re-probing.
	rt2 := cacheTestRuntime(t, dir, fb)
	card := rt2.materializeCard(ctx, spec, fb, "openai-compatible", "m1:7b")
	if fb.calls["m1:7b"] != 1 {
		t.Fatalf("restart re-probed: %v", fb.calls)
	}
	if card.Family != "test" || card.ModelID != "ndrxwzjm2wszualstnxsoiz2uv/m1:7b" {
		t.Fatalf("cached card lost facts: %+v", card)
	}

	// --probe-model forces exactly the named model.
	rt2.opts.ProbeModels = []string{"m1:7b"}
	rt2.materializeCard(ctx, spec, fb, "openai-compatible", "m1:7b")
	if fb.calls["m1:7b"] != 2 {
		t.Fatalf("explicit probe-model did not force: %v", fb.calls)
	}
	// ...and the untouched model stays cached.
	rt2.materializeCard(ctx, spec, fb, "openai-compatible", "m2:3b")
	if fb.calls["m2:3b"] != 1 {
		t.Fatalf("unrelated model re-probed: %v", fb.calls)
	}

	// --reprobe forces everything.
	rt2.opts.ForceReprobe = true
	rt2.opts.ProbeModels = nil
	rt2.materializeCard(ctx, spec, fb, "openai-compatible", "m1:7b")
	rt2.materializeCard(ctx, spec, fb, "openai-compatible", "m2:3b")
	if fb.calls["m1:7b"] != 3 || fb.calls["m2:3b"] != 2 {
		t.Fatalf("reprobe did not force: %v", fb.calls)
	}
}

func TestRefreshClaimsAndDrops(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fb := newCountingProber("m1:7b")
	rt := cacheTestRuntime(t, dir, fb)
	spec := BackendSpec{ID: "b"}
	first := rt.materializeCard(ctx, spec, fb, "openai-compatible", "m1:7b")
	rt.Backends[0].Models = []ServedModel{first}

	// m2 appears on the backend: Refresh claims it (probing only the new
	// model) and reports the change.
	fb.models = []string{"m1:7b", "m2:3b"}
	if !rt.Refresh(ctx) {
		t.Fatal("Refresh reported no change when a model appeared")
	}
	if fb.calls["m2:3b"] != 1 {
		t.Fatalf("new model probe calls = %v", fb.calls)
	}
	if fb.calls["m1:7b"] != 1 {
		t.Fatalf("known model re-probed by refresh: %v", fb.calls)
	}
	if len(rt.Models) != 2 {
		t.Fatalf("flat models = %d, want 2", len(rt.Models))
	}

	// A no-op listing leaves everything (and the report) unchanged.
	if rt.Refresh(ctx) {
		t.Fatal("Refresh reported change with identical listing")
	}

	// m2 disappears: dropped, m1 kept.
	fb.models = []string{"m1:7b"}
	if !rt.Refresh(ctx) {
		t.Fatal("drop not reported as a change")
	}
	if len(rt.Models) != 1 || rt.Models[0].RawID != "m1:7b" {
		t.Fatalf("models after drop = %+v", rt.Models)
	}
}
