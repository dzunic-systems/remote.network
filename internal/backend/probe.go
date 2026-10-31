// probe.go: service-card metadata collection. Optional on the
// Client interface — backends that cannot describe their models (or do so in
// an unknown dialect) simply don't implement Prober, and the caller falls
// back to heuristics read off the raw model tag. Every miss is non-fatal:
// a card with unknown metadata is still servable, just unpriced-by-curve
// and ctx-less.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"remote.network/node/wire"
)

// Meta is what a backend can say about one model — the collectable subset
// of a service card.
type Meta struct {
	Family       string
	ParamsB      float64
	Quant        string
	Ctx          int
	SizeBytes    int64
	Capabilities []string // ollama: completion, tools, embedding, vision, …
}

// Prober is implemented by backends that can describe a model's metadata.
type Prober interface {
	ProbeMeta(ctx context.Context, model string) (Meta, error)
}

// CtxReporter is implemented by backends that state the context window
// they will actually enforce per model — vLLM's max_model_len rides the
// /v1/models listing, llama.cpp's n_ctx rides /props. Ollama doesn't
// implement it (its /api/ps residency is the truth there).
type CtxReporter interface {
	ServingCtxs() map[string]int
}

// Residencer is implemented by backends that can report which models are
// in memory right now, with the context window each is actually serving
// (Ollama's /api/ps). Backends without such an endpoint (vLLM,
// llama-server) don't implement it — residency is then unknowable and the
// caller assumes resident.
type Residencer interface {
	ProbeResidency(ctx context.Context) (map[string]int, error)
}

// metaTimeout bounds one metadata call: card collection must never make
// startup noticeably slower, and a slow answer is the same as a missing one
// (the heuristic path fills in).
const metaTimeout = 3 * time.Second

// oaShowRequest / oaShowResponse: Ollama's /api/show. vLLM and llama-server
// do not serve it — the request fails and ProbeMeta errors, which is the
// designed fallback path.
type oaShowRequest struct {
	Model string `json:"model"`
}

type oaShowResponse struct {
	Family        string   `json:"family"`
	ParameterSize string   `json:"parameter_size"`
	Quantization  string   `json:"quantization_level"`
	Capabilities  []string `json:"capabilities"`
	Details       struct {
		Families []string `json:"families"`
	} `json:"details"`
	ModelInfo map[string]any `json:"model_info"`
}

