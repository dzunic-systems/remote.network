package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewReqCarriesReqID(t *testing.T) {
	env, err := NewReq(TypeInferenceRequest, "node123", "req456", InferenceRequestData{
		Model:    "qwen/qwen3-4b",
		Messages: []InferenceMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.ReqID != "req456" || env.V != Version || env.Type != TypeInferenceRequest {
		t.Fatalf("envelope = %+v", env)
	}
	// Round-trip: the correlation id survives the wire.
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ReqID != "req456" || back.NodeID != "node123" {
		t.Fatalf("round-trip = %+v", back)
	}
	var d InferenceRequestData
	if err := json.Unmarshal(back.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Model != "qwen/qwen3-4b" || len(d.Messages) != 1 || d.Messages[0].Content != "hi" {
		t.Fatalf("data = %+v", d)
	}
}

// TestAdvertisementRoundTrip pins the shared contract: what the node's
// serve stack serializes, the router must parse back — both sides use
// these types, and this guards the JSON tags against accidental edits.
func TestAdvertisementRoundTrip(t *testing.T) {
	var ad Advertisement
	ad.Hardware = json.RawMessage(`{"cpu":"m2"}`)
	ad.Backend.Kind = "openai-compatible"
	ad.Backend.Managed = true
	ad.Models = []ServedModel{{
		ModelID: "qwen/qwen3-4b", RawID: "qwen3:4b", State: "ready",
		TPS: 55.4, TTFTms: 127, MaxCtx: 32768, MaxConcurrent: 1,
		PriceInPer1M: 10, PriceOutPer1M: 25, BackendKind: "openai-compatible",
		Modality: ModalityChat, Family: "qwen3", ParamsB: 4, DefaultQuant: "Q4_K_M",
		SizeMB: 2500, BackendID: "local-ollama",
	}}
	b, err := json.Marshal(ad)
	if err != nil {
		t.Fatal(err)
	}

	back, ok := ParseAdvertisement(b)
	if !ok {
		t.Fatal("advertisement did not parse")
	}
	m := back.Models[0]
	if m.ModelID != "qwen/qwen3-4b" || m.RawID != "qwen3:4b" || m.TTFTms != 127 ||
		m.MaxConcurrent != 1 || m.PriceOutPer1M != 25 || !back.Backend.Managed {
		t.Fatalf("parsed = %+v", back)
	}
	if m.Modality != ModalityChat || m.Family != "qwen3" || m.ParamsB != 4 ||
		m.DefaultQuant != "Q4_K_M" || m.SizeMB != 2500 || m.BackendID != "local-ollama" {
		t.Fatalf("card facts lost: %+v", m)
	}
	// Static JSON shape check: the keys the other side depends on.
	for _, key := range []string{"model_id", "raw_id", "ttft_ms", "max_concurrent", "price_out_per_1m",
		"models", "modality", "params_b", "backend_id"} {
		if !strings.Contains(string(b), "\""+key+"\"") {
			t.Fatalf("serialized blob missing key %q: %s", key, b)
		}
	}

	// Empty card facts must not serialize (additive-field compat: an old
	// router reading a new node's blob must see nothing new).
	b2, err := json.Marshal(ServedModel{ModelID: "x/y", RawID: "x", State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"modality", "family", "params_b", "default_quant", "size_mb", "backend_id"} {
		if strings.Contains(string(b2), "\""+key+"\"") {
			t.Fatalf("empty card fact %q serialized: %s", key, b2)
		}
	}
}

// TestEmbedEnvelopeRoundTrip pins the embeddings envelope pair:
// terminal, non-streaming, one vector per input.
func TestEmbedEnvelopeRoundTrip(t *testing.T) {
	req, err := NewReq(TypeEmbedRequest, "node1", "req1", EmbedRequestData{
		Model: "nd-abc/qwen3-embedding:0.6b", Input: []string{"hello", "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Type != TypeEmbedRequest {
		t.Fatalf("type = %s", req.Type)
	}
	var er EmbedRequestData
	if err := json.Unmarshal(req.Data, &er); err != nil {
		t.Fatal(err)
	}
	if er.Model != "nd-abc/qwen3-embedding:0.6b" || len(er.Input) != 2 {
		t.Fatalf("embed request = %+v", er)
	}

	done, err := NewReq(TypeEmbedDone, "node1", "req1", EmbedDoneData{
		Embeddings:  [][]float32{{1, 2}, {3, 4}},
		InputTokens: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Type != TypeEmbedDone {
		t.Fatalf("type = %s", done.Type)
	}
	var ed EmbedDoneData
	if err := json.Unmarshal(done.Data, &ed); err != nil {
		t.Fatal(err)
	}
	if len(ed.Embeddings) != 2 || ed.Embeddings[1][0] != 3 || ed.InputTokens != 4 {
		t.Fatalf("embed done = %+v", ed)
	}
}

func TestParseAdvertisementRejectsNonAdvertisement(t *testing.T) {
	if _, ok := ParseAdvertisement([]byte(`{"cpu":"m2","ram_mb":32768}`)); ok {
		t.Fatal("plain hardware blob accepted as advertisement")
	}
	if _, ok := ParseAdvertisement([]byte(`not json`)); ok {
		t.Fatal("garbage accepted as advertisement")
	}
}
