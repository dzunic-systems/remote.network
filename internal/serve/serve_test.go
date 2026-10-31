package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"remote.network/node/internal/backend"
	"remote.network/node/wire"
)

// fakeBackend is a scripted backend.Client.
type fakeBackend struct {
	gotModel string
	deltas   []backend.Delta
	block    chan struct{} // non-nil: hold the stream open (concurrency tests)
}

func (f *fakeBackend) ProbeModels(ctx context.Context) ([]string, error) { return nil, nil }

func (f *fakeBackend) StreamChat(ctx context.Context, r backend.ChatRequest, onDelta func(backend.Delta) error) error {
	f.gotModel = r.Model
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, d := range f.deltas {
		if err := onDelta(d); err != nil {
			return err
		}
	}
	return nil
}

func TestHandleInferenceMapsCanonicalToRaw(t *testing.T) {
	fb := &fakeBackend{deltas: []backend.Delta{
		{Content: "hel"},
		{Content: "lo"},
		{FinishReason: "stop"},
		{Usage: &backend.Usage{InputTokens: 4, OutputTokens: 2}},
	}}
	rt := &Runtime{Client: fb, sem: make(chan struct{}, 1), Models: []ServedModel{
		{ModelID: "qwen/qwen3-4b", RawID: "qwen3:4b"},
	}}

	var chunks []wire.InferenceChunkData
	done, err := rt.HandleInference(context.Background(),
		wire.InferenceRequestData{
			Model:    "qwen/qwen3-4b", // canonical id in
			Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}},
		},
		func(c wire.InferenceChunkData) error { chunks = append(chunks, c); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if fb.gotModel != "qwen3:4b" { // raw id out
		t.Fatalf("backend saw model %q, want raw qwen3:4b", fb.gotModel)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (usage-only delta must not stream)", len(chunks))
	}
	if chunks[0].Content != "hel" || chunks[2].FinishReason != "stop" {
		t.Fatalf("chunks = %+v", chunks)
	}
	if done.InputTokens != 4 || done.OutputTokens != 2 {
		t.Fatalf("usage = %+v", done)
	}
}

func TestHandleInferenceUnknownModel(t *testing.T) {
	rt := &Runtime{Client: &fakeBackend{}, sem: make(chan struct{}, 1)}
	if _, err := rt.HandleInference(context.Background(),
		wire.InferenceRequestData{Model: "nope/nope"}, func(wire.InferenceChunkData) error { return nil }); err == nil {
		t.Fatal("unknown model accepted")
	}
}

