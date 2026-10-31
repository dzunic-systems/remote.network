package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicClient speaks the Anthropic Messages API (also Z.ai and other
// Anthropic-shaped endpoints via base_url). Its SSE stream is translated to
// the uniform Delta — this is the node-side translation, by design,
// so the router tunnel never carries vendor-specific shapes.
type anthropicClient struct {
	cfg Config
}

type anModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (c *anthropicClient) ProbeModels(ctx context.Context) ([]string, error) {
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
	var out anModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

type anTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anStreamRequest struct {
	Model      string          `json:"model"`
	MaxTokens  int             `json:"max_tokens"`
	Stream     bool            `json:"stream"`
	System     string          `json:"system,omitempty"` // role:system messages land here, not in Messages
	Thinking   *anThinking     `json:"thinking,omitempty"`
	Tools      []anTool        `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Messages   []anMessageOut  `json:"messages"`
	// Temperature rides when the consumer set one within Anthropic's 0..1
	// range; a larger value (the OpenAI vocabulary allows up to 2) drops —
	// passing it would 400 every request, and there is no honest clamp.
	Temperature *float64 `json:"temperature,omitempty"`
}

// anThinking is Anthropic's extended-thinking control. budget_tokens must
// sit below max_tokens; the caller adjusts max_tokens accordingly.
type anThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// anContentBlock is one content block on the Anthropic wire: text,
// thinking, image, tool_use (assistant), or tool_result (user).
type anContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   string          `json:"content,omitempty"` // tool_result payload
	Source    *anImageSource  `json:"source,omitempty"`  // image blocks
}

// anImageSource is an image block's source: inline base64 or a URL.
type anImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Anthropic SSE frames we consume; everything else is skipped.
type anEvent struct {
	Type  string          `json:"type"`
	Delta json.RawMessage `json:"delta"`
	Usage *anUsage        `json:"usage"`
}

type anContentDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Index       int    `json:"index"`
	PartialJSON string `json:"partial_json"`
}

type anUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anMessageOut is one outgoing Anthropic message with block-array content —
// the wire shape tool_use/tool_result require.
type anMessageOut struct {
	Role    string           `json:"role"`
	Content []anContentBlock `json:"content"`
}

// anTools maps the uniform tool specs to Anthropic's input_schema shape.
func anTools(specs []ToolSpec) []anTool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]anTool, 0, len(specs))
	for _, s := range specs {
		schema := s.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, anTool{Name: s.Name, Description: s.Description, InputSchema: schema})
	}
	return out
}

// anTranslateMessages converts uniform turns into Anthropic blocks:
// assistant tool calls become tool_use blocks, "tool" turns become user
// messages carrying tool_result blocks.
func anTranslateMessages(msgs []ChatMessage) []anMessageOut {
	out := make([]anMessageOut, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case strings.EqualFold(m.Role, "assistant") && len(m.ToolCalls) > 0:
			blocks := make([]anContentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, anContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Arguments)
				if !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anContentBlock{
					Type: "tool_use", ToolUseID: tc.ID, Name: tc.Name, Input: input,
				})
			}
			out = append(out, anMessageOut{Role: "assistant", Content: blocks})
		case strings.EqualFold(m.Role, "tool"):
			out = append(out, anMessageOut{Role: "user", Content: []anContentBlock{{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content,
			}}})
		default:
			blocks := []anContentBlock{}
			if m.Content != "" {
				blocks = append(blocks, anContentBlock{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				if img.URL != "" {
					blocks = append(blocks, anContentBlock{Type: "image",
						Source: &anImageSource{Type: "url", URL: img.URL}})
					continue
				}
				media := img.MediaType
				if media == "" {
					media = "image/png"
				}
				blocks = append(blocks, anContentBlock{Type: "image",
					Source: &anImageSource{Type: "base64", MediaType: media, Data: img.Data}})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anContentBlock{Type: "text", Text: ""})
			}
			out = append(out, anMessageOut{Role: m.Role, Content: blocks})
		}
	}
	return out
}

func (c *anthropicClient) StreamChat(ctx context.Context, r ChatRequest, onDelta func(Delta) error) error {
	// Anthropic takes the system prompt as a top-level field, not a
	// message — role:system entries are pulled out and joined. The router
	// always speaks OpenAI wire, so /v1/messages users and OpenAI clients
	// alike land here with system messages attached.
	var sys []string
	msgs := make([]ChatMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		if strings.EqualFold(m.Role, "system") {
			sys = append(sys, m.Content)
			continue
		}
		msgs = append(msgs, m)
	}
	// Thinking: budget → enabled block, with max_tokens kept above the
	// budget (Anthropic rejects budget >= max_tokens). "off" or nil = no
	// thinking block, which is Anthropic's default-off.
	var thinking *anThinking
	if r.Reasoning != nil && r.Reasoning.Effort != "off" {
		budget := r.Reasoning.MaxTokens
		if budget < 1024 {
			budget = 1024
		}
		maxTok := r.MaxTokens
		if maxTok <= budget {
			maxTok = budget + 4096
		}
		r.MaxTokens = maxTok
		thinking = &anThinking{Type: "enabled", BudgetTokens: budget}
	}
	var temperature *float64
	if r.Temperature > 0 && r.Temperature <= 1 {
		t := r.Temperature
		temperature = &t
	}
	body, err := json.Marshal(anStreamRequest{
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Stream:      true,
		Thinking:    thinking,
		System:      strings.Join(sys, "\n\n"),
		Temperature: temperature,
		Tools:       anTools(r.Tools),
		ToolChoice: func() json.RawMessage {
			switch r.ToolChoice {
			case "auto":
				return json.RawMessage(`{"type":"auto"}`)
			case "any":
				return json.RawMessage(`{"type":"any"}`)
			case strings.TrimPrefix(r.ToolChoice, "name:"):
				if strings.HasPrefix(r.ToolChoice, "name:") {
					b, _ := json.Marshal(map[string]string{
						"type": "tool", "name": strings.TrimPrefix(r.ToolChoice, "name:")})
					return b
				}
			}
			return nil
		}(),
		Messages: anTranslateMessages(msgs),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream %s %s: %s", c.cfg.BaseURL, r.Model, httpStatus(resp))
	}

	// usage accumulates from message_start (input) + message_delta (output);
	// tool_use blocks accumulate their input JSON across input_json_delta
	// fragments and are emitted as complete calls at message_stop.
	var usage Usage
	toolBlocks := map[int]*ToolCall{}
	var toolOrder []int
	stopReason := "stop"
	return scanSSE(resp.Body, func(data []byte) (bool, error) {
		var ev anEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return true, nil
		}
		switch ev.Type {
		case "content_block_start":
			var wrap struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal(data, &wrap) == nil && wrap.ContentBlock.Type == "tool_use" {
				toolBlocks[wrap.Index] = &ToolCall{ID: wrap.ContentBlock.ID, Name: wrap.ContentBlock.Name}
				toolOrder = append(toolOrder, wrap.Index)
			}
			return true, nil
		case "content_block_delta":
			var d anContentDelta
			if err := json.Unmarshal(ev.Delta, &d); err != nil {
				return true, nil
			}
			switch d.Type {
			case "text_delta":
				if d.Text == "" {
					return true, nil
				}
				if err := onDelta(Delta{Content: d.Text}); err != nil {
					return false, err
				}
			case "thinking_delta":
				if d.Thinking == "" {
					return true, nil
				}
				if err := onDelta(Delta{Reasoning: d.Thinking}); err != nil {
					return false, err
				}
			case "input_json_delta":
				if tc, ok := toolBlocks[d.Index]; ok {
					tc.Arguments += d.PartialJSON
				}
			}
			return true, nil
		case "message_start":
			// usage rides nested under message; best-effort.
			var wrap struct {
				Message struct {
					Usage *anUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(data, &wrap) == nil && wrap.Message.Usage != nil {
				usage.InputTokens = wrap.Message.Usage.InputTokens
			}
			return true, nil
		case "message_delta":
			if ev.Usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil && d.StopReason != "" {
				stopReason = d.StopReason
			}
			return true, nil
		case "message_stop":
			u := usage
			finish := oaStopReason(stopReason)
			var calls []ToolCall
			for _, i := range toolOrder {
				if tc := toolBlocks[i]; tc != nil && tc.Name != "" {
					if tc.Arguments == "" {
						tc.Arguments = "{}"
					}
					calls = append(calls, *tc)
				}
			}
			if len(calls) > 0 {
				finish = "tool_calls"
			}
			if err := onDelta(Delta{FinishReason: finish, Usage: &u, ToolCalls: calls}); err != nil {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
}

// oaStopReason maps Anthropic stop reasons onto the OpenAI vocabulary the
// internal wire speaks: the router's /v1/messages translates back
// (tool_calls → tool_use, length → max_tokens), and /v1/chat/completions
// consumers expect stop|length|tool_calls. Passing the raw Anthropic values
// through made claude-cli see stop_reason "end_turn" on tool_use turns —
// breaking its loop against anthropic-kind nodes.
func oaStopReason(an string) string {
	switch an {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default: // end_turn, stop_sequence, refusal, pause_turn, ""
		return "stop"
	}
}

func (c *anthropicClient) auth(req *http.Request) {
	if c.cfg.APIKey != "" {
		req.Header.Set("x-api-key", c.cfg.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
}
