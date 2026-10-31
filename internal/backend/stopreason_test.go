package backend

import "testing"

// TestOaStopReason: the internal wire speaks OpenAI stop vocab — Anthropic
// values leaking through made the router translate "end_turn" (unknown) to
// end_turn even on tool_use turns, breaking claude-cli's loop.
func TestOaStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"refusal":       "stop",
		"pause_turn":    "stop",
		"":              "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
	}
	for in, want := range cases {
		if got := oaStopReason(in); got != want {
			t.Errorf("oaStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
