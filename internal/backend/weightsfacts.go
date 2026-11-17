// weightsfacts.go: what a backend can say about where a model's weights
// live on disk — the raw facts the serve stack turns into a WeightsID
// (the provenance tiers below an operator --weights declaration).
// Implemented by the openai kinds (Ollama digest, vLLM root, llama.cpp model_path); backends that
// cannot say anything simply don't implement it, which leaves the card
// without a weights identity — never with a guessed one.
package backend

import "context"

// WeightsProvenance is implemented by backends whose model listing or
// props endpoint names the files behind a model.
type WeightsProvenance interface {
	// WeightsFacts reports, for one raw model id:
	//   digest — the Ollama manifest digest from /api/tags ("" = unreported)
	//   root   — the /v1/models listing's root path ("" = unreported)
	//   path   — the llama.cpp /props model_path ("" = unreported)
	WeightsFacts(ctx context.Context, model string) (digest, root, path string)
}

// WeightsFacts collects the three sources. All are best-effort with the
// probe-path timeouts: a backend that cannot answer leaves the field
// empty and the card simply carries no weights id from that tier.
func (c *openaiClient) WeightsFacts(ctx context.Context, model string) (digest, root, path string) {
	c.mu.Lock()
	root = c.listRoot[model]
	propsDone := c.propsDone
	c.mu.Unlock()
	if !propsDone {
		c.loadProps()
	}
	c.mu.Lock()
	path = c.propsModelPath
	if len(c.listed) > 1 {
		// /props describes ONE loaded model; a backend listing several
		// (llama-server router mode, llama-swap) would stamp that file on
		// every card. No id beats a wrong one.
		path = ""
	}
	c.mu.Unlock()
	digest = c.ollamaDigest(ctx, model)
	return digest, root, path
}
