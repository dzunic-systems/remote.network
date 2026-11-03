package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"remote.network/node/wire"
)

// openaiClient speaks the OpenAI wire protocol (openai AND
// openai-compatible kinds — see package comment).
type openaiClient struct {
	cfg Config

	// /api/tags cache for ProbeMeta sizes (probe.go); lazy, once.
	tagsOnce sync.Once
	tags     map[string]int64

	// Per-model serving ctx harvested from the last /v1/models listing —
	// vLLM reports max_model_len (the len it will actually enforce) right
	// in the model objects. Guarded by mu; refilled on every listing.
	mu            sync.Mutex
	modelCtx      map[string]int
	listed        map[string]bool
	listTools     map[string]bool   // supported_parameters names "tools" (ds4-server, OpenRouter shape)
	listReasoning map[string]bool   // …names "reasoning"/"thinking" (OpenRouter vocabulary)
	rungWorked    map[string]string // request shape → reasoning ladder rung to use (reasoning.go)
	offThoughts   map[string]int    // request shape → requests in a row that thought through "off"

	// /props cache (llama.cpp: default_generation_settings.n_ctx), guarded
	// by mu. Retried until it answers (llama-server 503s while loading),
	// then kept — llama-server serves a fixed ctx until restart.
	propsNCtx int
	propsDone bool
	// chat_template_caps.reasoning_effort_levels from the same /props: known
	// when the server reports the list.
	propsLevelsKnown bool
	propsLevels      []string

	// LM Studio's /api/v1/models: per-model reasoning options, guarded by mu.
	nativeOptions map[string][]string
	nativeDone    bool

	// Capability-probe state (caps.go): per-model 1-token probe results,
	// lazily initialized. Its own mutex; allocated on first use.
	caps *probeCaps
}

// ServingCtxs snapshots the ctx the backend itself reports per model
// (vLLM max_model_len, ds4-server context_length, llama.cpp /props n_ctx). Empty for plain Ollama —
// /api/ps is the residency path there.
func (c *openaiClient) ServingCtxs() map[string]int {
	c.mu.Lock()
	done := c.propsDone
	c.mu.Unlock()
	if !done {
		c.loadProps()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.modelCtx))
	for k, v := range c.modelCtx {
		out[k] = v
	}
	// llama.cpp: one server ctx covers whatever /v1/models lists.
	if len(out) == 0 && c.propsNCtx > 0 {
		for id := range c.listed {
			out[id] = c.propsNCtx
		}
	}
	return out
}

// loadProps fetches llama.cpp's /props for the server-wide n_ctx. A 404
// (any non-llama backend) is final; a transport error or 5xx (still
// loading) is retried on the next call.
func (c *openaiClient) loadProps() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/props", nil)
	if err != nil {
		return
	}
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < 500 {
			c.mu.Lock()
			c.propsDone = true // not llama.cpp: never ask again
			c.mu.Unlock()
		}
		return
	}
	var props struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
		NCtx             int `json:"n_ctx"`
		ChatTemplateCaps struct {
			Levels *[]string `json:"reasoning_effort_levels"`
		} `json:"chat_template_caps"`
	}
	if json.NewDecoder(resp.Body).Decode(&props) != nil {
		return
	}
	n := props.DefaultGenerationSettings.NCtx
	if n <= 0 {
		n = props.NCtx
	}
	c.mu.Lock()
	c.propsNCtx = n
	c.propsDone = true
	if l := props.ChatTemplateCaps.Levels; l != nil {
		c.propsLevelsKnown, c.propsLevels = true, *l
	}
	c.mu.Unlock()
}

type oaModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		Root        string `json:"root"`
		MaxModelLen int    `json:"max_model_len"` // vLLM extension: the context len the server enforces
		// OpenRouter-shaped listings (ds4-server): context_length is the
		// allocated --ctx; supported_parameters names "tools" when the
		// server parses tool calls.
		ContextLength int `json:"context_length"`
		TopProvider   struct {
			ContextLength int `json:"context_length"`
		} `json:"top_provider"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

func (c *openaiClient) ProbeModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("probe %s: %s", c.cfg.BaseURL, httpStatus(resp))
	}
	var out oaModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("probe %s: %w", c.cfg.BaseURL, err)
	}
	ids := make([]string, 0, len(out.Data))
	ctxs := make(map[string]int, len(out.Data))
	listed := make(map[string]bool, len(out.Data))
	tools := map[string]bool{}
	reasoning := map[string]bool{}
	for _, m := range out.Data {
		ids = append(ids, m.ID)
		listed[m.ID] = true
		for _, n := range []int{m.MaxModelLen, m.ContextLength, m.TopProvider.ContextLength} {
			if n > 0 {
				ctxs[m.ID] = n
				break
			}
		}
		for _, p := range m.SupportedParameters {
			switch p {
			case "tools":
				tools[m.ID] = true
			case "reasoning", "thinking":
				reasoning[m.ID] = true
			}
		}
	}
	c.mu.Lock()
	c.modelCtx = ctxs
	c.listed = listed
	c.listTools = tools
	c.listReasoning = reasoning
	if len(c.nativeOptions) > 0 {
		c.nativeDone = false // LM Studio: the loaded models may have changed
	}
	c.mu.Unlock()
	return ids, nil
}

type oaStreamRequest struct {
	Model           string        `json:"model"`
	Messages        []oaMessage   `json:"messages"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Stream          bool          `json:"stream"`
	StreamOptions   *oaStreamOpts `json:"stream_options,omitempty"`
	Tools           []oaToolSpec  `json:"tools,omitempty"`
	ToolChoice      any           `json:"tool_choice,omitempty"`      // string or {"type","function"}
	ReasoningEffort string        `json:"reasoning_effort,omitempty"` // none|low|medium|high|xhigh (reasoning.go)
	// ChatTemplateKwargs carries enable_thinking=false for effort "off".
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	// Consumer sampling knobs: passed through when set — backends
	// without the field ignore it, priority-scheduled vLLM orders by it.
	Temperature *float64 `json:"temperature,omitempty"`
	Priority    *int     `json:"priority,omitempty"`
	// Structured output: the OpenAI shape verbatim. vLLM (guided
	// decoding), Ollama and llama.cpp enforce it; a backend that cannot
	// either ignores it or is probed-and-warned by the consumer — carrying
	// the field is always safe, refusing never is.
	ResponseFormat *oaResponseFormat `json:"response_format,omitempty"`
}

// oaResponseFormat is the upstream structured-output shape — the wire's
// ResponseFormat is already OpenAI-shaped, so this is a rename layer.
type oaResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name,omitempty"`
		Strict bool            `json:"strict,omitempty"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema,omitempty"`
}

func oaResponseFormatOf(f *wire.ResponseFormat) *oaResponseFormat {
	if f == nil {
		return nil
	}
	out := &oaResponseFormat{Type: f.Type}
	if f.JSONSchema != nil {
		out.JSONSchema = &struct {
			Name   string          `json:"name,omitempty"`
			Strict bool            `json:"strict,omitempty"`
			Schema json.RawMessage `json:"schema"`
		}{Name: f.JSONSchema.Name, Strict: f.JSONSchema.Strict, Schema: f.JSONSchema.Schema}
	}
	return out
}

// oaToolSpec is the OpenAI tool shape on the upstream wire. The uniform
// wire's ToolSpec is flat (name/description/parameters); marshaling it
// directly made strict OpenAI-compatible backends (Ollama 0.35+) read
// parameters as absent and reject every tool round with "invalid tool
// call arguments" — round one generated fine, round two (history with
// assistant tool_calls) 400'd, which looked exactly like a broken agent.
type oaToolSpec struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// oaTools wraps the uniform wire's flat specs into the upstream shape.
func oaTools(specs []ToolSpec) []oaToolSpec {
	out := make([]oaToolSpec, 0, len(specs))
	for _, t := range specs {
		var w oaToolSpec
		w.Type = "function"
		w.Function.Name = t.Name
		w.Function.Description = t.Description
		w.Function.Parameters = t.Parameters
		out = append(out, w)
	}
	return out
}

// oaMessage is one OpenAI chat turn. Content is a plain string for text
// turns and a part array when images ride along (the OpenAI vision shape).
type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

