package clientproto

import (
	"encoding/json"

	"remote.network/node/wire"
)

// ChatRequest is the consumer-facing OpenAI chat request — the fields a
// client may send and the router accepts on POST /v1/chat/completions.
// The router decodes into it (with its private routing flags alongside);
// the direct-path client builds the same shape and translates it with
// ToInference, so a request sent either way reaches the node identically.
//
// Attachments is the router-side document-reference array (attachments
// ride chat bodies; the router executes the read tool). The direct client
// only inspects presence: any attachment needs the router, so it falls
// back to the router's HTTP API rather than sending the request direct.
type ChatRequest struct {
	Model           string                 `json:"model"`
	Messages        []Message              `json:"messages"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	Stream          bool                   `json:"stream"`
	StreamOptions   *StreamOptions         `json:"stream_options,omitempty"`
	Tools           []Tool                 `json:"tools,omitempty"`
	ToolChoice      json.RawMessage        `json:"tool_choice,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"` // off|low|medium|high (+none/minimal aliases)
	Reasoning       *wire.ReasoningControl `json:"reasoning,omitempty"`        // normalized control
	// Temperature rides to backends that take one (optional 0..2); nil =
	// backend default.
	Temperature *float64 `json:"temperature,omitempty"`
	// ResponseFormat is a structured-output contract (a JSON schema or
	// json_object request), passed through to backends that can enforce
	// it. Never a 400: a backend that cannot enforce ignores it.
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	// Priority marks background work: forwarded so a vLLM started with
	// --scheduling-policy priority can overtake the tenant's own bulk
	// reads with interactive chat.
	Priority *int `json:"priority,omitempty"`
	// Attachments references documents uploaded to the router; presence
	// means the request needs the router (which executes the read tool).
	Attachments []Attachment `json:"attachments,omitempty"`
	// WebSearch asks the router to ground the answer in the web — a
	// router-executed search loop, so a request carrying it is never
	// routed direct.
	WebSearch bool `json:"web_search,omitempty"`
}

// StreamOptions is the OpenAI streaming-options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Attachment references one document uploaded to the router (id + display
// name); the model reads it through the router-executed read tool.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ToInference translates the request onto the node wire. modelID is the
// card id to dispatch (the caller's resolve step decides it); the
// reasoning control normalizes here exactly as the relayed path's
// Reasoning does, so both paths dispatch the same wire request.
func (r *ChatRequest) ToInference(modelID string) wire.InferenceRequestData {
	var tempOut *float64
	if r.Temperature != nil {
		t := *r.Temperature
		tempOut = &t
	}
	var priorityOut int
	if r.Priority != nil {
		priorityOut = *r.Priority
	}
	return wire.InferenceRequestData{
		Model:       modelID,
		Messages:    WireMessages(r.Messages),
		MaxTokens:   r.MaxTokens,
		Tools:       WireTools(r.Tools),
		ToolChoice:  ToolChoice(r.ToolChoice),
		Reasoning:   Reasoning(r.Reasoning, r.ReasoningEffort),
		Temperature: tempOut,
		// Structured output rides when it carries an enforceable shape and
		// is dropped (never refused) when it does not.
		ResponseFormat: ResponseFormat(r.ResponseFormat),
		Priority:       priorityOut,
	}
}
