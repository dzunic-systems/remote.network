// embed.go: the embeddings surface. Optional on the Client
// interface like Prober — anthropic-shaped backends have no embeddings API
// and simply don't implement Embedder, so they can never publish an
// embedding card they couldn't answer.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Embedder is implemented by backends that can produce vectors.
type Embedder interface {
	// Embed returns one vector per input, parallel by index, plus usage if
	// the backend reported it (InputTokens 0 = unreported).
	Embed(ctx context.Context, model string, input []string) ([][]float32, Usage, error)
}

type oaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type oaEmbedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// Embed calls the OpenAI embeddings shape — Ollama, vLLM and llama-server
// all speak it. Vectors are re-ordered by the backend's index field so the
// result is dense and parallel to input regardless of server ordering.
func (c *openaiClient) Embed(ctx context.Context, model string, input []string) ([][]float32, Usage, error) {
	body, err := json.Marshal(oaEmbedRequest{Model: model, Input: input})
	if err != nil {
		return nil, Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, Usage{}, fmt.Errorf("embed %s %s: %s", c.cfg.BaseURL, model, httpStatus(resp))
	}
	var out oaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	if len(out.Data) != len(input) {
		return nil, Usage{}, fmt.Errorf("embed %s: %d vectors for %d inputs", model, len(out.Data), len(input))
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		if len(d.Embedding) == 0 {
			return nil, Usage{}, fmt.Errorf("embed %s: empty vector at index %d", model, i)
		}
		vecs[i] = d.Embedding
	}
	var usage Usage
	if out.Usage != nil {
		usage = Usage{InputTokens: out.Usage.PromptTokens}
	}
	return vecs, usage, nil
}