// oaToolCall is the OpenAI tool_calls entry: id + type + a nested function
// object with the arguments as a JSON string. The uniform wire's flat
// ToolCall (id/name/arguments) marshals without the type and nesting, and
// Ollama 0.35+ reads such an entry as malformed — "invalid tool call
// arguments" on every round that replays assistant tool calls.
type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// oaToolCalls converts the uniform wire's flat calls to the upstream shape.
func oaToolCalls(calls []ToolCall) []oaToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]oaToolCall, 0, len(calls))
	for _, tc := range calls {
		var w oaToolCall
		w.ID = tc.ID
		w.Type = "function"
		w.Function.Name = tc.Name
		w.Function.Arguments = tc.Arguments
		out = append(out, w)
	}
	return out
}

// oaContent builds the content value: text-only turns stay plain strings;
// image turns become the part array every OpenAI-shaped vision endpoint
// (OpenAI, vLLM, llama-server, Ollama) accepts.
func oaContent(m ChatMessage) any {
	if len(m.Images) == 0 {
		return m.Content
	}
	parts := []map[string]any{}
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		url := img.URL
		if url == "" {
			url = "data:" + img.MediaType + ";base64," + img.Data
		}
		parts = append(parts, map[string]any{
			"type": "image_url", "image_url": map[string]string{"url": url},
		})
	}
	return parts
}

type oaStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// oaUsage is the OpenAI wire shape, mapped into our uniform Usage.
type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Thinking-model fields: Ollama/LM Studio say "reasoning",
			// llama-server says "reasoning_content" (DeepSeek convention).
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

// oaToolChoice maps the internal choice string ("", auto, any, none,
// name:<tool>) into OpenAI's dual-shape field.
func oaToolChoice(choice string) any {
	switch {
	case choice == "":
		return nil
	case choice == "auto", choice == "none":
		return choice
	case choice == "any":
		return "required"
	case strings.HasPrefix(choice, "name:"):
		return map[string]any{"type": "function", "function": map[string]string{"name": strings.TrimPrefix(choice, "name:")}}
	}
	return nil
}

// postStream sends one streaming chat request; the caller owns the body. A server
// closing a kept-alive connection just as it is reused fails the POST before any
// reply (llama-server: 9 of 40 requests), and Go only replays that for idempotent
// methods, so one retry here gets a fresh connection.
func (c *openaiClient) postStream(ctx context.Context, oaReq oaStreamRequest) (*http.Response, error) {
	body, err := json.Marshal(oaReq)
	if err != nil {
		return nil, err
	}
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.auth(req)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		return http.DefaultClient.Do(req)
	}
	resp, err := do()
	if err != nil && ctx.Err() == nil &&
		(errors.Is(err, io.EOF) || strings.Contains(err.Error(), "server closed idle connection")) {
		return do()
	}
	return resp, err
}

