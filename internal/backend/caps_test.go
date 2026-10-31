package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// capsServer stands in for the openai-dialect servers the probes target.
// apiShowStatus codes /api/show (404 = vLLM/llama.cpp/ds4, 400 = Ollama);
// chat behaves per the flags: 200s tool-bearing requests, 400s image
// parts, unless overridden. reasoningContent fills the parsed-reasoning
// field of plain answers; rawThink makes the content open with <think>
// instead (the parser-less shape).
type capsServer struct {
	apiShowStatus    int
	chatStatus       int // -1 = shape-dependent (200 tools / 400 image)
	chatProbes       atomic.Int64
	reasoningContent string
	rawThink         bool
}

func (s *capsServer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(s.apiShowStatus)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		s.chatProbes.Add(1)
		if s.chatStatus >= 0 {
			if s.chatStatus != http.StatusOK {
				w.WriteHeader(s.chatStatus)
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "image_url") {
			w.WriteHeader(http.StatusBadRequest) // text-only model
			return
		}
		content := "ok"
		if s.rawThink {
			content = "<think>17*23...</think>\n\n391"
		}
		msg := map[string]any{"content": content}
		if s.reasoningContent != "" {
			msg["reasoning_content"] = s.reasoningContent
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": msg}},
		})
	})
	return httptest.NewServer(mux)
}

func capsClient(t *testing.T, url string) Client {
	t.Helper()
	c, err := New(Config{Name: "t", Kind: KindOpenAICompatible, BaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDetectCapabilitiesVLLMShape(t *testing.T) {
	s := &capsServer{apiShowStatus: 404, chatStatus: -1}
	c := capsClient(t, s.serve(t).URL)

	prober, ok := c.(CapabilityProber)
	if !ok {
		t.Fatal("openai client does not implement CapabilityProber")
	}
	d := prober.DetectCapabilities(context.Background(), "qwen38-27b")
	if !d.Tools || d.Vision || d.Thinking {
		t.Fatalf("detected = %+v, want {tools:true vision:false thinking:false}", d)
	}
	// Resolved results are cached: a second ask must not re-probe.
	prober.DetectCapabilities(context.Background(), "qwen38-27b")
	if n := s.chatProbes.Load(); n != 3 {
		t.Fatalf("chat probes = %d, want 3 (one per question, cached after)", n)
	}
}

// A reasoning model behind a reasoning parser: the plain generation comes
// back with reasoning_content — thinking detected.
func TestDetectCapabilitiesThinkingParsed(t *testing.T) {
	s := &capsServer{apiShowStatus: 404, chatStatus: -1, reasoningContent: "17*23 = 17*20 + 17*3 = 391"}
	c := capsClient(t, s.serve(t).URL)

	d := c.(CapabilityProber).DetectCapabilities(context.Background(), "m")
	if !d.Thinking || !d.Tools {
		t.Fatalf("detected = %+v, want thinking+tools true", d)
	}
}

// Parser-less thinking model: the raw <think> markup opens the content.
func TestDetectCapabilitiesThinkingRawThink(t *testing.T) {
	s := &capsServer{apiShowStatus: 404, chatStatus: -1, rawThink: true}
	c := capsClient(t, s.serve(t).URL)

	d := c.(CapabilityProber).DetectCapabilities(context.Background(), "m")
	if !d.Thinking {
		t.Fatalf("detected = %+v, want thinking true from <think> prefix", d)
	}
}

func TestDetectCapabilitiesTextOnly(t *testing.T) {
	s := &capsServer{apiShowStatus: 404, chatStatus: http.StatusBadRequest}
	c := capsClient(t, s.serve(t).URL)

	d := c.(CapabilityProber).DetectCapabilities(context.Background(), "m")
	if d.Tools || d.Vision {
		t.Fatalf("detected = %+v, want all false (4xx is evidence of absence)", d)
	}
}

func TestDetectCapabilitiesVisionModel(t *testing.T) {
	s := &capsServer{apiShowStatus: 404, chatStatus: http.StatusOK}
	c := capsClient(t, s.serve(t).URL)

	d := c.(CapabilityProber).DetectCapabilities(context.Background(), "vlm")
	if !d.Tools || !d.Vision {
		t.Fatalf("detected = %+v, want both true", d)
	}
}

// A transient answer (5xx) must not be cached as fact — the next call
// re-probes and picks up the recovered server.
func TestDetectCapabilitiesTransientNotCached(t *testing.T) {
	s := &capsServer{apiShowStatus: 404, chatStatus: http.StatusServiceUnavailable}
	srv := s.serve(t)
	c := capsClient(t, srv.URL)

	if d := c.(CapabilityProber).DetectCapabilities(context.Background(), "m"); d.Tools || d.Vision {
		t.Fatalf("5xx read as fact: %+v", d)
	}
	s.chatStatus = -1 // server recovered
	if d := c.(CapabilityProber).DetectCapabilities(context.Background(), "m"); !d.Tools {
		t.Fatalf("recovered server not detected: %+v", d)
	}
}

// Ollama-dialect servers never get probed: /api/show is authoritative and
// a probe would cold-load the model just to ask.
func TestDetectCapabilitiesSkipsOllamaDialect(t *testing.T) {
	s := &capsServer{apiShowStatus: http.StatusBadRequest, chatStatus: http.StatusOK} // chat would say yes to everything
	srv := s.serve(t)
	c := capsClient(t, srv.URL)

	d := c.(CapabilityProber).DetectCapabilities(context.Background(), "m")
	if d.Tools || d.Vision {
		t.Fatalf("ollama dialect probed anyway: %+v", d)
	}
	if n := s.chatProbes.Load(); n != 0 {
		t.Fatalf("chat probed %d times on an ollama-dialect server", n)
	}
}

// The probe request wire shapes: tool spec marshals with the nested
// function object; the vision part is the OpenAI content-part array.
func TestProbeRequestShapes(t *testing.T) {
	var gotTools, gotImage bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			ToolChoice string `json:"tool_choice"`
			MaxTokens  int    `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) == 1 && req.Tools[0].Type == "function" && req.Tools[0].Function.Name != "" &&
			req.ToolChoice == "auto" && req.MaxTokens == 1 {
			gotTools = true
		}
		if strings.Contains(string(req.Messages[0].Content), "image_url") {
			gotImage = true
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	c := capsClient(t, httptest.NewServer(mux).URL)
	c.(CapabilityProber).DetectCapabilities(context.Background(), "m")
	if !gotTools || !gotImage {
		t.Fatalf("probe shapes: tools-ok=%v image-ok=%v", gotTools, gotImage)
	}
}

// TestProbeEfforts: the effort vocabulary is the backend's own answer, not
// a guess — a vLLM qwen3.8 build accepts xhigh/medium/low and REJECTS
// "high" and "off" outright, which no static list encodes.
func TestProbeEfforts(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ReasoningEffort string `json:"reasoning_effort"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen[req.ReasoningEffort] = true
		mu.Unlock()
		switch req.ReasoningEffort {
		case "low", "medium", "xhigh":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unexpected reasoning effort ` + req.ReasoningEffort + `"}}`))
		}
	})
	c := capsClient(t, httptest.NewServer(mux).URL)
	got := c.(*openaiClient).probeEfforts(context.Background(), "m")
	want := []string{"low", "medium", "xhigh"}
	if len(got) != len(want) {
		t.Fatalf("efforts = %v, want %v", got, want)
	}
	for i, e := range want {
		if got[i] != e {
			t.Fatalf("efforts = %v, want %v", got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen["off"] || !seen["high"] {
		t.Fatalf("every candidate must be probed, saw %v", seen)
	}
}
