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
	// Efforts is the effort vocabulary the backend stated for the model
	// (a listing's reasoning object — TensorFold's /v1/models), "none"
	// already dropped: off is enable_thinking:false, not an effort.
	// Empty = unknown; probing (--thinking-efforts) fills it elsewhere.
	Efforts []string
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
	// Template is the model's chat template (Go text/template): it says
	// whether thinking can be switched off at all (alwaysThinks).
	Template string `json:"template"`
}

// oaTagsResponse: Ollama's /api/tags — the only public source of on-disk
// model size, and (since v0.1.4) of the manifest digest, the weights
// identity for ollama-served cards. Fetched once per client and cached;
// tags change rarely mid-process and the whole blob is small.
type oaTagsResponse struct {
	Models []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
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
	efforts := c.listEfforts[model]
	c.mu.Unlock()
	if n == 0 && !tools && !reasoning && len(efforts) == 0 {
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
	meta.Efforts = efforts
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
	c.noteThinking(model, alwaysThinks(show.Capabilities, show.Template))
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

// tagSize lazily fetches and caches /api/tags (sizes and, for the weights
// identity, digests); misses just return ok=false.
func (c *openaiClient) tagSize(ctx context.Context, model string) (int64, bool) {
	c.fetchTags(ctx)
	size, ok := c.tags[model]
	return size, ok
}

// ollamaDigest returns the manifest digest /api/tags reported for a model
// ("" when the backend is not Ollama-dialect or names no digest). The
// weights package normalizes it; this is the raw wire value.
func (c *openaiClient) ollamaDigest(ctx context.Context, model string) string {
	c.fetchTags(ctx)
	return c.tagDigests[model]
}

func (c *openaiClient) fetchTags(ctx context.Context) {
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
		digests := make(map[string]string, len(tags.Models))
		for _, m := range tags.Models {
			sizes[m.Name] = m.Size
			if m.Digest != "" {
				digests[m.Name] = m.Digest
			}
		}
		c.tags = sizes
		c.tagDigests = digests
	})
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

// thinkSwitch matches a template reading Ollama's think switch (.Think —
// not .IsThinkSet, which only says whether the request named it, and not
// .ThinkLevel, a level with no "off").
var thinkSwitch = regexp.MustCompile(`\.Think\b`)

// alwaysThinks reports an Ollama model that thinks whatever the request
// says: it has the thinking capability, but its template never reads the
// think switch — so think:false / reasoning_effort "none" change nothing
// in the prompt, and only make Ollama stop separating the thinking from
// the answer (the thinking then lands in the answer text). Measured
// 2026-11-26: qwen3:4b (the 2507 thinking edition) opens <think>
// unconditionally; the hybrid qwen3:1.7b writes an empty <think></think>
// when $.Think is false. qwen3.5:4b's stub template renders in Ollama's
// own code and honours think:false, so a template that never writes
// <think> proves nothing either way.
func alwaysThinks(capabilities []string, template string) bool {
	thinking := false
	for _, c := range capabilities {
		if c == "thinking" {
			thinking = true
		}
	}
	// The template must itself open the thinking block: a stub template
	// (qwen3.5's is 13 characters) means Ollama renders the prompt in its
	// own code, which may well honour think:false — it did for qwen3.5:4b.
	return thinking && strings.Contains(template, "<think>") && !thinkSwitch.MatchString(template)
}

// noteThinking records one model's alwaysThinks verdict.
func (c *openaiClient) noteThinking(model string, always bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.alwaysThinking == nil {
		c.alwaysThinking = map[string]bool{}
		c.thinkChecked = map[string]bool{}
	}
	c.thinkChecked[model] = true
	if always {
		c.alwaysThinking[model] = true
	}
}

// checkThinking reads one model's template from /api/show once, for a
// node whose card came from cache (the card probe that would have read it
// was skipped). Not Ollama (404), or any definite answer: recorded, never
// asked again. A transport error records nothing, so the next request
// retries.
func (c *openaiClient) checkThinking(model string) {
	c.mu.Lock()
	done := c.thinkChecked[model]
	c.mu.Unlock()
	if done {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()
	body, _ := json.Marshal(oaShowRequest{Model: model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/api/show", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < 500 {
			c.noteThinking(model, false)
		}
		return
	}
	var show oaShowResponse
	if json.NewDecoder(resp.Body).Decode(&show) != nil {
		return
	}
	c.noteThinking(model, alwaysThinks(show.Capabilities, show.Template))
}