func TestHandleInferenceBusy(t *testing.T) {
	fb := &fakeBackend{block: make(chan struct{})}
	rt := &Runtime{Client: fb, sem: make(chan struct{}, 1), Models: []ServedModel{
		{ModelID: "qwen/qwen3-4b", RawID: "qwen3:4b"},
	}}

	done := make(chan error, 1)
	go func() {
		_, err := rt.HandleInference(context.Background(),
			wire.InferenceRequestData{Model: "qwen/qwen3-4b"},
			func(wire.InferenceChunkData) error { return nil })
		done <- err
	}()

	// Second concurrent request must be rejected instantly with wire.ErrBusy
	// (the instant-failover trigger), not queued. Wait for the first to
	// actually hold the slot before probing.
	deadline := time.Now().Add(2 * time.Second)
	for len(rt.sem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first request never took its slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := rt.HandleInference(context.Background(),
		wire.InferenceRequestData{Model: "qwen/qwen3-4b"},
		func(wire.InferenceChunkData) error { return nil })
	if !errors.Is(err, wire.ErrBusy) {
		t.Fatalf("second request err = %v, want wire.ErrBusy", err)
	}
	close(fb.block)
	if err := <-done; err != nil {
		t.Fatalf("first request failed: %v", err)
	}
}

// fakeOllama serves the card-collection surfaces: /v1/models, /api/show,
// /api/tags, /v1/chat/completions (canned, fast), /v1/embeddings.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:4b"},{"id":"deepseek-r1:7b"},{"id":"nomic-embed-text:latest"}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Model {
		case "qwen3:4b":
			_, _ = w.Write([]byte(`{"family":"qwen3","parameter_size":"4B","quantization_level":"Q4_K_M",
				"capabilities":["completion"],"model_info":{"qwen3.context_length":32768}}`))
		case "deepseek-r1:7b":
			w.WriteHeader(http.StatusNotFound) // no metadata → heuristic path
		case "nomic-embed-text:latest":
			_, _ = w.Write([]byte(`{"family":"nomic-bert","parameter_size":"0.2B","quantization_level":"F16",
				"capabilities":["embedding"],"model_info":{"nomic-bert.context_length":8192}}`))
		}
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:4b","size":2500000000}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":3}}`))
	})
	return httptest.NewServer(mux)
}

const testNodeID = "ndbench0000000000000000000a"

func TestPrepareBuildsCards(t *testing.T) {
	ctx := context.Background()
	rt, err := Prepare(ctx, Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: fakeOllama(t).URL}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if len(rt.Models) != 3 || len(rt.Backends) != 1 {
		t.Fatalf("cards = %d across %d backends, want 3/1", len(rt.Models), len(rt.Backends))
	}
	byID := map[string]ServedModel{}
	for _, c := range rt.Models {
		byID[c.ModelID] = c
	}

	// Catalog-mapped: canonical id, catalog ctx, catalog price (card beats heuristic meta).
	// Prices are real money: qwen3-4b
	// tracks the catalog's first-band entry, 100000/300000 micro-USD per 1M.
	q := byID["qwen/qwen3-4b"]
	if q.RawID != "qwen3:4b" || q.Modality != "chat" || q.MaxCtx != 32768 ||
		q.PriceInPer1M != 100000 || q.PriceOutPer1M != 300000 || q.Family != "qwen3" ||
		q.ParamsB != 4 || q.DefaultQuant != "Q4_K_M" || q.SizeMB != 2384 || q.BackendID != "ollama" {
		t.Fatalf("canonical card = %+v", q)
	}

	// Unmapped: namespaced id, heuristic meta, curve-priced from heuristic params.
	d := byID[testNodeID+"/deepseek-r1:7b"]
	if d.RawID != "deepseek-r1:7b" || d.Modality != "chat" || d.Family != "deepseek" ||
		d.ParamsB != 7 || d.PriceInPer1M != 120000 || d.PriceOutPer1M != 400000 {
		t.Fatalf("namespaced card = %+v", d)
	}

	// Embedding card: classified from capabilities, bench-free ready state.
	e := byID[testNodeID+"/nomic-embed-text:latest"]
	if e.Modality != "embedding" || e.State != "ready" || e.MaxCtx != 8192 {
		t.Fatalf("embedding card = %+v", e)
	}

	// Operator price override beats the catalog.
	rt2, err := Prepare(ctx, Options{
		NodeID:        testNodeID,
		Backends:      []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: rt.Backends[0].URL}},
		SkipBench:     true,
		PriceInPer1M:  99,
		PriceOutPer1M: 199,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	if rt2.Models[0].PriceInPer1M != 99 || rt2.Models[0].PriceOutPer1M != 199 {
		t.Fatalf("override card prices = %d/%d", rt2.Models[0].PriceInPer1M, rt2.Models[0].PriceOutPer1M)
	}

	// --model filter matches raw OR card id, and unknown names error.
	rt3, err := Prepare(ctx, Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: rt.Backends[0].URL}},
		SkipBench: true,
		Models:    []string{"deepseek-r1:7b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt3.Close()
	if len(rt3.Models) != 1 || rt3.Models[0].RawID != "deepseek-r1:7b" {
		t.Fatalf("filtered cards = %+v", rt3.Models)
	}
	if _, err := Prepare(ctx, Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: rt.Backends[0].URL}},
		SkipBench: true,
		Models:    []string{"not-there"},
	}); err == nil {
		t.Fatal("unknown --model accepted")
	}
}

func TestHandleEmbedRoutesToRightBackend(t *testing.T) {
	ctx := context.Background()
	rt, err := Prepare(ctx, Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: fakeOllama(t).URL}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	embedID := testNodeID + "/nomic-embed-text:latest"
	done, err := rt.HandleEmbed(ctx, wire.EmbedRequestData{Model: embedID, Input: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Embeddings) != 1 || len(done.Embeddings[0]) != 2 || done.InputTokens != 3 {
		t.Fatalf("embed done = %+v", done)
	}

	// Chat card via the embed surface → refused; embedding card via chat → refused.
	if _, err := rt.HandleEmbed(ctx, wire.EmbedRequestData{Model: "qwen/qwen3-4b", Input: []string{"x"}}); err == nil {
		t.Fatal("chat card accepted by HandleEmbed")
	}
	if _, err := rt.HandleInference(ctx, wire.InferenceRequestData{
		Model:    embedID,
		Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}},
	}, func(wire.InferenceChunkData) error { return nil }); err == nil {
		t.Fatal("embedding card accepted by HandleInference")
	}
	if _, err := rt.HandleEmbed(ctx, wire.EmbedRequestData{Model: embedID, Input: nil}); err == nil {
		t.Fatal("empty embed input accepted")
	}
}

// residencerFake answers /api/ps-style residency questions.
type residencerFake struct {
	fakeBackend
	loaded map[string]int
}

func (f *residencerFake) ProbeResidency(ctx context.Context) (map[string]int, error) {
	return f.loaded, nil
}

// TestColdLoadAnnouncesLoading: a dispatch for a model the backend lists
// but hasn't loaded flips the card to loading for the load's duration and
// back the moment the model answers — the router's window-stretching hook.
func TestColdLoadAnnouncesLoading(t *testing.T) {
	fb := &residencerFake{loaded: map[string]int{}} // model not resident
	fb.deltas = []backend.Delta{{Content: "a"}, {FinishReason: "stop"}}
	rt := &Runtime{Client: fb, sem: make(chan struct{}, 1), Models: []ServedModel{
		{ModelID: "qwen/qwen3-4b", RawID: "qwen3:4b", State: "ready-unbenched"},
	}}
	var flips []string
	rt.OnChange(func() {
		rt.mu.RLock()
		flips = append(flips, rt.Models[0].State)
		rt.mu.RUnlock()
	})

	_, err := rt.HandleInference(context.Background(),
		wire.InferenceRequestData{
			Model:    "qwen/qwen3-4b",
			Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}},
		},
		func(c wire.InferenceChunkData) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.Models[0].State != "ready-unbenched" {
		t.Fatalf("state after load = %q, want restored", rt.Models[0].State)
	}
	if len(flips) != 2 || flips[0] != "loading" || flips[1] != "ready-unbenched" {
		t.Fatalf("flips = %v, want [loading ready-unbenched]", flips)
	}
}

// TestWarmModelSkipsLoadingFlip: a resident model never flips state —
// the dispatch must not announce loads that are not happening.
func TestWarmModelSkipsLoadingFlip(t *testing.T) {
	fb := &residencerFake{loaded: map[string]int{"qwen3:4b": 40960}}
	fb.deltas = []backend.Delta{{Content: "a"}, {FinishReason: "stop"}}
	rt := &Runtime{Client: fb, sem: make(chan struct{}, 1), Models: []ServedModel{
		{ModelID: "qwen/qwen3-4b", RawID: "qwen3:4b", State: "ready"},
	}}
	flipped := false
	rt.OnChange(func() { flipped = true })

	_, err := rt.HandleInference(context.Background(),
		wire.InferenceRequestData{
			Model:    "qwen/qwen3-4b",
			Messages: []wire.InferenceMessage{{Role: "user", Content: "hi"}},
		},
		func(c wire.InferenceChunkData) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if flipped {
		t.Fatal("warm model flipped state")
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.Models[0].State != "ready" {
		t.Fatalf("state changed to %q", rt.Models[0].State)
	}
}

// lateBackend serves 503s until raised, then a /v1/models listing. It
// stands in for a vLLM service the operator starts after the agent.
type lateBackend struct {
	up atomic.Bool
}

func (l *lateBackend) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		if !l.up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		// qwen3:4b duplicates the up backend's card (first wins); phi3:mini is new.
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:4b"},{"id":"phi3:mini"}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // heuristic metadata path
	})
	return httptest.NewServer(mux)
}

// TestAdmitPendingPicksUpLateBackend: a backend that is down at startup
// stays out of the serving set but is admitted by admitPending once its
// endpoint answers — cards join both views under the Prepare rules
// (first backend wins a shared id), the heartbeat/re-advertise hook
// fires, and an empty retry list makes later calls no-ops.
func TestAdmitPendingPicksUpLateBackend(t *testing.T) {
	late := &lateBackend{}
	lateSrv := late.serve(t)
	defer lateSrv.Close()

	rt, err := Prepare(context.Background(), Options{
		NodeID: testNodeID,
		Backends: []BackendSpec{
			{ID: "up", Kind: "openai-compatible", URL: fakeOllama(t).URL},
			{ID: "late", Kind: "openai-compatible", URL: lateSrv.URL},
		},
		SkipBench:       true,
		AdmitRetryEvery: time.Minute, // loop runs off the caller's ctx; not started here
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	rt.mu.RLock()
	if len(rt.Backends) != 1 || len(rt.pending) != 1 || rt.pending[0].ID != "late" {
		pend := rt.pending
		rt.mu.RUnlock()
		t.Fatalf("after startup: backends=%d pending=%+v", len(rt.Backends), pend)
	}
	rt.mu.RUnlock()
	if _, _, ok := rt.entryFor(testNodeID + "/phi3:mini"); ok {
		t.Fatal("late card served before admission")
	}

	admitted := 0
	rt.OnChange(func() { admitted++ })
	late.up.Store(true)
	if !rt.admitPending(context.Background()) {
		t.Fatal("admitPending reported no change after the backend came up")
	}

	rt.mu.RLock()
	backends, models, pend := len(rt.Backends), len(rt.Models), len(rt.pending)
	rt.mu.RUnlock()
	if backends != 2 || pend != 0 {
		t.Fatalf("after admission: backends=%d pending=%d, want 2/0", backends, pend)
	}
	// 4 cards total: up backend's 3 + late backend's phi3 (its qwen3:4b
	// duplicates the up backend's canonical id and loses).
	if models != 4 {
		rt.mu.RLock()
		ids := make([]string, len(rt.Models))
		for i, m := range rt.Models {
			ids[i] = m.ModelID
		}
		rt.mu.RUnlock()
		t.Fatalf("flat view = %v (%d), want 4 cards", ids, models)
	}
	entry, _, ok := rt.entryFor(testNodeID + "/phi3:mini")
	if !ok || entry.ID != "late" {
		t.Fatalf("late card not routable to the late backend (ok=%v entry=%+v)", ok, entry)
	}
	if admitted == 0 {
		t.Fatal("admission did not fire the re-advertise hook")
	}
	if rt.admitPending(context.Background()) {
		t.Fatal("admitPending changed something with an empty retry list")
	}
}

// fakeVLLM is an openai-dialect server (no /api/show): /v1/models lists
// one chat model; the chat endpoint 200s text requests and 400s image
// parts — a tool-capable, text-only vLLM.
func fakeVLLM(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen38-27b","max_model_len":262144}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte("image_url")) {
			w.WriteHeader(http.StatusBadRequest) // text-only model
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	return httptest.NewServer(mux)
}

// TestProbeDetectedCapabilitiesOnCards: a vLLM-shaped backend with no
// self-reported capabilities and no operator pins still advertises
// tools=true (probe 200), never vision (probe 400), and the probe result
// lands in the Capabilities list exactly once.
func TestProbeDetectedCapabilitiesOnCards(t *testing.T) {
	rt, err := Prepare(context.Background(), Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "vllm", Kind: "openai-compatible", URL: fakeVLLM(t).URL}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	card := rt.Models[0]
	if card.RawID != "qwen38-27b" || !card.Tools || card.Vision {
		t.Fatalf("card = %+v, want raw qwen38-27b tools=true vision=false", card)
	}
	n := 0
	for _, c := range card.Capabilities {
		if c == "tools" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("capabilities = %v, want exactly one tools entry", card.Capabilities)
	}
}

// TestListedSupportedParametersClaimCaps: a ds4-server/OpenRouter-shaped
// listing (supported_parameters on each model, no /api/show) feeds
// tools + thinking straight into the cards — no probes needed for what
// the listing already declares.
func TestListedSupportedParametersClaimCaps(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5","max_model_len":131072,
			"supported_parameters":["tools","reasoning"]}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	srv := httptest.NewServer(mux)

	rt, err := Prepare(context.Background(), Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "ds4", Kind: "openai-compatible", URL: srv.URL}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	card := rt.Models[0]
	if !card.Tools || !card.Reasoning {
		t.Fatalf("card = %+v, want tools+reasoning from supported_parameters", card)
	}
}

// TestManifestMerge: the model-cards manifest fills the descriptive fields
// of a built card — keyed by raw id (the operator's natural handle) — and
// never touches the collected or measured facts.
func TestManifestMerge(t *testing.T) {
	ctx := context.Background()
	rt, err := Prepare(ctx, Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: fakeOllama(t).URL}},
		SkipBench: true,
		Manifest: map[string]CardMeta{
			"qwen3:4b": {
				DisplayName: "Qwen3 4B (Serbian legal)",
				Description: "LoRA fine-tune of qwen3-4b on Serbian legal text",
				License:     "apache-2.0",
				BaseModel:   "qwen/qwen3-8b",
				Origin:      "fine-tune",
				Version:     "2026-10-15.1",
				Links:       []string{"https://huggingface.co/me/qwen3-4b-sr-legal"},
				Languages:   []string{"sr-Latn", "en"},
			},
			// Canonical-id keying works for a card the manifest author
			// knows by its network id instead.
			"nomic-embed-text:latest": {Origin: "catalog", License: "mit"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var qwen, nomic *ServedModel
	for i := range rt.Models {
		switch rt.Models[i].RawID {
		case "qwen3:4b":
			qwen = &rt.Models[i]
		case "nomic-embed-text:latest":
			nomic = &rt.Models[i]
		}
	}
	if qwen == nil || nomic == nil {
		t.Fatalf("cards missing: %+v", rt.Models)
	}
	if qwen.DisplayName != "Qwen3 4B (Serbian legal)" || qwen.Description == "" ||
		qwen.License != "apache-2.0" || qwen.BaseModel != "qwen/qwen3-8b" ||
		qwen.Origin != "fine-tune" || qwen.Version != "2026-10-15.1" ||
		len(qwen.Links) != 1 || len(qwen.Languages) != 2 {
		t.Fatalf("manifest not merged: %+v", qwen)
	}
	// Collected facts stay the agent's: family/quant from the probe, price
	// from the catalog, identity from the mapping.
	if qwen.Family != "qwen3" || qwen.ModelID != "qwen/qwen3-4b" || qwen.PriceOutPer1M == 0 {
		t.Fatalf("manifest clobbered collected facts: %+v", qwen)
	}
	if nomic.Origin != "catalog" || nomic.License != "mit" {
		t.Fatalf("canonical-keyed entry not merged: %+v", nomic)
	}
}

// TestLoadManifest: the YAML file loads keyed entries; a missing file is
// quiet and nil; a broken file warns and yields nil (never bricks serving).
func TestLoadManifest(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := loadManifest(filepath.Join(t.TempDir(), "absent.yaml"), log); got != nil {
		t.Fatalf("missing file = %v, want nil", got)
	}
	path := filepath.Join(t.TempDir(), DefaultManifestName)
	yamlBody := `
qwen3:4b:
  display_name: "Qwen3 4B (Serbian legal)"
  description: fine-tune
  origin: fine-tune
  links:
    - https://huggingface.co/me/tune
`
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadManifest(path, log)
	if len(got) != 1 || got["qwen3:4b"].DisplayName != "Qwen3 4B (Serbian legal)" ||
		got["qwen3:4b"].Origin != "fine-tune" || len(got["qwen3:4b"].Links) != 1 {
		t.Fatalf("manifest parsed wrong: %+v", got)
	}
	broken := filepath.Join(t.TempDir(), DefaultManifestName)
	if err := os.WriteFile(broken, []byte("qwen3:4b: [not: valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadManifest(broken, log); got != nil {
		t.Fatalf("broken file = %v, want nil", got)
	}
}
