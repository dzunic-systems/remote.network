package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func newTestClient(t *testing.T, url string) *openaiClient {
	t.Helper()
	c, err := New(Config{Kind: KindOpenAICompatible, BaseURL: url + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*openaiClient)
}

// vLLM states the window it enforces per model in /v1/models.
func TestServingCtxsVLLMMaxModelLen(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"chat","max_model_len":131072},{"id":"e5","max_model_len":512}]}`))
	})
	srv := httptest.NewServer(mux) // /props 404s: not llama.cpp
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := c.ServingCtxs()
	if got["chat"] != 131072 || got["e5"] != 512 {
		t.Fatalf("ServingCtxs = %v", got)
	}
}

// llama.cpp: one server-wide n_ctx from /props, retried while loading (503).
func TestServingCtxsLlamaCppProps(t *testing.T) {
	var propsCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma.gguf"}]}`))
	})
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, _ *http.Request) {
		if propsCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // still loading
			return
		}
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":16384}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.ServingCtxs(); len(got) != 0 {
		t.Fatalf("while loading: ServingCtxs = %v, want empty", got)
	}
	if got := c.ServingCtxs(); got["gemma.gguf"] != 16384 {
		t.Fatalf("after load: ServingCtxs = %v", got)
	}
	c.ServingCtxs()
	if n := propsCalls.Load(); n != 2 {
		t.Fatalf("/props called %d times, want 2 (cached once answered)", n)
	}
}

// ds4-server lists the allocated --ctx as OpenRouter-style context_length
// and names "tools" in supported_parameters; it has no /api/show.
func TestDS4ListingCtxAndTools(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4-flash","object":"model","owned_by":"ds4.c",
			"name":"DeepSeek V4 Flash","context_length":100000,
			"top_provider":{"context_length":100000,"max_completion_tokens":4096,"is_moderated":false},
			"supported_parameters":["tools","tool_choice","max_tokens","stream","reasoning_effort"]}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.ServingCtxs()["deepseek-v4-flash"]; got != 100000 {
		t.Fatalf("serving ctx = %d, want 100000", got)
	}
	meta, err := c.ProbeMeta(context.Background(), "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Ctx != 100000 || len(meta.Capabilities) != 2 || meta.Capabilities[1] != "tools" {
		t.Fatalf("meta = %+v", meta)
	}
	// A model the listing said nothing about still errors → heuristics.
	if _, err := c.ProbeMeta(context.Background(), "unknown"); err == nil {
		t.Fatal("ProbeMeta(unknown) succeeded with no facts")
	}
}
