package clientproto

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"remote.network/node/wire"
)

func msg(t *testing.T, raw string) Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTextAndImages(t *testing.T) {
	m := msg(t, `{"role":"user","content":[{"type":"text","text":"look "},{"type":"image_url","image_url":{"url":"data:image/png;base64, AAAA"}},{"type":"text","text":"here"},{"type":"image_url","image_url":{"url":"https://x/y.jpg"}}]}`)
	if got := m.Text(); got != "look here" {
		t.Errorf("Text = %q", got)
	}
	want := []wire.ImagePart{{MediaType: "image/png", Data: "AAAA"}, {URL: "https://x/y.jpg"}}
	if got := m.ImageParts(); !reflect.DeepEqual(got, want) {
		t.Errorf("ImageParts = %+v", got)
	}
	if s := msg(t, `{"role":"user","content":"plain"}`); s.Text() != "plain" || s.ImageParts() != nil {
		t.Errorf("string content: %q %v", s.Text(), s.ImageParts())
	}
}

func TestWireMessagesKeepsToolHistory(t *testing.T) {
	in := []Message{
		msg(t, `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`),
		msg(t, `{"role":"tool","tool_call_id":"c1","content":""}`),
	}
	got := WireMessages(in)
	if len(got) != 2 || got[0].ToolCalls[0] != (wire.ToolCall{ID: "c1", Name: "f", Arguments: `{"a":1}`}) || got[1].ToolCallID != "c1" {
		t.Errorf("WireMessages = %+v", got)
	}
}

func TestToolChoiceAndFormat(t *testing.T) {
	for raw, want := range map[string]string{
		``: "", `{"type":"auto"}`: "auto", `{"type":"required"}`: "any", `{"type":"none"}`: "none",
		`{"type":"function","function":{"name":"lookup"}}`: "name:lookup", `{"type":"function"}`: "", `junk`: "",
	} {
		if got := ToolChoice(json.RawMessage(raw)); got != want {
			t.Errorf("ToolChoice(%s) = %q, want %q", raw, got, want)
		}
	}
	if f := ResponseFormat(json.RawMessage(`{"type":"json_object"}`)); f == nil || f.Type != "json_object" {
		t.Errorf("json_object = %+v", f)
	}
	if f := ResponseFormat(json.RawMessage(`{"type":"json_schema","json_schema":{"name":"n","strict":true,"schema":{"type":"object"}}}`)); f == nil || f.JSONSchema.Name != "n" || !f.JSONSchema.Strict {
		t.Errorf("json_schema = %+v", f)
	}
	for _, raw := range []string{`{"type":"text"}`, `{"type":"json_schema"}`, `x`} {
		if f := ResponseFormat(json.RawMessage(raw)); f != nil {
			t.Errorf("%s = %+v, want nil (lenient)", raw, f)
		}
	}
}

func TestReasoning(t *testing.T) {
	for in, want := range map[string]string{"none": "off", "minimal": "low", "max": "xhigh", "high": "high", "bogus": ""} {
		if got := ReasoningEffort(in); got != want {
			t.Errorf("ReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
	if Reasoning(nil, "") != nil || Reasoning(nil, "bogus") != nil {
		t.Error("no effort must be nil (backend default)")
	}
	if c := Reasoning(nil, "none"); c == nil || c.Effort != "off" {
		t.Errorf("none = %+v", c)
	}
	if c := Reasoning(&wire.ReasoningControl{Effort: "bogus", MaxTokens: -5}, "high"); c != nil {
		t.Errorf("explicit junk control = %+v, want nil", c)
	}
	if c := Reasoning(&wire.ReasoningControl{MaxTokens: 1024}, ""); c == nil || c.MaxTokens != 1024 {
		t.Errorf("budget-only control = %+v", c)
	}
}

// The conversation hash is a cross-binary contract: the public client
// computes it, the router keys sticky routing by it. A change here moves
// every conversation once — make it on purpose, in both at the same time.
func TestConvHashVector(t *testing.T) {
	msgs := []Message{
		msg(t, `{"role":"system","content":"be brief"}`),
		msg(t, `{"role":"user","content":"hi"}`),
		msg(t, `{"role":"assistant","content":"hello"}`),
		msg(t, `{"role":"user","content":"later turn"}`),
	}
	// sha256("8:be brief" + "2:hi"), checked with Python hashlib.
	const want = "72c0855bf0ada30b4e8d57181b509c8dbd5dc4439fa9524197c01a0224955a93"
	got := ConvHash(msgs)
	if got != HashParts("be brief", "hi") {
		t.Fatalf("ConvHash is not HashParts(system, first user)")
	}
	if got != want {
		t.Logf("ConvHash vector = %s", got)
		t.Errorf("ConvHash changed — see the comment above")
	}
	if ConvHash(msgs[:1]) != "" {
		t.Error("no user message must give no key")
	}
	if HashParts("ab", "c") == HashParts("a", "bc") {
		t.Error("length prefixes must separate parts")
	}
}

type buf struct{ bytes.Buffer }

func (*buf) SetWriteDeadline(time.Time) error { return nil }

func TestEnvelopeRoundTrip(t *testing.T) {
	var b buf
	for i, typ := range []string{wire.TypeInferenceChunk, wire.TypeInferenceDone} {
		env, err := wire.NewReq(typ, "", "r1", map[string]int{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteEnvelope(&b, env, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	sc := NewEnvelopeScanner(&b)
	var got []string
	for sc.Scan() {
		var env wire.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		got = append(got, env.Type+"/"+env.ReqID)
	}
	if !reflect.DeepEqual(got, []string{wire.TypeInferenceChunk + "/r1", wire.TypeInferenceDone + "/r1"}) {
		t.Errorf("round trip = %v", got)
	}
}
