package clientproto

import (
	"bufio"
	"encoding/json"
	"io"
	"time"

	"remote.network/node/wire"
)

// A data-plane stream (router↔node today, client↔node on the direct path)
// carries newline-delimited JSON wire.Envelopes: one request envelope in,
// response envelopes out, then FIN. These are the codec's two halves; both
// ends use them so the framing and the line cap can never drift apart.

// MaxEnvelopeLine bounds one envelope line. A response chunk is tiny; a
// request carrying base64 images is the large case the cap must admit.
const MaxEnvelopeLine = 2 << 20

// NewEnvelopeScanner reads one envelope per line from r.
func NewEnvelopeScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxEnvelopeLine)
	return sc
}

// deadlineWriter is the part of a QUIC stream WriteEnvelope needs.
type deadlineWriter interface {
	io.Writer
	SetWriteDeadline(time.Time) error
}

// WriteEnvelope writes env as one line, under a write deadline of wait
// (0 = no deadline).
func WriteEnvelope(w deadlineWriter, env wire.Envelope, wait time.Duration) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if wait > 0 {
		_ = w.SetWriteDeadline(time.Now().Add(wait))
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
