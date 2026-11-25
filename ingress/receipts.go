// receipts.go: the direct path's receipt delivery. Every ticketed request
// ends with a node-signed receipt that must reach a router — the only
// thing that turns the node's work into earnings (and the consumer's
// reservation into a settled debit). The queue is write-ahead: the receipt
// lands in the queue directory first, delivery goes to the ticket's
// router (else any connected one — they share the spine and the receipts
// PK dedupes), and the node deletes its copy on the router's
// usage.receipt_ack. A receipt delivered but never acked (an old router)
// is resent to a harmless dedup; a node that loses its queue directory
// loses only its own earnings.
package ingress

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"remote.network/node/wire"
)

// Receipt delivery bounds and cadence.
const (
	receiptRetryEvery  = 30 * time.Second
	receiptMaxPending  = 5000               // files; a stuck node stops queueing past this
	receiptFileAgesOut = 7 * 24 * time.Hour // an un-ackable receipt dies of age (its ticket long gone)
)

// queuedReceipt is one queue file's contents: the receipt and the router
// it was for ("" = unknown — after a restart only the receipt survives,
// and any connected router may take it).
type queuedReceipt struct {
	RouterID string            `json:"router_id,omitempty"`
	Receipt  wire.UsageReceipt `json:"receipt"`
}

// receiptSink sends one usage.receipt envelope: routerID is the ticket's
// router when known; the sink falls back to any connected router itself.
// true = handed to a live session.
type receiptSink func(routerID string, env wire.Envelope) bool

// receiptStore is the queue.
type receiptStore struct {
	dir  string
	log  *slog.Logger
	send receiptSink

	mu   sync.Mutex
	acks map[string]bool // "ticket/request" → the router confirmed it
}

func newReceiptStore(dir string, log *slog.Logger, send receiptSink) *receiptStore {
	if log == nil {
		log = slog.Default()
	}
	return &receiptStore{dir: dir, log: log, send: send, acks: map[string]bool{}}
}

// enqueue persists the receipt (write-ahead) and starts delivery. The
// file comes first: a crash between the file and the send retries on the
// next sweep.
func (q *receiptStore) enqueue(r wire.UsageReceipt, routerID string) {
	if err := os.MkdirAll(q.dir, 0o700); err != nil {
		q.log.Warn("receipt queue unavailable — receipt lost", "dir", q.dir, "err", err)
		return
	}
	if n, err := countJSON(q.dir); err == nil && n >= receiptMaxPending {
		q.log.Warn("receipt queue full — receipt lost", "pending", n)
		return
	}
	b, err := json.Marshal(queuedReceipt{RouterID: routerID, Receipt: r})
	if err != nil {
		return
	}
	name := r.TicketID + "_" + r.RequestID + "_" + strconv.FormatInt(time.Now().Unix(), 10) + ".json"
	if err := os.WriteFile(filepath.Join(q.dir, name), b, 0o600); err != nil {
		q.log.Warn("receipt write failed — receipt lost", "err", err)
		return
	}
	q.deliverOne(filepath.Join(q.dir, name), queuedReceipt{RouterID: routerID, Receipt: r})
}

// deliverOne sends one queued receipt; the file stays until the ack (the
// router may crash between receiving and settling — the retry resends,
// the receipts PK dedupes).
func (q *receiptStore) deliverOne(path string, qr queuedReceipt) {
	if q.acked(qr.Receipt.TicketID, qr.Receipt.RequestID) {
		_ = os.Remove(path)
		return
	}
	env, err := wire.New(wire.TypeUsageReceipt, qr.Receipt.NodeID, qr.Receipt)
	if err != nil {
		return
	}
	if q.send != nil && q.send(qr.RouterID, env) {
		q.log.Debug("receipt delivered", "ticket_id", qr.Receipt.TicketID,
			"request_id", qr.Receipt.RequestID)
	}
}

// sweep delivers every queued receipt (reconnect, boot, retry tick) and
// ages out the dead ones.
func (q *receiptStore) sweep(ctx context.Context) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return // no queue dir = nothing queued
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(q.dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var qr queuedReceipt
		if json.Unmarshal(b, &qr) != nil || qr.Receipt.RequestID == "" {
			continue // unreadable junk: leave it for the age-out forensics
		}
		if qr.Receipt.At > 0 && time.Since(time.Unix(qr.Receipt.At, 0)) > receiptFileAgesOut {
			_ = os.Remove(path)
			q.log.Warn("receipt aged out un-acked",
				"ticket_id", qr.Receipt.TicketID, "request_id", qr.Receipt.RequestID)
			continue
		}
		q.deliverOne(path, qr)
	}
}

// noteAcked records the router's confirmation and drops the queue file.
func (q *receiptStore) noteAcked(ticketID, requestID string) {
	q.mu.Lock()
	q.acks[ticketID+"/"+requestID] = true
	q.mu.Unlock()
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || !strings.HasPrefix(name, ticketID+"_"+requestID+"_") {
			continue
		}
		_ = os.Remove(filepath.Join(q.dir, name))
	}
}

func (q *receiptStore) acked(ticketID, requestID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.acks[ticketID+"/"+requestID]
}

// run sweeps the queue forever (reconnects trigger immediate sweeps too).
func (q *receiptStore) run(ctx context.Context) {
	t := time.NewTicker(receiptRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.sweep(ctx)
		}
	}
}

func countJSON(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n, nil
}
