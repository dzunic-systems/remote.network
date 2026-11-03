package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"remote.network/node/wire"
)

// offRequest is what the fake server recorded about one chat request.
type offRequest struct {
	ReasoningEffort    *string         `json:"reasoning_effort"`
	ChatTemplateKwargs json.RawMessage `json:"chat_template_kwargs"`
}

// offServer records each request and answers with respond's status; 200
// streams a one-word reply.
func offServer(t *testing.T, respond func(offRequest) int) (Client, func() []offRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []offRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req offRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		status := respond(req)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"refused"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return capsClient(t, srv.URL), func() []offRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]offRequest(nil), seen...)
	}
}

func streamEffort(t *testing.T, c Client, effort string) (string, error) {
	t.Helper()
	var content string
	err := c.StreamChat(context.Background(), ChatRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Reasoning: &wire.ReasoningControl{Effort: effort},
	}, func(d Delta) error { content += d.Content; return nil })
	return content, err
}

// TestStreamChatEffortOffDisablesThinking: "off" asks both ways at once, and
// never with "minimal" or "off", which vLLM refuses.
func TestStreamChatEffortOffDisablesThinking(t *testing.T) {
	c, seen := offServer(t, func(offRequest) int { return http.StatusOK })
	if got, err := streamEffort(t, c, "off"); err != nil || got != "ok" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0].ReasoningEffort == nil || *reqs[0].ReasoningEffort != "none" {
		t.Fatalf("reasoning_effort = %v, want none", reqs[0].ReasoningEffort)
	}
	if string(reqs[0].ChatTemplateKwargs) != `{"enable_thinking":false}` {
		t.Fatalf("chat_template_kwargs = %s", reqs[0].ChatTemplateKwargs)
	}
}

// TestStreamChatEffortKeepsThinking: any other effort leaves the template
// switch alone.
func TestStreamChatEffortKeepsThinking(t *testing.T) {
	c, seen := offServer(t, func(offRequest) int { return http.StatusOK })
	if _, err := streamEffort(t, c, "low"); err != nil {
		t.Fatal(err)
	}
	r := seen()[0]
	if len(r.ChatTemplateKwargs) != 0 {
		t.Fatalf("chat_template_kwargs = %s, want absent", r.ChatTemplateKwargs)
	}
	if r.ReasoningEffort == nil || *r.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %v, want low", r.ReasoningEffort)
	}
}

