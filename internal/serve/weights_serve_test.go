package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tensorFoldFake serves the listing shape TensorFold answers with (seen
// live 16 Nov): no /api/show, no files named — just max_model_len and the
// reasoning object. rawID must not be catalog-mapped so the card's ctx
// comes from the listing, exercising exactly the §2.5 mechanism.
func tensorFoldFake(t *testing.T, rawID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"` + rawID + `","max_model_len":131072,
			"reasoning":{"default_enabled":false,"supported_efforts":["xhigh","medium","low","none"]}}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // the caps probes get a plain refusal; the listing already declared thinking
	})
	return httptest.NewServer(mux)
}

// TestTensorFoldCardRestoresThinking: the listing's reasoning object puts
// thinking + the effort vocabulary on the card without any operator pin,
// and max_model_len becomes both ctx fields.
func TestTensorFoldCardRestoresThinking(t *testing.T) {
	const raw = "tf-qwen38-flash"
	home := t.TempDir()
	url := tensorFoldFake(t, raw).URL
	rt, err := Prepare(t.Context(), Options{
		Home:      home,
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "tf", Kind: "openai-compatible", URL: url}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	assertThinking := func(card ServedModel, where string) {
		t.Helper()
		if !card.Reasoning {
			t.Fatalf("%s: card.Reasoning false (capabilities %v)", where, card.Capabilities)
		}
		var thinking bool
		for _, c := range card.Capabilities {
			if c == "thinking" {
				thinking = true
			}
		}
		if !thinking {
			t.Fatalf("%s: capabilities = %v, want thinking", where, card.Capabilities)
		}
		if strings.Join(card.ReasoningEfforts, ",") != "xhigh,medium,low" {
			t.Fatalf("%s: efforts = %v, want [xhigh medium low] (none dropped, order kept)", where, card.ReasoningEfforts)
		}
		if card.ServingCtx != 131072 || card.MaxCtx != 131072 {
			t.Fatalf("%s: serving/max ctx = %d/%d, want 131072/131072 from max_model_len", where, card.ServingCtx, card.MaxCtx)
		}
	}
	assertThinking(rt.Models[0], "fresh probe")

	// A restart rides the probe cache: the efforts persist with the rest
	// of the characterization (CachedModel.Efforts).
	rt2, err := Prepare(t.Context(), Options{
		Home:      home,
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "tf", Kind: "openai-compatible", URL: url}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	if got := rt2.Models[0]; got.ReasoningEfforts == nil {
		t.Fatalf("cached card lost efforts: %+v", got)
	} else {
		assertThinking(got, "cached")
	}
}

// TestWeightsDeclarationWins: the operator's --weights declaration is
// precedence 1 — it pins the id over anything the backend could say, and
// the fingerprint stays empty while the snapshot is not on this machine
// (the TensorFold/container case).
func TestWeightsDeclarationWins(t *testing.T) {
	const raw = "tf-qwen38-flash"
	decl := "nvidia/Qwen3.8-Flash-Next-FP4-Local@b9ce18a6887e18c089fdea8509a33e63429a3300"
	rt, err := Prepare(t.Context(), Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "tf", Kind: "openai-compatible", URL: tensorFoldFake(t, raw).URL}},
		SkipBench: true,
		Weights:   map[string]string{raw: decl},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	card := rt.Models[0]
	if card.WeightsID != "hf:"+decl {
		t.Fatalf("weights id = %q, want %q", card.WeightsID, "hf:"+decl)
	}
	if card.WeightsFingerprint != "" {
		t.Fatalf("fingerprint = %q, want empty (snapshot not on this machine)", card.WeightsFingerprint)
	}
	// Card-id matching works too (the operator names the card, not the raw).
	rt2, err := Prepare(t.Context(), Options{
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "tf", Kind: "openai-compatible", URL: tensorFoldFake(t, raw).URL}},
		SkipBench: true,
		Weights:   map[string]string{testNodeID + "/" + raw: decl},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	if rt2.Models[0].WeightsID != "hf:"+decl {
		t.Fatalf("card-id declaration ignored: %q", rt2.Models[0].WeightsID)
	}
}

// TestVLLMRootWeightsID: a vLLM listing whose root names a HF snapshot
// (container prefix and all) yields the exact hf: id without any pin; a
// bare org/name resolves through the local hub cache's refs/main; a local
// dir yields nothing at all. Never a guessed revision.
func TestVLLMRootWeightsID(t *testing.T) {
	const rev = "b9ce18a6887e18c089fdea8509a33e63429a3300"

	// Bare-repo resolution needs a local hub cache with refs/main.
	hfCache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hfCache, "models--acme--base", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hfCache, "models--acme--base", "refs", "main"), []byte(rev+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		root string
		hf   string // HFCache option
		want string
	}{
		{"container snapshot path", "/root/.cache/huggingface/hub/models--nvidia--Qwen3.8-Flash-Next-FP4-Local/snapshots/" + rev, "", "hf:nvidia/Qwen3.8-Flash-Next-FP4-Local@" + rev},
		{"bare repo with local ref", "acme/base", hfCache, "hf:acme/base@" + rev},
		{"bare repo without local ref", "acme/missing", hfCache, ""},
		{"local dir", "/mnt/models/local-weights", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"vllm-model","root":"` + tc.root + `"}]}`))
			})
			mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			})
			mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			})
			srv := httptest.NewServer(mux)
			rt, err := Prepare(t.Context(), Options{
				NodeID:    testNodeID,
				Backends:  []BackendSpec{{ID: "vllm", Kind: "openai-compatible", URL: srv.URL}},
				SkipBench: true,
				HFCache:   tc.hf,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			if got := rt.Models[0].WeightsID; got != tc.want {
				t.Fatalf("weights id = %q, want %q (root %q)", got, tc.want, tc.root)
			}
		})
	}
}

// TestGGUFWeightsBackground: a llama.cpp card whose model_path names a
// readable GGUF publishes without a weights id first, then gets
// gguf:sha256:… when the background hash lands (with a capability update
// via the OnChange hook), memoized in ~/.remote/weights-cache.json and in
// the probe cache — a restart carries the id with no re-hash.
func TestGGUFWeightsBackground(t *testing.T) {
	home := t.TempDir()
	gguf := filepath.Join(home, "qwen3-8b-q4_k_m.gguf")
	payload := []byte("pretend this is thirty gigabytes of weights")
	if err := os.WriteFile(gguf, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	wantHex := sha256.Sum256(payload)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model"}]}`))
	})
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, _ *http.Request) {
		// json.Marshal quotes the path: a Windows temp dir's backslashes
		// would otherwise be invalid JSON escapes.
		q, _ := json.Marshal(gguf)
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192},"model_path":` + string(q) + `}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)

	changes := make(chan struct{}, 16)
	rt, err := Prepare(t.Context(), Options{
		Home:      home,
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "llama", Kind: "openai-compatible", URL: srv.URL}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.OnChange(func() { changes <- struct{}{} })

	// Snapshot the card under the runtime's lock — the background hasher
	// writes these fields under the write lock.
	weightsOf := func() (id, fp string) {
		rt.mu.RLock()
		defer rt.mu.RUnlock()
		return rt.Models[0].WeightsID, rt.Models[0].WeightsFingerprint
	}

	// Whether the digest landed via the flush at Prepare's end (fast hash,
	// card views assembled first) or by direct apply after it, the id must
	// be on the card now or within moments.
	want := "gguf:sha256:" + hex.EncodeToString(wantHex[:])
	deadline := time.Now().Add(5 * time.Second)
	for {
		id, fp := weightsOf()
		if id == want && fp == hex.EncodeToString(wantHex[:]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("weights id never landed: %q (fp %q)", id, fp)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The probe cache carries it for the restart.
	if c, ok := rt.cache.Get("llama|local-model"); !ok || c.WeightsID != want {
		t.Fatalf("probe cache = %+v (ok %v), want weights id %q", c, ok, want)
	}

	// A second digest hashes strictly after setup — the direct background
	// path a real tens-of-GB hash takes. The card updates and the change
	// hook (→ SendCapabilityUpdate) fires when it lands.
	payload2 := []byte("the replaced weights, also pretend-gigantic")
	gguf2 := filepath.Join(home, "qwen3-8b-v2.gguf")
	if err := os.WriteFile(gguf2, payload2, 0o644); err != nil {
		t.Fatal(err)
	}
	sum2 := sha256.Sum256(payload2)
	want2 := "gguf:sha256:" + hex.EncodeToString(sum2[:])
	rt.mu.RLock()
	modelID := rt.Models[0].ModelID
	rt.mu.RUnlock()
	rt.hashGGUFLater(modelID, gguf2, "llama|local-model")
	deadline = time.Now().Add(5 * time.Second)
	for {
		id, _ := weightsOf()
		if id == want2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement weights id never landed: %q", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-changes:
	case <-time.After(2 * time.Second):
		t.Fatal("no capability update fired when the hash landed")
	}
	if c, _ := rt.cache.Get("llama|local-model"); c.WeightsID != want2 {
		t.Fatalf("probe cache not updated with the new digest: %+v", c)
	}
	rt.Close()

	// Restart: the card states what the backend loads NOW (/props still
	// names the first file), never the identity the probe cache last
	// recorded — and the digest memo answers it instantly, already on the
	// card when Prepare returns: no re-hash, no gap.
	rt2, err := Prepare(t.Context(), Options{
		Home:      home,
		NodeID:    testNodeID,
		Backends:  []BackendSpec{{ID: "llama", Kind: "openai-compatible", URL: srv.URL}},
		SkipBench: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	if got := rt2.Models[0].WeightsID; got != want {
		t.Fatalf("restarted card weights id = %q, want %q (the file /props names now)", got, want)
	}
}

// TestOllamaRepullNotStaleOnCacheHit: the probe cache is keyed by backend
// and raw tag, which survives `ollama pull` of new weights under the same
// tag. A restart that rides the cache must publish the digest Ollama
// reports now, never the cached one.
func TestOllamaRepullNotStaleOnCacheHit(t *testing.T) {
	home := t.TempDir()
	digest := strings.Repeat("aa", 32)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:8b"}]}`))
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b","size":5000000000,"digest":"` + digest + `"}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"}}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	start := func() ServedModel {
		t.Helper()
		rt, err := Prepare(t.Context(), Options{
			Home: home, NodeID: testNodeID, SkipBench: true,
			Backends: []BackendSpec{{ID: "ollama", Kind: "openai-compatible", URL: srv.URL}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		return rt.Models[0]
	}
	if got := start(); got.WeightsID != "ollama:sha256:"+digest {
		t.Fatalf("first start: weights id %q", got.WeightsID)
	}
	digest = strings.Repeat("bb", 32) // `ollama pull` landed new weights under the same tag
	got := start()
	if got.WeightsID != "ollama:sha256:"+digest || got.WeightsFingerprint != digest {
		t.Fatalf("cache hit replayed a stale identity: %q / %q", got.WeightsID, got.WeightsFingerprint)
	}
}

// TestCacheHitRereadsListingFacts: a probe-cache entry written before the
// agent captured TensorFold's reasoning object (v0.1.3) has no thinking
// and no efforts; a v0.1.5 restart re-reads them from the listing it
// fetches anyway — no --reprobe — and refreshes the entry.
func TestCacheHitRereadsListingFacts(t *testing.T) {
	const raw = "tf-qwen38-flash"
	home := t.TempDir()
	url := tensorFoldFake(t, raw).URL
	start := func() ServedModel {
		t.Helper()
		rt, err := Prepare(t.Context(), Options{
			Home: home, NodeID: testNodeID, SkipBench: true,
			Backends: []BackendSpec{{ID: "tf", Kind: "openai-compatible", URL: url}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		return rt.Models[0]
	}
	start() // writes the cache entry

	// Age the entry into what v0.1.3 wrote: no capabilities, no efforts.
	path := filepath.Join(home, "probe-cache.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Entries map[string]CachedModel `json:"entries"`
	}
	if err := json.Unmarshal(b, &f); err != nil || len(f.Entries) != 1 {
		t.Fatalf("cache file: %v (%d entries)", err, len(f.Entries))
	}
	for k, e := range f.Entries {
		e.Capabilities, e.Efforts = nil, nil
		f.Entries[k] = e
	}
	if b, err = json.Marshal(f); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	card := start()
	if !card.Reasoning || strings.Join(card.ReasoningEfforts, ",") != "xhigh,medium,low" {
		t.Fatalf("cache hit lost the listing's facts: reasoning=%v efforts=%v", card.Reasoning, card.ReasoningEfforts)
	}
	// The refreshed entry carries them for the next start too.
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), `"thinking"`) || !strings.Contains(string(b), `"xhigh"`) {
		t.Fatalf("cache entry not refreshed: %s", b)
	}
}