// oaTagsResponse: Ollama's /api/tags — the only public source of on-disk
// model size. Fetched once per client and cached; tags change rarely
// mid-process and the whole blob is small.
type oaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"models"`
}

// ProbeMeta collects one model's metadata from an Ollama-style endpoint.
// Without /api/show, what the /v1/models listing stated (ds4-server's
// context_length + supported_parameters) enriches the raw-id heuristic.
// Neither source = an error, not zero facts — the caller falls back to
// heuristics.
func (c *openaiClient) ProbeMeta(ctx context.Context, model string) (Meta, error) {
	meta, err := c.probeShow(ctx, model)
	if err == nil {
		return meta, nil
	}
	c.mu.Lock()
	n, tools, reasoning := c.modelCtx[model], c.listTools[model], c.listReasoning[model]
	c.mu.Unlock()
	if n == 0 && !tools && !reasoning {
		return Meta{}, err
	}
	meta = HeuristicMeta(model)
	if n > 0 {
		meta.Ctx = n
	}
	if tools || reasoning {
		meta.Capabilities = []string{"completion"}
		if tools {
			meta.Capabilities = append(meta.Capabilities, "tools")
		}
		if reasoning {
			meta.Capabilities = append(meta.Capabilities, "thinking")
		}
	}
	return meta, nil
}

func (c *openaiClient) probeShow(ctx context.Context, model string) (Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()

	body, err := json.Marshal(oaShowRequest{Model: model})
	if err != nil {
		return Meta{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/api/show", strings.NewReader(string(body)))
	if err != nil {
		return Meta{}, err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Meta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Meta{}, fmt.Errorf("show %s: %s", model, httpStatus(resp))
	}
	var show oaShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return Meta{}, err
	}

	meta := Meta{
		Family:       show.Family,
		Quant:        show.Quantization,
		Capabilities: show.Capabilities,
	}
	if meta.Family == "" && len(show.Details.Families) > 0 {
		meta.Family = show.Details.Families[0]
	}
	if p := parseParamSize(show.ParameterSize); p > 0 {
		meta.ParamsB = p
	}
	for k, v := range show.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok && f > 0 {
				meta.Ctx = int(f)
			}
		}
		if k == "general.parameter_count" {
			if f, ok := v.(float64); ok && f > 0 && meta.ParamsB == 0 {
				meta.ParamsB = f / 1e9
			}
		}
	}
	// Size from /api/tags (cached), keyed by exact name.
	if size, ok := c.tagSize(ctx, model); ok {
		meta.SizeBytes = size
	}
	return meta, nil
}

// oaPSResponse: Ollama's /api/ps — the only public source of what is
// resident right now and the context window each model is actually
// serving with. vLLM and llama-server do not serve it; the request fails
// and the caller treats residency as unknowable.
type oaPSResponse struct {
	Models []struct {
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
	} `json:"models"`
}

// ProbeResidency returns the loaded models keyed by raw id with the
// context window each is serving. A non-200 (backends without /api/ps) is
// an error, not zero facts — the caller keeps the previous snapshot.
func (c *openaiClient) ProbeResidency(ctx context.Context) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ps %s: %s", c.cfg.BaseURL, httpStatus(resp))
	}
	var ps oaPSResponse
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(ps.Models))
	for _, m := range ps.Models {
		if m.ContextLength > 0 {
			out[m.Name] = m.ContextLength
		}
	}
	return out, nil
}

// tagSize lazily fetches and caches /api/tags; misses just return ok=false.
func (c *openaiClient) tagSize(ctx context.Context, model string) (int64, bool) {
	c.tagsOnce.Do(func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/api/tags", nil)
		if err != nil {
			return
		}
		c.auth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		var tags oaTagsResponse
		if json.NewDecoder(resp.Body).Decode(&tags) != nil {
			return
		}
		sizes := make(map[string]int64, len(tags.Models))
		for _, m := range tags.Models {
			sizes[m.Name] = m.Size
		}
		c.tags = sizes
	})
	size, ok := c.tags[model]
	return size, ok
}

// parseParamSize reads ollama's "26.3B" / "7B" / "0.6B" notation; anything
// else ("latest", "") → 0.
func parseParamSize(s string) float64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !strings.HasSuffix(s, "B") {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "B"), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// --- heuristic fallback (no /api/show — vLLM, llama-server, unknown) ---

var (
	heuParam    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*[bB]`)
	heuQuant    = regexp.MustCompile(`(?i)(iq[0-9]+[a-z_]*|q[1-8]_?k?_?[a-z0-9_]*|bf16|fp16|f16|fp8|qat)`)
	heuSegSplit = regexp.MustCompile(`[-_:.]`)
)

// HeuristicMeta reads a card's collectable facts straight off the raw tag:
// family from the leading token, params from "26b"-style notation, quant
// from quant-suffix notation. Ctx is unknowable this way (0 = unknown).
func HeuristicMeta(raw string) Meta {
	var m Meta
	name := raw
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".gguf")

	if segs := heuSegSplit.Split(name, 2); len(segs) > 0 && segs[0] != "" && !isDigit(segs[0][0]) {
		m.Family = strings.ToLower(segs[0])
	}
	if loc := heuParam.FindStringSubmatch(name); loc != nil {
		if f, err := strconv.ParseFloat(loc[1], 64); err == nil {
			m.ParamsB = f
		}
	}
	if loc := heuQuant.FindStringSubmatch(name); loc != nil {
		m.Quant = strings.ToUpper(loc[1])
	}
	return m
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// embeddingHints are raw-id fragments that mark embedding models for
// backends that cannot declare capabilities.
var embeddingHints = []string{
	"embed", "bge-", "gte-", "e5-", "minilm", "nomic", "jina-", "mxbai", "arctic-embed",
}

// ModalityOf classifies a card: an explicit backend capability wins
// (ollama's /api/show reports "embedding"), otherwise the raw-id heuristic.
// Everything that is not demonstrably an embedder is chat — a chat request
// against a mistagged model fails loudly, the reverse would bill garbage.
func ModalityOf(raw string, meta Meta) string {
	for _, c := range meta.Capabilities {
		if strings.EqualFold(c, "embedding") {
			return "embedding"
		}
	}
	lower := strings.ToLower(raw)
	for _, h := range embeddingHints {
		if strings.Contains(lower, h) {
			return "embedding"
		}
	}
	return "chat"
}

// RawTag renders a raw backend id as the tag half of a namespaced card id.
// Lives in pkg/wire (delegated here for call-site brevity): the router
// derives pin tags with the same function, so the three sides — node
// naming, router resolution, consumer addressing — cannot drift.
func RawTag(raw string) string { return wire.RawTag(raw) }
