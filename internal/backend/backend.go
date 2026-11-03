// Package backend is the node-side inference backend abstraction — the
// dossier provider pattern at the surface a
// node needs: a Kind (wire protocol), a Config, model probing, and uniform
// streaming chat. Kind is the wire, not the vendor: openai covers
// OpenAI/OpenRouter/Together/…, openai-compatible covers Ollama/LM
// Studio/vLLM and RUNS THE SAME CODE as openai (the split is a form
// affordance), anthropic covers Anthropic-shaped endpoints. All kinds emit
// the same Delta stream so the router tunnel stays uniform regardless of
// what actually generated the tokens.
package backend

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"remote.network/node/wire"
)

// Kind is a wire protocol.
type Kind string

const (
	KindOpenAI           Kind = "openai"
	KindOpenAICompatible Kind = "openai-compatible"
	KindAnthropic        Kind = "anthropic"
)

// ParseKind validates a kind string from flags/config.
func ParseKind(s string) (Kind, error) {
	switch Kind(strings.ToLower(strings.TrimSpace(s))) {
	case KindOpenAI:
		return KindOpenAI, nil
	case KindOpenAICompatible, "ollama", "vllm", "lmstudio":
		return KindOpenAICompatible, nil
	case KindAnthropic:
		return KindAnthropic, nil
	}
	return "", fmt.Errorf("backend kind %q: want openai | openai-compatible | anthropic", s)
}

// Config is one backend endpoint (the dossier Config shape plus the node's
// serving concerns). APIKey comes from the environment or flags — never the
// database — and is only required for non-loopback endpoints.
type Config struct {
	Name    string
	Kind    Kind
	BaseURL string
	APIKey  string
	// Models, when set, restricts serving to these raw backend model ids
	// (after alias mapping). Empty means: probe and serve everything the
	// backend reports.
	Models []string
}

// ToolCall/ToolSpec are the shared wire shapes (same aliasing pattern as
// ServedModel): one definition in pkg/wire, reused verbatim by every layer.
type (
	ToolCall = wire.ToolCall
	ToolSpec = wire.ToolSpec
)

// ChatMessage is one turn; content may carry parts (vision).
// Tool fields ride the OpenAI shape: assistant turns may carry ToolCalls,
// tool results arrive as a "tool" role keyed by ToolCallID.
type ChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	Images     []wire.ImagePart `json:"images,omitempty"` // vision models
	ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// ChatRequest is the uniform generation request.
type ChatRequest struct {
	Model      string
	Messages   []ChatMessage
	MaxTokens  int
	Tools      []ToolSpec
	ToolChoice string                 // "" | "auto" | "any" | "none" | "name:<tool>"
	Reasoning  *wire.ReasoningControl // nil = backend default
	// Temperature is the consumer's sampling knob (0 = backend default);
	// backends without one ignore it.
	Temperature float64
	// ResponseFormat carries a structured-output constraint: enforce
	// when the backend can, ignore when it cannot — never refuse.
	ResponseFormat *wire.ResponseFormat
	// Priority is the consumer's scheduling hint (dossier background
	// passes send 10); a vLLM on priority scheduling orders by it.
	Priority int
}

// Usage is token accounting when the backend reports it (bench counts
// deltas independently — never trust, always cross-check).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Delta is one uniform streaming event, already translated from whatever
// wire shape the backend speaks. Reasoning carries thinking-model tokens
// (Ollama/LM Studio delta.reasoning, Anthropic thinking_delta) — they are
// generated, billable output, so bench and metering count them. ToolCalls,
// when set, carries complete tool invocations (Anthropic shapes them from
// its streamed fragments before they reach this layer).
type Delta struct {
	Content      string     `json:"content,omitempty"`
	Reasoning    string     `json:"reasoning,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        *Usage     `json:"usage,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	// ReasoningOutcome, on a final delta of its own, says the thinking control
	// was not met exactly.
	ReasoningOutcome *wire.ReasoningOutcome `json:"reasoning_outcome,omitempty"`
}

// Client is a backend connection.
type Client interface {
	// ProbeModels lists raw model ids the backend currently serves.
	ProbeModels(ctx context.Context) ([]string, error)
	// StreamChat drives one generation, invoking onDelta per event. An
	// error returned by onDelta aborts the stream and is returned.
	StreamChat(ctx context.Context, req ChatRequest, onDelta func(Delta) error) error
}

// New builds the client for cfg. Non-loopback endpoints must carry a key.
func New(cfg Config) (Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("backend %q: base URL required", cfg.Name)
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("backend %q: bad base URL %q", cfg.Name, cfg.BaseURL)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	// Accept both conventions — base with or without the /v1 version
	// prefix (OpenAI SDK style includes it; a bare host does not) — by
	// normalizing to the bare root. Clients always append /v1/….
	if strings.HasSuffix(cfg.BaseURL, "/v1") {
		cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/v1")
	}

	switch cfg.Kind {
	case KindOpenAI, KindOpenAICompatible:
		if cfg.APIKey == "" && !IsLoopback(cfg.BaseURL) {
			return nil, fmt.Errorf("backend %q: API key required for non-loopback %q (set --backend-api-key or REMOTE_BACKEND_API_KEY)", cfg.Name, cfg.BaseURL)
		}
		return &openaiClient{cfg: cfg}, nil
	case KindAnthropic:
		if cfg.APIKey == "" && !IsLoopback(cfg.BaseURL) {
			return nil, fmt.Errorf("backend %q: API key required for non-loopback %q", cfg.Name, cfg.BaseURL)
		}
		return &anthropicClient{cfg: cfg}, nil
	}
	return nil, fmt.Errorf("backend %q: unsupported kind %q", cfg.Name, cfg.Kind)
}

// IsLoopback reports whether the endpoint is on this machine — the dossier
// rule that a local backend (Ollama, llama-server) needs no credential.
func IsLoopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
