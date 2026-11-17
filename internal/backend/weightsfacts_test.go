package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTensorFoldListingEfforts: TensorFold's /v1/models (seen live 16 Nov)
// states thinking support per model — a reasoning object with the effort
// list. The listing branch of ProbeMeta must hand both to the card:
// thinking in capabilities, efforts with "none" dropped (off is
// enable_thinking:false, not an effort) and the server's order kept.
func TestTensorFoldListingEfforts(t *testing.T) {
	mux := http.NewServeMux()
	// Exactly the JSON the plan quotes, on the real TensorFold model id.
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen38-flash-next-fp4","max_model_len":131072,
			"reasoning":{"default_enabled":false,"supported_efforts":["xhigh","medium","low","none"]}}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // not Ollama: the listing is the only source
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{Name: "tf", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	meta, err := c.(*openaiClient).ProbeMeta(context.Background(), "qwen38-flash-next-fp4")
	if err != nil {
		t.Fatalf("ProbeMeta: %v", err)
	}
	if meta.Ctx != 131072 {
		t.Fatalf("ctx = %d, want 131072 from max_model_len", meta.Ctx)
	}
	var thinking bool
	for _, cap := range meta.Capabilities {
		if cap == "thinking" {
			thinking = true
		}
	}
	if !thinking {
		t.Fatalf("capabilities = %v, want thinking", meta.Capabilities)
	}
	if got, want := strings.Join(meta.Efforts, ","), "xhigh,medium,low"; got != want {
		t.Fatalf("efforts = %q, want %q (none dropped, order kept)", got, want)
	}
}

// TestWeightsFactsOllama: /api/tags carries the manifest digest — the
// weights identity for ollama-served cards — fetched once alongside the
// sizes ProbeMeta already uses.
func TestWeightsFactsOllama(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"family":"qwen3","parameter_size":"4B"}`))
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:4b","size":2500000000,"digest":"` + digest + `"}]}`))
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{Name: "ollama", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	wp, ok := c.(WeightsProvenance)
	if !ok {
		t.Fatal("openai kinds must implement WeightsProvenance")
	}
	meta, err := c.(Prober).ProbeMeta(context.Background(), "qwen3:4b")
	if err != nil || meta.SizeBytes != 2500000000 {
		t.Fatalf("ProbeMeta = %+v %v", meta, err)
	}
	d, root, path := wp.WeightsFacts(context.Background(), "qwen3:4b")
	if d != digest || root != "" || path != "" {
		t.Fatalf("WeightsFacts = %q %q %q, want digest only", d, root, path)
	}
}

// TestAnthropicNoWeightsProvenance: the anthropic kind names no files —
// no provenance, so its cards carry no weights id from tiers 2–4.
func TestAnthropicNoWeightsProvenance(t *testing.T) {
	c, err := New(Config{Name: "a", Kind: KindAnthropic, BaseURL: "http://127.0.0.1:9", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(WeightsProvenance); ok {
		t.Fatal("anthropic kind must not implement WeightsProvenance")
	}
}

// TestWeightsFactsLlamaProps: llama.cpp /props names the GGUF it loaded
// (model_path) — the weights identity for llama-server cards, captured
// alongside the n_ctx ServingCtxs already reads.
func TestWeightsFactsLlamaProps(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192},
			"model_path":"/models/qwen3-8b-q4_k_m.gguf"}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{Name: "llama", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	wp := c.(WeightsProvenance)
	d, root, path := wp.WeightsFacts(context.Background(), "qwen3-8b")
	if d != "" || root != "" || path != "/models/qwen3-8b-q4_k_m.gguf" {
		t.Fatalf("WeightsFacts = %q %q %q, want model_path only", d, root, path)
	}
}

// TestWeightsFactsVLLMRoot: vLLM's listing root names the local snapshot
// dir — captured per model id, container prefix and all.
func TestWeightsFactsVLLMRoot(t *testing.T) {
	root := "/root/.cache/huggingface/hub/models--nvidia--Qwen3.8-Flash-Next-FP4-Local/snapshots/b9ce18a6887e18c089fdea8509a33e63429a3300"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen38-flash-next-fp4","root":"` + root + `","max_model_len":131072}]}`))
	})
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{Name: "vllm", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, gotRoot, path := c.(WeightsProvenance).WeightsFacts(context.Background(), "qwen38-flash-next-fp4")
	if d != "" || gotRoot != root || path != "" {
		t.Fatalf("WeightsFacts = %q %q %q", d, gotRoot, path)
	}
}

// TestWeightsFactsMultiModelNoPath: /props describes ONE loaded model; a
// backend that lists several (llama-server router mode, llama-swap) must
// not stamp that file on every card.
func TestWeightsFactsMultiModelNoPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	})
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192},"model_path":"/models/a.gguf"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, err := New(Config{Name: "swap", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.(interface {
		ProbeModels(context.Context) ([]string, error)
	}).ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, path := c.(WeightsProvenance).WeightsFacts(context.Background(), "b"); path != "" {
		t.Fatalf("path = %q, want none on a multi-model backend", path)
	}
}

// TestTensorFoldNoneOnlyIsNotThinking: a reasoning object whose only
// effort is "none" and that is off by default declares a non-thinking
// model — no thinking capability, no efforts.
func TestTensorFoldNoneOnlyIsNotThinking(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","max_model_len":8192,
			"reasoning":{"default_enabled":false,"supported_efforts":["none"]}}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, err := New(Config{Name: "tf", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := c.(Prober).ProbeMeta(context.Background(), "m")
	for _, cp := range meta.Capabilities {
		if cp == "thinking" {
			t.Fatalf("capabilities = %v, want no thinking", meta.Capabilities)
		}
	}
	if len(meta.Efforts) != 0 {
		t.Fatalf("efforts = %v, want none", meta.Efforts)
	}
}
