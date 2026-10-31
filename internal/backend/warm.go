// warm.go: keep-warm — load a model into backend memory ahead of traffic.
// Ollama lists every pulled model on /v1/models, so a node advertises
// models that are not resident; the first request then pays a full cold
// load, and a first-token timeout cancels it mid-flight (the retry re-kills
// it — the model may never come up). Warming the pinned set at startup with
// keep_alive=-1 makes the cold window a startup event instead of a request
// event. Ollama-native endpoint; other backends simply don't implement
// Warmer and are skipped.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Warmer is implemented by backends that can load a model into resident
// memory on demand. WarmModel blocks until the model is loaded (minutes on
// small machines) — the caller bounds it with ctx and treats every error as
// non-fatal (the card stays servable; the first request just pays the load).
type Warmer interface {
	WarmModel(ctx context.Context, raw string) error
}

// WarmModel issues Ollama's load primitive: POST /api/chat with an empty
// message list and keep_alive=-1 (JSON number — some versions parse the
// string form as a duration and reject "-1"), loading the model resident
// until the Ollama process exits.
func (c *openaiClient) WarmModel(ctx context.Context, raw string) error {
	body, err := json.Marshal(map[string]any{
		"model": raw, "keep_alive": -1, "messages": []any{},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.cfg.BaseURL, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain: the response completes when the load completes.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("warm %s: %s", raw, httpStatus(resp))
	}
	return nil
}
