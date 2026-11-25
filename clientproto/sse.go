package clientproto

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"remote.network/node/wire"
)

// The OpenAI response shapes and the one renderer that emits them. The
// router's relay and the direct-path client both turn the same node wire
// events into consumer bytes, so both drive this one type and a client's
// answer is byte-identical to the router's for the same stream. Field
// order here IS the wire contract: encoding/json writes struct fields in
// declaration order, and golden tests pin the exact bytes.

// Usage is the consumer usage block (the OpenAI usage object).
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost,omitempty"` // USD for this call (OpenRouter convention); absent when free/unpriced
	// Reasoning is present only when the backend could not meet the
	// request's thinking control: what was asked and what ran instead.
	Reasoning *wire.ReasoningOutcome `json:"reasoning,omitempty"`
}

// ChunkDelta is one SSE delta (or, on a completion body, the message).
type ChunkDelta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"` // thinking models (DeepSeek convention)
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ChunkChoice is one streaming choice.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// Chunk is one SSE chat.completion.chunk frame's payload.
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// CompletionChoice is one choice of a completion body.
type CompletionChoice struct {
	Index        int        `json:"index"`
	Message      ChunkDelta `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

// Completion is the non-streaming chat.completion body.
type Completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   Usage              `json:"usage"`
	// Sources carries web-search sources on the JSON wire (raw, pre-
	// marshaled by the surfacing layer; the streaming surface emits them
	// as their own frame instead). The direct client never sets it.
	Sources json.RawMessage `json:"web_search,omitempty"`
}

// Response renders one chat response: SSE frames to its writer (streaming)
// and/or the accumulated completion body (non-streaming), folding the node
// wire's chunk events as they arrive. Everything consumer-visible — role
// chunk, content/reasoning/tool-call deltas with stable tool-call indexes,
// the finish chunk, the usage frame, the error event, [DONE] — is emitted
// here, once, for both data paths.
//
// The writer is pure output; keep-alive pings, source frames and other
// surfacing concerns belong to the caller, which may interleave its own
// frames on the same writer between Response calls. Cost prices the usage
// blocks (nil = no cost field).
type Response struct {
	w       io.Writer
	id      string
	model   string
	created int64
	Cost    func(in, out int) float64

	content   strings.Builder
	reasoning strings.Builder
	toolCalls []ToolCall // accumulated, merged (fragmented arguments appended)
	// toolIndexes maps a call id to its stable OpenAI delta index: two
	// parallel calls must not both land at 0.
	toolIndexes map[string]int
	finish      string
	// Physical bounds on what this stream produced: deltas that carried
	// output (each at least one token) and the bytes they spelled. The
	// router cross-checks the node's own count against them.
	outChunks int
	outBytes  int
}

// NewResponse builds a renderer. w may be nil: a nil writer accumulates
// only (the non-streaming body), a set one additionally emits each SSE
// frame as it folds.
func NewResponse(w io.Writer, id, model string, created int64) *Response {
	return &Response{w: w, id: id, model: model, created: created}
}

// Chunks counts the deltas that carried output.
func (r *Response) Chunks() int { return r.outChunks }

// Bytes counts the output bytes the deltas spelled.
func (r *Response) Bytes() int { return r.outBytes }

// Finish returns the stream's last finish_reason ("stop" when no chunk
// carried one).
func (r *Response) Finish() string {
	if r.finish != "" {
		return r.finish
	}
	return "stop"
}

// ToolCalls returns the merged tool calls, in emission order.
func (r *Response) ToolCalls() []ToolCall { return r.toolCalls }

// Content returns the accumulated text content.
func (r *Response) Content() string { return r.content.String() }

// Reasoning returns the accumulated reasoning text.
func (r *Response) Reasoning() string { return r.reasoning.String() }

// fold accumulates one chunk's output. Reports whether it carried output
// (content, reasoning or tool calls) — the caller's first-token clock.
func (r *Response) fold(c wire.InferenceChunkData) bool {
	if c.Content != "" {
		r.content.WriteString(c.Content)
		r.outChunks++
		r.outBytes += len(c.Content)
	}
	if c.Reasoning != "" {
		r.reasoning.WriteString(c.Reasoning)
		r.outChunks++ // reasoning is generated, billable output
		r.outBytes += len(c.Reasoning)
	}
	if len(c.ToolCalls) > 0 {
		// A tool call is generated output too.
		r.outChunks++
	}
	for _, tc := range c.ToolCalls {
		r.outBytes += len(tc.Name) + len(tc.Arguments)
		// Nodes emit complete calls in order; merge defensive against
		// fragmented arguments.
		merged := false
		for i := range r.toolCalls {
			if r.toolCalls[i].ID == tc.ID && tc.ID != "" {
				r.toolCalls[i].Function.Arguments += tc.Arguments
				merged = true
				break
			}
		}
		if !merged {
			r.toolCalls = append(r.toolCalls, ToolCall{
				ID: tc.ID, Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
	}
	if c.FinishReason != "" {
		r.finish = c.FinishReason
	}
	return c.Content != "" || c.Reasoning != "" || len(c.ToolCalls) > 0
}

// Fold accumulates one chunk and, on a streaming renderer, writes its SSE
// delta when the chunk carried output. Reports whether it did.
func (r *Response) Fold(c wire.InferenceChunkData) bool {
	had := r.fold(c)
	if r.w != nil && had {
		_ = r.write(chunkFrame(c, r.id, r.model, r.created, r.indexed(c.ToolCalls), nil))
	}
	return had
}

// indexed assigns each streamed tool call a stable OpenAI delta index,
// keyed by call id — consumers key fragments by this index.
func (r *Response) indexed(calls []wire.ToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, tc := range calls {
		var fn struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		fn.Name, fn.Arguments = tc.Name, tc.Arguments
		out = append(out, ToolCall{ID: tc.ID, Type: "function", Function: fn})
	}
	for i := range out {
		if r.toolIndexes == nil {
			r.toolIndexes = map[string]int{}
		}
		key := out[i].ID
		if key == "" {
			key = "\x00" + out[i].Function.Name + "#" + strconv.Itoa(i)
		}
		idx, seen := r.toolIndexes[key]
		if !seen {
			idx = len(r.toolIndexes)
			r.toolIndexes[key] = idx
		}
		out[i].Index = idx
	}
	return out
}

func chunkFrame(c wire.InferenceChunkData, id, model string, created int64, calls []ToolCall, finish *string) Chunk {
	return Chunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{
			Content: c.Content, ReasoningContent: c.Reasoning, ToolCalls: calls,
		}, FinishReason: finish}},
	}
}

// EmitRole writes the OpenAI-convention opening chunk (assistant role).
func (r *Response) EmitRole() error {
	return r.write(Chunk{
		ID: r.id, Object: "chat.completion.chunk", Created: r.created, Model: r.model,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Role: "assistant"}}},
	})
}

// EmitFinish writes the terminal chunk carrying the accumulated
// finish_reason (stop when none).
func (r *Response) EmitFinish() error {
	return r.write(Chunk{
		ID: r.id, Object: "chat.completion.chunk", Created: r.created, Model: r.model,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{}, FinishReason: strPtr(r.Finish())}},
	})
}

// EmitUsage writes the usage chunk (the include_usage stream trailer):
// empty choices, the request's final counts, cost through the Cost hook.
func (r *Response) EmitUsage(in, out int, reasoning *wire.ReasoningOutcome) error {
	return r.write(Chunk{
		ID: r.id, Object: "chat.completion.chunk", Created: r.created, Model: r.model,
		Choices: []ChunkChoice{},
		Usage:   r.usage(in, out, reasoning),
	})
}

func (r *Response) usage(in, out int, reasoning *wire.ReasoningOutcome) *Usage {
	u := &Usage{
		PromptTokens: in, CompletionTokens: out, TotalTokens: in + out,
		Reasoning: reasoning,
	}
	if r.Cost != nil {
		u.Cost = r.Cost(in, out)
	}
	return u
}

// EmitError writes the documented error event followed by [DONE] — the
// only failure shape left once a stream's headers have gone out.
func (r *Response) EmitError(typ, code, message string) error {
	if err := r.raw("data: {\"error\":{\"message\":" + jsonString(message) +
		",\"type\":\"" + typ + "\",\"code\":\"" + code + "\"}}\n\n"); err != nil {
		return err
	}
	return r.EmitDone()
}

// EmitDone writes the terminating [DONE] sentinel.
func (r *Response) EmitDone() error { return r.raw("data: [DONE]\n\n") }

// Body renders the non-streaming completion body from the accumulated
// state: one choice (role, content, reasoning, merged tool calls), the
// finish reason, the usage block (cost through the Cost hook). sources is
// the pre-marshaled web_search payload (nil = omit).
func (r *Response) Body(in, out int, reasoning *wire.ReasoningOutcome, sources json.RawMessage) []byte {
	u := r.usage(in, out, reasoning)
	b, err := json.Marshal(Completion{
		ID: r.id, Object: "chat.completion", Created: r.created, Model: r.model,
		Choices: []CompletionChoice{{
			Index: 0,
			Message: ChunkDelta{
				Role: "assistant", Content: r.content.String(),
				ReasoningContent: r.reasoning.String(), ToolCalls: r.toolCalls,
			},
			FinishReason: r.Finish(),
		}},
		Usage:   *u,
		Sources: sources,
	})
	if err != nil {
		return nil // never: the shape is plain JSON
	}
	return b
}

// jsonString is a Go string as one JSON string value.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (r *Response) write(v Chunk) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.raw("data: " + string(b) + "\n\n")
}

func (r *Response) raw(s string) error {
	if r.w == nil {
		return nil
	}
	_, err := io.WriteString(r.w, s)
	return err
}

func strPtr(s string) *string { return &s }