func (c *openaiClient) StreamChat(ctx context.Context, r ChatRequest, onDelta func(Delta) error) error {
	msgs := make([]oaMessage, len(r.Messages))
	for i, m := range r.Messages {
		role := m.Role
		if role == "system" && i > 0 {
			// vLLM-style chat templates demand system at index 0 and 400 the
			// rest ("System message must be at the beginning") — claude-cli
			// rides context as a mid-array system message. Demote later system
			// turns to user; the content reaches the model either way. (The
			// router's messages handler rewrites its own, so this guards the
			// OpenAI surface and older routers alike.)
			role = "user"
		}
		msgs[i] = oaMessage{Role: role, Content: oaContent(m),
			ToolCalls: oaToolCalls(m.ToolCalls), ToolCallID: m.ToolCallID}
	}
	requested := ""
	var options reasoningOptions
	if r.Reasoning != nil {
		requested = r.Reasoning.Effort
		options = c.reasoningOptionsFor(r.Model)
	}
	ladder := reasoningLadder(requested, options)
	rungKey := reasoningRungKey(r.Model, requested, len(r.Tools) > 0)
	rung := 0
	if requested != "" {
		rung = c.startRung(rungKey, ladder)
	}
	var temperature *float64
	if r.Temperature > 0 {
		t := r.Temperature
		temperature = &t
	}
	var priority *int
	if r.Priority != 0 {
		p := r.Priority
		priority = &p
	}
	oaReq := oaStreamRequest{
		Model:              r.Model,
		Messages:           msgs,
		MaxTokens:          r.MaxTokens,
		Stream:             true,
		ReasoningEffort:    ladder[rung].effort,
		ChatTemplateKwargs: ladder[rung].kwargs,
		Temperature:        temperature,
		Priority:           priority,
		ResponseFormat:     oaResponseFormatOf(r.ResponseFormat),
		Tools:              oaTools(r.Tools),
		ToolChoice: func() any {
			if v := oaToolChoice(r.ToolChoice); v != nil {
				return v
			}
			return nil
		}(),
		// Standard OpenAI field; Ollama/vLLM/LM Studio accept it and it
		// gives bench an honest usage cross-check.
		StreamOptions: &oaStreamOpts{IncludeUsage: true},
	}
	resp, err := c.postStream(ctx, oaReq)
	if err != nil {
		return err
	}
	// A refused thinking control is not a failed request: walk to the nearest
	// level the server takes. The rung is remembered only once one is accepted,
	// since a request that is bad for another reason fails all the way down.
	for start := rung; (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) && rung+1 < len(ladder); {
		resp.Body.Close()
		rung++
		oaReq.ReasoningEffort, oaReq.ChatTemplateKwargs = ladder[rung].effort, ladder[rung].kwargs
		if resp, err = c.postStream(ctx, oaReq); err != nil {
			return err
		}
		if resp.StatusCode == http.StatusOK && rung != start {
			c.rememberRung(rungKey, ladder[rung])
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream %s %s: %s", c.cfg.BaseURL, r.Model, httpStatus(resp))
	}
	var toolAcc []ToolCall // indexed accumulation across fragments
	var seen thinkingSeen
	err = scanSSE(resp.Body, func(data []byte) (bool, error) {
		if string(data) == "[DONE]" {
			return false, nil
		}
		var chunk oaChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return true, nil // tolerate keep-alives and vendor extras
		}
		var usage *Usage
		if chunk.Usage != nil {
			usage = &Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		d := Delta{Usage: usage}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			d.Content = choice.Delta.Content
			d.Reasoning = choice.Delta.Reasoning
			if d.Reasoning == "" {
				d.Reasoning = choice.Delta.ReasoningContent
			}
			d.FinishReason = choice.FinishReason
			// Tool calls arrive as indexed fragments: id/name on the first,
			// argument JSON in chunks. Accumulate and emit the complete
			// calls once the finish reason confirms them.
			for _, tc := range choice.Delta.ToolCalls {
				for len(toolAcc) <= tc.Index {
					toolAcc = append(toolAcc, ToolCall{})
				}
				if tc.ID != "" {
					toolAcc[tc.Index].ID = tc.ID
				}
				if tc.Function.Name != "" {
					toolAcc[tc.Index].Name = tc.Function.Name
				}
				toolAcc[tc.Index].Arguments += tc.Function.Arguments
			}
			// Emit the complete calls once, on the terminal chunk — when the
			// finish reason confirms them, or when fragments accumulated
			// under any other finish (a backend that stops while emitting
			// calls would otherwise silently drop them).
			if choice.FinishReason != "" && (choice.FinishReason == "tool_calls" || len(toolAcc) > 0) {
				for _, tc := range toolAcc {
					if tc.Name != "" {
						d.ToolCalls = append(d.ToolCalls, tc)
					}
				}
			}
		}
		if d.Content == "" && d.Reasoning == "" && d.FinishReason == "" && d.Usage == nil && len(d.ToolCalls) == 0 {
			return true, nil
		}
		seen.observe(d)
		if err := onDelta(d); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil || requested == "" {
		return err
	}
	if ladder[rung].applied == "off" && c.noteOff(rungKey, seen.thought()) {
		c.rememberRung(rungKey, ladder[firstRungPast(ladder)])
	}
	if o := reasoningOutcome(requested, ladder[rung], seen.thought()); o != nil {
		return onDelta(Delta{ReasoningOutcome: o})
	}
	return nil
}

func (c *openaiClient) auth(req *http.Request) {
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
}

// scanSSE drives fn per "data:" payload; fn returns false to stop cleanly.
func scanSSE(body io.Reader, fn func(data []byte) (bool, error)) error {
	br := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				continue
			}
			ok, err := fn([]byte(data))
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

func httpStatus(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if len(b) == 0 {
		return resp.Status
	}
	return resp.Status + ": " + strings.TrimSpace(string(b))
}
