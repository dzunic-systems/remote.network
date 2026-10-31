// caps.go: probe-detected capabilities for backends that cannot declare
// them. vLLM executes tool calls and accepts images but carries zero
// capability facts on the wire — no /api/show (Ollama), no /props
// capabilities (llama.cpp), no supported_parameters (ds4/OpenRouter
// shape) — so a card built from its /v1/models listing advertises
// all-false and the operator had to pin --tool-capable/--vision-capable
// by hand. The probes here ask the server directly with two 1-token
// generations: a tool-bearing request (vLLM answers 400 "auto tool choice
// requires --enable-auto-tool-choice and --tool-call-parser to be set"
// without the parser, 200 with it) and a tiny-image request (a text-only
// model rejects the multimodal part). 200 = capable, 4xx = not, anything
// transient (timeout, 5xx) = unknown — never guessed.
//
// Ollama-dialect servers are exempt: their /api/show already answers
// authoritatively, and a probe there would cold-load the model just to
// ask. The dialect is detected by pinging /api/show with no model (404 =
// not Ollama; 200/400 = Ollama-ish), which never touches model memory.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds one capability probe: a 1-token generation can queue
// behind real traffic (vLLM --max-num-seqs is often 1-2), so it must
// tolerate a wait, but a probe must never stretch characterization.
const probeTimeout = 30 * time.Second

// DetectedCapabilities is what the probes confirmed about one model. true
// is evidence; false means "unknown or absent" and is never applied —
// cards default to false and operator pins stay authoritative, so a probe
// result can only ever upgrade a card, never clobber a pin or a lie.
type DetectedCapabilities struct {
	Tools    bool
	Vision   bool
	Thinking bool
	// Efforts is the reasoning_effort vocabulary the backend accepts,
	// probed only for thinking models (a non-thinking model has no effort
	// knob to ask about). Empty = unknown.
	Efforts []string
}

// CapabilityProber is implemented by backends whose real capability set
// can be established by micro-probing the endpoint (one short generation
// per question). Serve overlays the result on cards at characterization —
// fresh and cache-hit paths alike — so a server's support shows up
// without --reprobe.
type CapabilityProber interface {
	DetectCapabilities(ctx context.Context, model string) DetectedCapabilities
}

// pixelPNG is a 1×1 transparent PNG — the smallest valid image a vision
// endpoint will decode.
const pixelPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// probeCaps caches resolved probe results per model. Both outcomes of a
// 4xx answer are deterministic per server launch, so they are cached; a
// transient failure caches nothing and the next caller retries.
type probeCaps struct {
	mu      sync.Mutex
	dialect ollamaDialect
	results map[string]DetectedCapabilities
}

// ollamaDialect: unknown (probes not yet allowed), yes (skip — /api/show
// is authoritative), no (probe away).
type ollamaDialect int

const (
	dialectUnknown ollamaDialect = iota
	dialectOllama
	dialectOpenAI
)

func (c *openaiClient) DetectCapabilities(ctx context.Context, model string) DetectedCapabilities {
	if c.caps == nil {
		c.caps = &probeCaps{results: map[string]DetectedCapabilities{}}
	}
	c.caps.mu.Lock()
	defer c.caps.mu.Unlock()
	switch c.caps.dialect {
	case dialectOllama:
		return DetectedCapabilities{} // /api/show already told us; probing would cold-load
	case dialectUnknown:
		c.caps.dialect = c.pingShowDialect(ctx)
		if c.caps.dialect != dialectOpenAI {
			return DetectedCapabilities{} // Ollama, or endpoint dead this round — retry later
		}
	}
	if d, ok := c.caps.results[model]; ok {
		return d
	}
	d, resolved := c.probeCapabilities(ctx, model)
	if resolved {
		c.caps.results[model] = d
	}
	return d
}

// pingShowDialect classifies the server by how /api/show answers an empty
// request: 404 = no such route (vLLM, llama.cpp, ds4-server) → probe;
// anything else (Ollama answers 400 "model is required") → treat as
// Ollama-dialect and skip. A transport error leaves it unknown so a
// server that was still booting gets reclassified next call.
func (c *openaiClient) pingShowDialect(ctx context.Context) ollamaDialect {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/api/show", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return dialectUnknown
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return dialectUnknown
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return dialectOpenAI
	}
	return dialectOllama
}

// probeCapabilities runs the probes. resolved is false when any answer
// was transient (the caller must not cache a half-answer).
func (c *openaiClient) probeCapabilities(ctx context.Context, model string) (d DetectedCapabilities, resolved bool) {
	tools, ok := c.probeTools(ctx, model)
	if !ok {
		return DetectedCapabilities{}, false
	}
	vision, ok := c.probeVision(ctx, model)
	if !ok {
		return DetectedCapabilities{}, false
	}
	thinking, ok := c.probeThinking(ctx, model)
	if !ok {
		return DetectedCapabilities{}, false
	}
	var efforts []string
	if thinking {
		// Only thinking models take an effort: for the rest the knob does
		// not exist and a failed probe would read as a broken model.
		efforts = c.probeEfforts(ctx, model)
	}
	return DetectedCapabilities{Tools: tools, Vision: vision, Thinking: thinking, Efforts: efforts}, true
}

