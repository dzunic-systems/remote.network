package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"remote.network/node/wire"
)

// fakeOpenAI serves /v1/models and a canned SSE chat stream.
func fakeOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3:14b"},{"id":"gpt-oss:20b"}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, chunk := range []string{
			`{"choices":[{"delta":{"reasoning_content":"think"}}]}`,
			`{"choices":[{"delta":{"content":"hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		} {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			f.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	})
	return httptest.NewServer(mux)
}

// TestOpenAIDemotesMidArraySystem: vLLM-style templates demand system at
// index 0 and 400 the rest ("System message must be at the beginning") —
// claude-cli rides context as a mid-array system message. Every later
// system turn ships upstream as user.
func TestOpenAIDemotesMidArraySystem(t *testing.T) {
	var got struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{Name: "test", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	err = c.StreamChat(context.Background(), ChatRequest{
		Model: "m",
		Messages: []ChatMessage{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "# Environment: /tmp"},
		},
	}, func(Delta) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"system", "user", "user"}
	if len(got.Messages) != len(want) {
		t.Fatalf("messages = %+v", got.Messages)
	}
	for i, m := range got.Messages {
		if m.Role != want[i] {
			t.Fatalf("messages[%d].role = %q, want %q (full: %+v)", i, m.Role, want[i], got.Messages)
		}
	}
}

func TestOpenAIProbeAndStream(t *testing.T) {
	srv := fakeOpenAI(t)
	c, err := New(Config{Name: "test", Kind: KindOpenAICompatible, BaseURL: srv.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	models, err := c.ProbeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "qwen3:14b" {
		t.Fatalf("models = %v", models)
	}

	var sb strings.Builder
	var deltas int
	var usage *Usage
	if err := c.StreamChat(context.Background(), ChatRequest{
		Model:     "qwen3:14b",
		Messages:  []ChatMessage{{Role: "user", Content: "hi"}},
		MaxTokens: 8,
	}, func(d Delta) error {
		deltas++
		sb.WriteString(d.Content)
		if d.Usage != nil {
			usage = d.Usage
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sb.String() != "hello" {
		t.Fatalf("streamed %q, want hello", sb.String())
	}
	if deltas != 4 { // reasoning_content chunk + 2 content + finish/usage
		t.Fatalf("deltas = %d, want 4", deltas)
	}
	if usage == nil || usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestLoopbackNeedsNoKey(t *testing.T) {
	if _, err := New(Config{Kind: KindOpenAICompatible, BaseURL: "http://localhost:11434/v1"}); err != nil {
		t.Fatalf("loopback without key rejected: %v", err)
	}
	if _, err := New(Config{Kind: KindOpenAI, BaseURL: "https://api.openai.com/v1"}); err == nil {
		t.Fatal("non-loopback without key accepted")
	}
}

func TestParseKind(t *testing.T) {
	for in, want := range map[string]Kind{
		"openai": KindOpenAI, "ollama": KindOpenAICompatible,
		"OpenAI-Compatible": KindOpenAICompatible, "anthropic": KindAnthropic,
	} {
		got, err := ParseKind(in)
		if err != nil || got != want {
			t.Fatalf("ParseKind(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseKind("google"); err == nil {
		t.Fatal("google kind accepted (not a supported kind)")
	}
}

// fakeOllama adds the /api/show + /api/tags + /v1/embeddings surfaces to
// the OpenAI fake — the card-collection endpoints.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		var req oaShowRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Model {
		case "qwen3:14b":
			_, _ = w.Write([]byte(`{
				"family":"qwen3","parameter_size":"14.8B","quantization_level":"Q4_K_M",
				"capabilities":["completion","tools"],
				"details":{"families":["qwen3"]},
				"model_info":{"general.parameter_count":14800000000,"qwen3.context_length":40960}}`))
		case "nomic-embed-text:latest":
			_, _ = w.Write([]byte(`{
				"family":"nomic-bert","parameter_size":"0.2B","quantization_level":"F16",
				"capabilities":["embedding"],
				"model_info":{"nomic-bert.context_length":8192}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"name":"qwen3:14b","size":9000000000},
			{"name":"nomic-embed-text:latest","size":500000000}]}`))
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req oaEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Deliberately out of order: index 1 first — Embed must reorder.
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[3,4]},
			{"index":0,"embedding":[1,2]}],
			"usage":{"prompt_tokens":7}}`))
		_ = req
	})
	return httptest.NewServer(mux)
}

func TestProbeMetaOllama(t *testing.T) {
	c, err := New(Config{Name: "t", Kind: KindOpenAICompatible, BaseURL: fakeOllama(t).URL})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.(Prober)
	if !ok {
		t.Fatal("openaiClient does not implement Prober")
	}

	meta, err := p.ProbeMeta(context.Background(), "qwen3:14b")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Family != "qwen3" || meta.ParamsB != 14.8 || meta.Quant != "Q4_K_M" ||
		meta.Ctx != 40960 || meta.SizeBytes != 9000000000 {
		t.Fatalf("meta = %+v", meta)
	}
	if ModalityOf("qwen3:14b", meta) != "chat" {
		t.Fatal("qwen3 misclassified")
	}

	embedMeta, err := p.ProbeMeta(context.Background(), "nomic-embed-text:latest")
	if err != nil {
		t.Fatal(err)
	}
	if ModalityOf("nomic-embed-text:latest", embedMeta) != "embedding" {
		t.Fatal("declared-capability embedding not detected")
	}

	// Unknown model → error → heuristic fallback path.
	if _, err := p.ProbeMeta(context.Background(), "deepseek-r1:7b"); err == nil {
		t.Fatal("missing /api/show should error")
	}
}

