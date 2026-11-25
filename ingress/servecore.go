// servecore.go: the serving core every inference plane shares — the WSS
// relay, the router's punched/reverse QUIC plane, and (since the direct
// client path) the node's own client listener. One place counts the slot
// (per-connection and NODE-wide, so every router's load view sees direct
// requests too), invokes the backend, maps busy, and signs the node's
// usage receipt — with the ticket id on the direct path.
package ingress

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"remote.network/node/clientproto"
	"remote.network/node/wire"
)

// serveCore is the shared request core.
type serveCore struct {
	handler      InferenceHandler
	identity     ed25519.PrivateKey
	log          *slog.Logger
	connInFlight *atomic.Int64 // this connection's own dispatch count (nil = none)
	nodeInFlight *atomic.Int64 // the NODE-wide counter every client shares (nil = legacy)
}

// core is this client's serving core (built from its config; the config
// is frozen after New).
func (c *Client) core() *serveCore {
	c.coreOnce.Do(func() {
		c.coreRef = &serveCore{
			handler:      c.cfg.Handler,
			identity:     c.cfg.Identity,
			log:          c.log,
			connInFlight: &c.inFlight,
			nodeInFlight: c.cfg.NodeInFlight,
		}
	})
	return c.coreRef
}

func (ic *serveCore) acquire() {
	ic.connInFlight.Add(1)
	if ic.nodeInFlight != nil {
		ic.nodeInFlight.Add(1)
	}
}

func (ic *serveCore) release() {
	ic.connInFlight.Add(-1)
	if ic.nodeInFlight != nil {
		ic.nodeInFlight.Add(-1)
	}
}

// run drives one request to completion: concurrency slot, handler
// invocation, busy mapping, the node's signed receipt on the done frame.
// The non-empty code return means "terminal error with that wire code".
// ticketID is set on the direct path — the receipt bills against it.
func (ic *serveCore) run(ctx context.Context, reqID string, req wire.InferenceRequestData,
	emit func(wire.InferenceChunkData) error, ticketID string) (done wire.InferenceDoneData, code string, err error) {
	if ic.handler == nil {
		return wire.InferenceDoneData{}, "no_handler", errors.New("node agent has no serving backend")
	}
	ic.acquire()
	defer ic.release()

	start := time.Now()
	var firstChunk time.Time
	// What actually left toward the consumer: the cut-off receipt's count
	// when the request ends in an error after output was sent.
	var sentChunks, sentBytes int
	timed := func(chunk wire.InferenceChunkData) error {
		if firstChunk.IsZero() {
			firstChunk = time.Now()
		}
		if err := emit(chunk); err != nil {
			return err
		}
		if n := chunkBytes(chunk); n > 0 {
			sentChunks++
			sentBytes += n
		}
		return nil
	}
	done, err = ic.handler.HandleInference(ctx, req, timed)
	if err != nil {
		code = "backend_error"
		if errors.Is(err, wire.ErrBusy) {
			code = "busy"
		}
		ic.log.Warn("inference failed", "req_id", reqID, "model", req.Model, "code", code, "err", err)
		// A request that failed — or was cut off by the consumer — AFTER
		// output went out still delivered that output. On the direct path
		// the router never saw the stream, so without a receipt the answer
		// would be free (read every chunk, reset before done). Sign what was
		// sent: status failed_mid_stream, output = the backend's count if it
		// reported one, else the stream's own bound (max(chunks, bytes/4),
		// the router's estimate rule). Nothing sent → no receipt: a
		// pre-token failure bills nothing on any path.
		if sentChunks > 0 {
			cut := done
			if cut.OutputTokens <= 0 {
				cut.OutputTokens = max(sentChunks, (sentBytes+3)/4)
			}
			done.Receipt = ic.signReceiptStatus(reqID, req.Model, ticketID, cut, start, firstChunk, "failed_mid_stream")
		}
		return done, code, err
	}
	done.Receipt = ic.signReceipt(reqID, req.Model, ticketID, done, start, firstChunk)
	return done, code, err
}

// chunkBytes is the output a chunk carries toward the consumer.
func chunkBytes(c wire.InferenceChunkData) int {
	n := len(c.Content) + len(c.Reasoning)
	for _, tc := range c.ToolCalls {
		n += len(tc.Name) + len(tc.Arguments)
	}
	return n
}

// signReceipt is the node's signed account of a served request. Every
// data plane (relay, router QUIC, the client listener) goes through run,
// so every completed request carries one — the direct path's with the
// ticket id set. Any router may settle it (they share the spine; the
// receipt queue prefers the ticket's router and falls back to any
// connected one). A signing failure never fails the request.
func (ic *serveCore) signReceipt(reqID, model, ticketID string, done wire.InferenceDoneData, start, firstChunk time.Time) *wire.UsageReceipt {
	return ic.signReceiptStatus(reqID, model, ticketID, done, start, firstChunk, "ok")
}

// signReceiptStatus is signReceipt with an explicit status (ok |
// failed_mid_stream).
func (ic *serveCore) signReceiptStatus(reqID, model, ticketID string, done wire.InferenceDoneData, start, firstChunk time.Time, status string) *wire.UsageReceipt {
	if ic.identity == nil {
		return nil
	}
	end := time.Now()
	ttft := 0
	if !firstChunk.IsZero() {
		ttft = int(firstChunk.Sub(start).Milliseconds())
	}
	r, err := clientproto.SignReceipt(ic.identity, wire.UsageReceipt{
		TicketID: ticketID, RequestID: reqID, ModelID: model,
		InputTokens: done.InputTokens, OutputTokens: done.OutputTokens,
		CachedTokens: done.CachedTokens, ReasoningTokens: done.ReasoningTokens,
		Status: status, TTFTms: ttft, TotalMs: int(end.Sub(start).Milliseconds()), At: end.Unix(),
	})
	if err != nil {
		ic.log.Warn("receipt signing failed", "req_id", reqID, "err", err)
		return nil
	}
	return &r
}
