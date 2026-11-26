package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The generation-prompt tails of two real Ollama templates (MIJ,
// 2026-11-26): qwen3:4b is the 2507 thinking edition, qwen3:1.7b the
// hybrid that honours think:false.
const (
	tmplThinkingOnly = `{{- if and (ne .Role "assistant") $last }}<|im_start|>assistant
<think>
{{ end }}
{{- end }}`
	tmplHybrid = `{{- if and (ne .Role "assistant") $last }}<|im_start|>assistant
{{ if and $.IsThinkSet (not $.Think) -}}
<think>

</think>

{{ end -}}
{{ end }}
{{- end }}`
)

func TestAlwaysThinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		tmpl string
		want bool
	}{
		{"thinking-only template", []string{"completion", "thinking"}, tmplThinkingOnly, true},
		{"hybrid template reads $.Think", []string{"completion", "thinking"}, tmplHybrid, false},
		{"no thinking capability", []string{"completion"}, tmplThinkingOnly, false},
		{"IsThinkSet alone is not the switch", []string{"thinking"}, `{{ if $.IsThinkSet }}x{{ end }}<think>`, true},
		{"a level is not an off switch", []string{"thinking"}, `{{ .ThinkLevel }}<think>`, true},
		{"no template known", []string{"thinking"}, "", false},
		// qwen3.5:4b on MIJ: a 13-character stub, rendered by Ollama's own
		// code — which honours think:false (measured: "391", no thinking).
		{"stub template proves nothing", []string{"completion", "thinking"}, "{{ .Prompt }}", false},
	} {
		if got := alwaysThinks(tc.caps, tc.tmpl); got != tc.want {
			t.Errorf("%s: alwaysThinks = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAlwaysThinkingModelNeverAskedOff: once /api/show has shown a
// thinking-only template, "off" is never sent — not reasoning_effort
// "none", not enable_thinking:false — the lowest level goes instead, so
// the model's thinking stays out of the answer text.
func TestAlwaysThinkingModelNeverAskedOff(t *testing.T) {
	var mu sync.Mutex
	var seen []offRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []string{"completion", "tools", "thinking"},
			"template":     tmplThinkingOnly,
		})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req offRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := capsClient(t, srv.URL)
	if _, err := c.(*openaiClient).ProbeMeta(context.Background(), "m"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got, err := streamEffort(t, c, "off"); err != nil || got != "ok" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	r := seen[0]
	if len(r.ChatTemplateKwargs) != 0 {
		t.Fatalf("enable_thinking sent to an always-thinking model: %s", r.ChatTemplateKwargs)
	}
	if r.ReasoningEffort == nil || *r.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %v, want low (the nearest level; never none)", r.ReasoningEffort)
	}
}