func TestEmbedReordersAndReportsUsage(t *testing.T) {
	c, err := New(Config{Name: "t", Kind: KindOpenAICompatible, BaseURL: fakeOllama(t).URL})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := c.(Embedder)
	if !ok {
		t.Fatal("openaiClient does not implement Embedder")
	}
	vecs, usage, err := e.Embed(context.Background(), "nomic-embed-text:latest", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[0][1] != 2 || vecs[1][0] != 3 {
		t.Fatalf("vectors not index-ordered: %v", vecs)
	}
	if usage.InputTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestModalityHeuristic(t *testing.T) {
	// No capabilities: the raw-id heuristic decides.
	for raw, want := range map[string]string{
		"qwen3-embedding:4b":        "embedding",
		"nomic-embed-text:latest":   "embedding",
		"bge-large-en-v1.5":         "embedding",
		"mxbai-embed-large:latest":  "embedding",
		"deepseek-r1:7b":            "chat",
		"gemma4:26b-a4b-it-qat":     "chat",
		"Qwen/Qwen3-4B-GGUF:Q4_K_M": "chat",
	} {
		if got := ModalityOf(raw, Meta{}); got != want {
			t.Fatalf("ModalityOf(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestHeuristicMetaAndRawTag(t *testing.T) {
	m := HeuristicMeta("qwen3:14b-q4_K_M")
	if m.Family != "qwen3" || m.ParamsB != 14 || m.Quant != "Q4_K_M" {
		t.Fatalf("heuristic meta = %+v", m)
	}
	m = HeuristicMeta("deepseek-r1:7b")
	if m.Family != "deepseek" || m.ParamsB != 7 || m.Quant != "" {
		t.Fatalf("deepseek meta = %+v", m)
	}
	m = HeuristicMeta("Qwen/Qwen3-4B-GGUF:Q4_K_M")
	if m.Family != "qwen3" || m.ParamsB != 4 || m.Quant != "Q4_K_M" {
		t.Fatalf("hf-style meta = %+v", m)
	}
	m = HeuristicMeta("muse-glimmer:30b-nvfp4")
	if m.Family != "muse" || m.ParamsB != 30 {
		t.Fatalf("muse meta = %+v", m)
	}

	if got := RawTag("qllama/multilingual-e5-large-instruct:latest"); got != "multilingual-e5-large-instruct:latest" {
		t.Fatalf("RawTag = %q", got)
	}
	if got := RawTag("weird model/name!.gguf"); got != "name" { // "!" → "-", trailing dashes trimmed
		t.Fatalf("RawTag sanitize = %q", got)
	}
	long := RawTag(strings.Repeat("x", 120))
	if len(long) != 80 {
		t.Fatalf("RawTag length = %d", len(long))
	}
}

func TestParseParamSize(t *testing.T) {
	for in, want := range map[string]float64{
		"26.3B": 26.3, "7b": 7, "0.6B": 0.6, "latest": 0, "": 0, "xB": 0,
	} {
		if got := parseParamSize(in); got != want {
			t.Fatalf("parseParamSize(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestOpenAICarriesStructuredOutputAndKnobs: the consumer knobs
// ride the upstream request in the OpenAI shape — response_format verbatim,
// temperature and priority when set — and stay absent when unset.
func TestOpenAICarriesStructuredOutputAndKnobs(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	c, err := New(Config{Name: "test", Kind: KindOpenAICompatible, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oc := c.(*openaiClient)
	req := ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		ResponseFormat: &wire.ResponseFormat{Type: "json_schema", JSONSchema: &wire.JSONSchemaSpec{
			Name: "a", Strict: true, Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Temperature: 0.2,
		Priority:    10,
	}
	if err := oc.StreamChat(context.Background(), req, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rf, _ := got["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v", got["response_format"])
	}
	js, _ := rf["json_schema"].(map[string]any)
	if js == nil || js["strict"] != true || js["schema"] == nil {
		t.Fatalf("json_schema = %v", rf)
	}
	if got["temperature"] != 0.2 || got["priority"] != float64(10) {
		t.Fatalf("knobs = %v %v", got["temperature"], got["priority"])
	}

	// Unset knobs stay absent — a backend that 400s an empty field would
	// otherwise break every plain request.
	got = nil
	if err := oc.StreamChat(context.Background(), ChatRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"temperature", "priority", "response_format"} {
		if _, present := got[k]; present {
			t.Fatalf("unset %s leaked upstream", k)
		}
	}
}