// effortVocabulary is every effort the fleet's consumers may send, in the
// order they should be offered. "xhigh" is here because the vLLM qwen3.8
// recipe's DEFAULT effort is xhigh and the build rejects everything else
// above medium — a vocabulary that stops at "high" cannot address the
// field-test card at all.
var effortVocabulary = []string{"off", "low", "medium", "high", "xhigh"}

// probeEfforts asks which reasoning_effort values the server accepts: one
// 1-token generation per candidate, 200 = accepted, 4xx = refused (vLLM's
// "Unexpected reasoning effort high. Supported types are …" among them).
// Transient failures just drop the candidate — a probe that cannot run is
// not evidence of refusal. All-refused (or all-transient) returns nil:
// unknown, not an empty vocabulary.
func (c *openaiClient) probeEfforts(ctx context.Context, model string) []string {
	var accepted []string
	for _, effort := range effortVocabulary {
		body, err := json.Marshal(oaStreamRequest{
			Model:           model,
			Messages:        []oaMessage{{Role: "user", Content: "Reply with the single word: ok"}},
			MaxTokens:       1,
			ReasoningEffort: effort,
		})
		if err != nil {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			continue
		}
		c.auth(req)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			accepted = append(accepted, effort)
		}
	}
	return accepted
}

// probeTools asks whether the server parses tool calls: a minimal
// tool-bearing generation with tool_choice "auto". 200 = parser present,
// 4xx = refused (vLLM's missing-parser message among them), transient =
// unknown.
func (c *openaiClient) probeTools(ctx context.Context, model string) (tools, ok bool) {
	body := oaStreamRequest{
		Model: model,
		Messages: []oaMessage{{Role: "user",
			Content: "Reply with the single word: ok"}},
		MaxTokens: 1,
		Tools: oaTools([]ToolSpec{{Name: "get_weather",
			Description: "Get current weather for a city",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}),
		ToolChoice: "auto",
	}
	return c.probeOnce(ctx, body)
}

// probeVision asks whether the model accepts image input: a 1×1 PNG part
// with a 1-token reply. A text-only model rejects the multimodal part
// with a 4xx; a vision model answers.
func (c *openaiClient) probeVision(ctx context.Context, model string) (vision, ok bool) {
	body := oaStreamRequest{
		Model: model,
		Messages: []oaMessage{{Role: "user", Content: []map[string]any{
			{"type": "text", "text": "Reply with the single word: ok"},
			{"type": "image_url", "image_url": map[string]string{"url": pixelPNG}},
		}}},
		MaxTokens: 1,
	}
	return c.probeOnce(ctx, body)
}

// probeThinking asks whether a default request actually gets reasoning.
// A thinking model emits its reasoning before any content, and it shows
// on the wire one way or another: reasoning_content (or OpenAI/llama.cpp
// "reasoning") when the server parses it (--reasoning-parser /
// --reasoning-format), or a literal "<think>" opening the content when it
// does not. The prompt must be a known reasoning trigger — adaptive
// thinkers (qwen3.8: reasoning_effort xhigh by default, per the vLLM
// recipe) legitimately skip thinking on easy prompts like plain
// arithmetic, which read as a false negative; the bat-and-ball trap does
// not get skipped (verified live: qwen3:30b detects true, the
// non-thinking qwen3.8-27b FP8 build detects false). Template
// introspection is not a shortcut: that same server's template reacts to
// chat_template_kwargs enable_thinking (render differs, 53 vs 13 tokens)
// while its weights never emit <think> — thinking-aware template,
// non-thinking checkpoint. Behavioral evidence only. 48 tokens is enough
// to see the opening; detection needs the first delta, not the thought.
func (c *openaiClient) probeThinking(ctx context.Context, model string) (thinking, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	b, err := json.Marshal(oaStreamRequest{
		Model: model,
		Messages: []oaMessage{{Role: "user",
			Content: "A bat and a ball cost $1.10 together. The bat costs $1.00 more than the ball. How much is the ball alone?"}},
		MaxTokens: 48,
	})
	if err != nil {
		return false, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return false, false
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, resp.StatusCode >= 400 && resp.StatusCode < 500
	}
	var out struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				Content          string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return false, false // truncated body — not an answer
	}
	if len(out.Choices) == 0 {
		return false, true
	}
	m := out.Choices[0].Message
	return m.ReasoningContent != "" || m.Reasoning != "" ||
		strings.HasPrefix(strings.TrimSpace(m.Content), "<think>"), true
}

// probeOnce POSTs one probe request (non-streaming) and classifies the
// answer: 200 = yes, 4xx = no, anything else = unknown.
func (c *openaiClient) probeOnce(ctx context.Context, body oaStreamRequest) (yes, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	b, err := json.Marshal(body)
	if err != nil {
		return false, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return false, false
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false // server busy/booting/dead — retry next call
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, true
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return false, true
	default:
		return false, false // 5xx: server under load — not an answer
	}
}