// TestStreamChatKwargsRefusedFallsBack: a server that refuses the template
// switch still gets "none", and is not asked again.
func TestStreamChatKwargsRefusedFallsBack(t *testing.T) {
	c, seen := offServer(t, func(r offRequest) int {
		if len(r.ChatTemplateKwargs) > 0 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	for i := 0; i < 2; i++ {
		if got, err := streamEffort(t, c, "off"); err != nil || got != "ok" {
			t.Fatalf("call %d: content = %q, err = %v", i, got, err)
		}
	}
	reqs := seen()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4 (both, kwargs, none, then straight to none)", len(reqs))
	}
	if len(reqs[3].ChatTemplateKwargs) != 0 || reqs[3].ReasoningEffort == nil || *reqs[3].ReasoningEffort != "none" {
		t.Fatalf("last request = %+v, want none alone", reqs[3])
	}
}

// TestStreamChatOtherBadRequestNotRemembered: a request that is bad for
// another reason fails all the way down, reports the error, and does not cost
// the model its thinking switch.
func TestStreamChatOtherBadRequestNotRemembered(t *testing.T) {
	fail := true
	var mu sync.Mutex
	c, seen := offServer(t, func(offRequest) int {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	if _, err := streamEffort(t, c, "off"); err == nil {
		t.Fatal("want the 400 reported")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := streamEffort(t, c, "off"); err != nil {
		t.Fatal(err)
	}
	reqs := seen()
	if len(reqs) != 6 || len(reqs[5].ChatTemplateKwargs) == 0 {
		t.Fatalf("requests = %+v, want the sixth to still carry the switch", reqs)
	}
}

// outcomeOf runs one request and returns the reported outcome and the efforts
// the server saw.
func outcomeOf(t *testing.T, c Client, effort string) *wire.ReasoningOutcome {
	t.Helper()
	var out *wire.ReasoningOutcome
	err := c.StreamChat(context.Background(), ChatRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Reasoning: &wire.ReasoningControl{Effort: effort},
	}, func(d Delta) error {
		if d.ReasoningOutcome != nil {
			out = d.ReasoningOutcome
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStreamChatHighFallsToNearest is a vLLM build where "high" 400s while
// "xhigh" runs, so the request runs at xhigh, says so, and remembers it.
func TestStreamChatHighFallsToNearest(t *testing.T) {
	c, seen := offServer(t, func(r offRequest) int {
		if r.ReasoningEffort != nil && *r.ReasoningEffort == "high" {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	for i := 0; i < 2; i++ {
		o := outcomeOf(t, c, "high")
		if o == nil || o.Requested != "high" || o.Applied != "xhigh" {
			t.Fatalf("call %d: outcome = %+v", i, o)
		}
	}
	if n := len(seen()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
}

// TestStreamChatAllRefusedRunsDefault: a server refusing every level still
// answers, reported as its default.
func TestStreamChatAllRefusedRunsDefault(t *testing.T) {
	c, _ := offServer(t, func(r offRequest) int {
		if r.ReasoningEffort != nil {
			return http.StatusUnprocessableEntity
		}
		return http.StatusOK
	})
	if o := outcomeOf(t, c, "medium"); o == nil || o.Applied != "default" {
		t.Fatalf("outcome = %+v", o)
	}
}

// TestStreamChatMetControlNotReported: nothing to report when it was met.
func TestStreamChatMetControlNotReported(t *testing.T) {
	c, _ := offServer(t, func(offRequest) int { return http.StatusOK })
	for _, effort := range []string{"off", "low"} {
		if o := outcomeOf(t, c, effort); o != nil {
			t.Fatalf("%s: outcome = %+v, want nil", effort, o)
		}
	}
}

// TestStreamChatOffStillThinkingReported is deepseek-r1 and gpt-oss on Ollama:
// "none" gets a 200 and the model thinks anyway.
func TestStreamChatOffStillThinkingReported(t *testing.T) {
	for name, delta := range map[string]string{
		"parsed":   `{"reasoning":"hmm"}`,
		"unparsed": `{"content":"<think>hmm"}`,
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":` + delta + `}]}` + "\n\ndata: [DONE]\n\n"))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := capsClient(t, srv.URL)
			o := outcomeOf(t, c, "off")
			if o == nil || o.Requested != "off" || o.Applied != "default" {
				t.Fatalf("outcome = %+v", o)
			}
			// Once is not proof, but three in a row is: it gets its lowest level.
			for i := 0; i < offThoughtLimit-1; i++ {
				if o := outcomeOf(t, c, "off"); o == nil || o.Applied != "default" {
					t.Fatalf("repeat %d: outcome = %+v", i, o)
				}
			}
			if o := outcomeOf(t, c, "off"); o == nil || o.Applied != "low" {
				t.Fatalf("after the limit: outcome = %+v", o)
			}
		})
	}
}

// TestStreamChatTemplateWithoutLevels is llama.cpp: it takes any level, but
// /props says the template renders none, so the level changed nothing.
func TestStreamChatTemplateWithoutLevels(t *testing.T) {
	levels := `[]`
	mux := http.NewServeMux()
	mux.HandleFunc("GET /props", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chat_template_caps":{"reasoning_effort_levels":` + levels + `}}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if o := outcomeOf(t, capsClient(t, srv.URL), "low"); o == nil || o.Applied != "default" {
		t.Fatalf("no levels: outcome = %+v", o)
	}
	if o := outcomeOf(t, capsClient(t, srv.URL), "off"); o != nil {
		t.Fatalf("off works without levels: outcome = %+v", o)
	}
	levels = `["low","high"]`
	if o := outcomeOf(t, capsClient(t, srv.URL), "low"); o != nil {
		t.Fatalf("with levels: outcome = %+v", o)
	}
}

// TestStreamChatLevelNotInModelOptions is LM Studio: gemma4 takes "low" on the
// OpenAI surface, while the listing says only off and on apply.
func TestStreamChatLevelNotInModelOptions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[
			{"key":"m","capabilities":{"reasoning":{"allowed_options":["off","on"],"default":"on"}}},
			{"key":"leveled","capabilities":{"reasoning":{"allowed_options":["low","high"],"default":"low"}}}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := capsClient(t, srv.URL)
	if o := outcomeOf(t, c, "low"); o == nil || o.Applied != "default" {
		t.Fatalf("low: outcome = %+v", o)
	}
	if o := outcomeOf(t, c, "off"); o != nil {
		t.Fatalf("off: outcome = %+v", o)
	}
	var out *wire.ReasoningOutcome
	if err := c.StreamChat(context.Background(), ChatRequest{
		Model: "leveled", Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Reasoning: &wire.ReasoningControl{Effort: "low"},
	}, func(d Delta) error {
		if d.ReasoningOutcome != nil {
			out = d.ReasoningOutcome
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("listed level: outcome = %+v", out)
	}
}

// optionsServer is LM Studio's listing for one model, recording the efforts sent.
func optionsServer(t *testing.T, listing string) (Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var efforts []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/models", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(listing)) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req offRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		if req.ReasoningEffort != nil {
			efforts = append(efforts, *req.ReasoningEffort)
		} else {
			efforts = append(efforts, "")
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return capsClient(t, srv.URL), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), efforts...)
	}
}

// TestStreamChatLadderFollowsListedOptions is gpt-oss on LM Studio: no off and no
// xhigh, so those ask for its lowest and highest levels outright.
func TestStreamChatLadderFollowsListedOptions(t *testing.T) {
	c, sent := optionsServer(t, `{"models":[{"key":"m","capabilities":{"reasoning":{"allowed_options":["low","medium","high"],"default":"low"}}}]}`)
	if o := outcomeOf(t, c, "xhigh"); o == nil || o.Applied != "high" {
		t.Fatalf("xhigh: outcome = %+v", o)
	}
	if o := outcomeOf(t, c, "off"); o == nil || o.Applied != "low" {
		t.Fatalf("off: outcome = %+v", o)
	}
	got := sent()
	if len(got) != 2 || got[0] != "high" || got[1] != "low" {
		t.Fatalf("efforts sent = %v, want [high low]", got)
	}
}

// TestStreamChatNoListedLevelSendsNothing: a model with only on and off gets no
// level it has no use for.
func TestStreamChatNoListedLevelSendsNothing(t *testing.T) {
	c, sent := optionsServer(t, `{"models":[{"key":"m","capabilities":{"reasoning":{"allowed_options":["off","on"],"default":"on"}}}]}`)
	if o := outcomeOf(t, c, "high"); o == nil || o.Applied != "default" {
		t.Fatalf("outcome = %+v", o)
	}
	if got := sent(); len(got) != 1 || got[0] != "" {
		t.Fatalf("efforts sent = %q, want none", got)
	}
}

// TestStreamChatOffMemorySeparatesTools: thinking through "off" with tools does
// not cost the same model "off" without them.
func TestStreamChatOffMemorySeparatesTools(t *testing.T) {
	mux := http.NewServeMux()
	var mu sync.Mutex
	var efforts []string
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Effort string            `json:"reasoning_effort"`
			Tools  []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		efforts = append(efforts, req.Effort)
		mu.Unlock()
		delta := `{"content":"ok"}`
		if len(req.Tools) > 0 {
			delta = `{"reasoning":"hmm"}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":` + delta + `}]}` + "\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := capsClient(t, srv.URL)
	call := func(tools []ToolSpec) {
		t.Helper()
		if err := c.StreamChat(context.Background(), ChatRequest{
			Model: "m", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, Tools: tools,
			Reasoning: &wire.ReasoningControl{Effort: "off"},
		}, func(Delta) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	tool := []ToolSpec{{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`)}}
	for i := 0; i < offThoughtLimit+1; i++ {
		call(tool)
	}
	call(nil)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(efforts, " "); got != "none none none low none" {
		t.Fatalf("efforts = %q", got)
	}
}

// TestStreamChatStrayThoughtKeepsOff: a clean request starts the count over, so a
// model that only sometimes thinks through "off" keeps it.
func TestStreamChatStrayThoughtKeepsOff(t *testing.T) {
	c := &openaiClient{}
	for i := 0; i < 10; i++ {
		if c.noteOff("k", i%2 == 0) {
			t.Fatalf("request %d: lost off", i)
		}
	}
	for i := 0; i < offThoughtLimit-1; i++ {
		if c.noteOff("k", true) {
			t.Fatalf("thought %d: lost off too early", i)
		}
	}
	if !c.noteOff("k", true) {
		t.Fatal("kept off through the limit")
	}
}

// TestStreamChatRetriesClosedConnection: a connection dropped before any reply
// costs one retry, not the request.
func TestStreamChatRetriesClosedConnection(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if got, err := streamEffort(t, capsClient(t, srv.URL), "off"); err != nil || got != "ok" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
}
