// Package clientproto is what a consumer-side client and the network must
// agree on byte for byte: the OpenAI request
// shapes and their translation onto the node wire, the newline-JSON
// envelope codec of a data-plane stream, and the conversation hash sticky
// routing keys by. The router imports it for its own consumer surface, the
// public client (remote.network/client) for the direct path, so a request
// sent either way reaches a node identically.
//
// Nothing here reads the network, the spine or a clock.
package clientproto

import (
	"encoding/json"
	"strings"

	"remote.network/node/wire"
)

// Message is one OpenAI chat message (the consumer-facing subset).
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OR content-part array (vision)
	// Tool fields ride the OpenAI shape: assistant turns may carry the
	// model's tool_calls, tool results arrive as role "tool" +
	// tool_call_id. Empty on plain turns.
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Text returns the message's text content (string form, or the text parts
// of a content-part array, concatenated).
func (m *Message) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ImageParts extracts image_url content parts as wire image parts (data
// URLs split into media type + base64; http(s) URLs pass through).
func (m *Message) ImageParts() []wire.ImagePart {
	if len(m.Content) == 0 {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var out []wire.ImagePart
	for _, p := range parts {
		if p.Type != "image_url" || p.ImageURL.URL == "" {
			continue
		}
		ip := wire.ImagePart{}
		if media, data, ok := strings.Cut(p.ImageURL.URL, "base64,"); ok && strings.HasPrefix(media, "data:") {
			ip.MediaType = strings.TrimSuffix(strings.TrimPrefix(media, "data:"), ";")
			ip.Data = strings.TrimSpace(data)
		} else {
			ip.URL = p.ImageURL.URL
		}
		out = append(out, ip)
	}
	return out
}

// Tool is the consumer-facing function tool.
type Tool struct {
	Type     string `json:"type"`
	Function ToolFn `json:"function"`
}

// ToolFn is a function tool's declaration.
type ToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is one model tool invocation in the OpenAI shape (history on
// requests, deltas on responses).
type ToolCall struct {
	ID       string `json:"id,omitempty"`
	Index    int    `json:"index,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// WireMessages translates chat history onto the node wire. Tool history
// rides along: assistant turns keep their tool_calls and tool results
// keep their tool_call_id — dropping either breaks the second round of
// every agent loop (the model never sees its own invocations; strict
// backends reject orphaned tool messages).
func WireMessages(in []Message) []wire.InferenceMessage {
	msgs := make([]wire.InferenceMessage, len(in))
	for i := range in {
		m := &in[i]
		calls := make([]wire.ToolCall, len(m.ToolCalls))
		for j, tc := range m.ToolCalls {
			calls[j] = wire.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
		}
		msgs[i] = wire.InferenceMessage{
			Role: m.Role, Content: m.Text(), Images: m.ImageParts(),
			ToolCalls: calls, ToolCallID: m.ToolCallID,
		}
	}
	return msgs
}

// WireTools maps the OpenAI tool shape 1:1 onto the uniform wire.
func WireTools(in []Tool) []wire.ToolSpec {
	out := make([]wire.ToolSpec, 0, len(in))
	for _, t := range in {
		out = append(out, wire.ToolSpec{
			Name: t.Function.Name, Description: t.Function.Description,
			Parameters: t.Function.Parameters,
		})
	}
	return out
}

// ToolChoice collapses OpenAI's dual-shape tool_choice into the wire's
// string grammar: "" | auto | any (=required) | none | name:<tool>.
func ToolChoice(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch v.Type {
	case "auto":
		return "auto"
	case "required":
		return "any"
	case "none":
		return "none"
	case "function":
		if v.Function.Name != "" {
			return "name:" + v.Function.Name
		}
	}
	return ""
}

// ResponseFormat translates the consumer's response_format onto the
// uniform wire. Lenient by design: a shape it does not recognize (type
// "text", a schemaless object, junk) returns nil and the dispatch carries
// no constraint — an unrecognized response_format is absent, never a 400.
func ResponseFormat(raw json.RawMessage) *wire.ResponseFormat {
	if len(raw) == 0 {
		return nil
	}
	var v struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	switch v.Type {
	case "json_object":
		return &wire.ResponseFormat{Type: "json_object"}
	case "json_schema":
		if v.JSONSchema == nil || len(v.JSONSchema.Schema) == 0 {
			return nil
		}
		return &wire.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &wire.JSONSchemaSpec{
				Name:   v.JSONSchema.Name,
				Strict: v.JSONSchema.Strict,
				Schema: v.JSONSchema.Schema,
			},
		}
	}
	return nil
}

// ReasoningEffort validates the consumer-facing reasoning_effort string
// and normalizes it to the wire vocabulary ("" = unknown or unset). "none"
// and "minimal" are OpenAI's spellings: "none" is thinking off, "minimal"
// the lightest thinking — mapped onto off/low so an unrecognized value
// never silently falls through to a thinking-max default. "xhigh" is in
// the set because some vLLM recipes reject plain "high".
func ReasoningEffort(v string) string {
	switch v {
	case "off", "low", "medium", "high", "xhigh":
		return v
	case "none":
		return "off"
	case "minimal":
		return "low"
	case "max", "ultra":
		return "xhigh"
	}
	return ""
}

// Reasoning resolves the reasoning control a request carries: an explicit
// control wins (normalized in place); otherwise the effort string. nil =
// the backend's default.
func Reasoning(c *wire.ReasoningControl, effort string) *wire.ReasoningControl {
	if c == nil {
		effort = ReasoningEffort(effort)
		if effort == "" {
			return nil
		}
		return &wire.ReasoningControl{Effort: effort}
	}
	c.Effort = ReasoningEffort(c.Effort)
	if c.MaxTokens < 0 {
		c.MaxTokens = 0
	}
	if c.Effort == "" && c.MaxTokens == 0 {
		return nil
	}
	return c
}
