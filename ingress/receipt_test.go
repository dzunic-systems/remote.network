package ingress

import (
	"context"
	"errors"
	"testing"

	"remote.network/node/clientproto"
	"remote.network/node/identity"
	"remote.network/node/wire"
)

type scriptedHandler struct {
	chunks int
	done   wire.InferenceDoneData
	err    error
}

func (h scriptedHandler) HandleInference(_ context.Context, _ wire.InferenceRequestData, emit func(wire.InferenceChunkData) error) (wire.InferenceDoneData, error) {
	for i := 0; i < h.chunks; i++ {
		if err := emit(wire.InferenceChunkData{Content: "x"}); err != nil {
			return wire.InferenceDoneData{}, err
		}
	}
	return h.done, h.err
}

// TestRunHandlerSignsReceipt: a completed request's inference.done carries
// a receipt signed by the node's identity key, naming the request, the
// model and the node's own counts; a failed request carries none.
func TestRunHandlerSignsReceipt(t *testing.T) {
	pub, priv, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	c := testClient(t.TempDir(), Config{Identity: priv, Handler: scriptedHandler{
		chunks: 3, done: wire.InferenceDoneData{InputTokens: 120, OutputTokens: 3, CachedTokens: 64},
	}})
	done, code, err := c.runHandler(context.Background(), "req-7", wire.InferenceRequestData{Model: "qwen/qwen3-4b"},
		func(wire.InferenceChunkData) error { return nil })
	if err != nil || code != "" {
		t.Fatalf("run: %q %v", code, err)
	}
	r := done.Receipt
	if r == nil {
		t.Fatal("no receipt on a completed request")
	}
	if err := clientproto.VerifyReceipt(identity.PubString(pub), *r); err != nil {
		t.Fatalf("receipt does not verify: %v", err)
	}
	if r.RequestID != "req-7" || r.ModelID != "qwen/qwen3-4b" || r.OutputTokens != 3 || r.InputTokens != 120 ||
		r.CachedTokens != 64 || r.Status != "ok" || r.At == 0 {
		t.Errorf("receipt = %+v", r)
	}

	c = testClient(t.TempDir(), Config{Identity: priv, Handler: scriptedHandler{err: errors.New("backend down")}})
	done, code, _ = c.runHandler(context.Background(), "req-8", wire.InferenceRequestData{Model: "m"},
		func(wire.InferenceChunkData) error { return nil })
	if code == "" || done.Receipt != nil {
		t.Errorf("failed request: code %q receipt %+v", code, done.Receipt)
	}
}
