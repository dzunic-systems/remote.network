// adapters.go: per-backend metric readers. Each
// adapter fetches `<base without /v1>/metrics` (or, for Ollama, the admin
// API), detects the dialect by metric prefix, and fills the backend's
// running/waiting/KV/rates. Every adapter's parse was written from a REAL
// body captured from that server:
//
//	tensorfold — captured 20 Nov on GB10, TensorFold 1.1.4
//	llamacpp   — captured 20 Nov from llama-server 0.8.0 --metrics (this
//	             build has no kv_cache_usage_ratio; the plan's caveat)
//	ollama     — captured 20 Nov from Ollama 0.40.16 (/metrics is 404;
//	             /api/ps carries size_vram + context_length)
//
// vLLM and SGLang ship DISABLED: no real server body has been captured
// yet, so per the plan's working rule they
// fall back to agent-sourced stats until the operator provides a body.
// Their metric names are already pinned in the plan for that moment.
package livestats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	httpTimeout  = 2 * time.Second
	metricsBytes = 1 << 20 // response cap
)

// adapterState is one backend's reader: dialect detection cache, the
// HTTP client, and the token-counter deltas behind DecodeTPS/PrefixHit.
type adapterState struct {
	id   string
	kind string
	base string // base URL without /v1
	// slots is the backend's slot count (the agent's max concurrent),
	// set by the sampler before each read; 0 = unknown.
	slots int

	client *http.Client

	mu        sync.Mutex
	dialect   string // detected prefix ("tensorfold"|"llamacpp"|"ollama"|"")
	detectAt  time.Time
	failures  int
	lastGen   float64 // generation_tokens_total at last read
	lastPmt   float64 // prompt_tokens_total at last read
	lastCache float64 // prompt_tokens_cached_total at last read
	lastAt    time.Time
}

func newAdapter(id, kind, baseURL string) *adapterState {
	return &adapterState{
		id:     id,
		kind:   kind,
		base:   strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/v1"), "/"),
		client: &http.Client{Timeout: httpTimeout},
	}
}

// read is one sample pass: returns whatever the backend exposed this
// round. nil fields mean the backend did not say — never 0.
func (a *adapterState) read(ctx context.Context) (running, waiting *int, kv *float64, decodeTPS, prefixHit *float64, memUsedMB *int64) {
	switch a.dialectOf(ctx) {
	case "tensorfold":
		return a.readTensorFold(ctx)
	case "llamacpp":
		return a.readLlamaCpp(ctx)
	case "ollama":
		return a.readOllama(ctx)
	default:
		// Unverified dialects (vllm, sglang) and non-HTTP kinds stay
		// agent-sourced: the sampler fills held/slots/busy itself.
		return nil, nil, nil, nil, nil, nil
	}
}

// dialectOf detects (and caches) the dialect by metric prefix, re-detecting
// after 3 failures or 10 minutes.
func (a *adapterState) dialectOf(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dialect != "" && time.Since(a.detectAt) < 10*time.Minute && a.failures < 3 {
		return a.dialect
	}
	kind := strings.ToLower(a.kind)
	if kind == "ollama" {
		// Ollama's /metrics is 404 (verified 0.40.16); its facts come from
		// /api/ps — the dialect IS the kind.
		a.dialect, a.detectAt, a.failures = "ollama", time.Now(), 0
		return a.dialect
	}
	body, err := a.fetch(ctx, "/metrics")
	if err != nil {
		// Ollama behind the generic kind: every Ollama node of the fleet is
		// registered openai-compatible (its /v1 is), and its /metrics is
		// 404. Until 21 Nov that left all four on agent counts only; the
		// /api/ps answer is what says Ollama.
		if a.isOllama(ctx) {
			a.dialect, a.detectAt, a.failures = "ollama", time.Now(), 0
			return a.dialect
		}
		a.failures++
		return a.dialect // keep the last detection until it fails hard
	}
	a.failures = 0
	a.detectAt = time.Now()
	a.dialect = ""
	// Only dialects with an adapter are detected; vllm/sglang metric
	// prefixes stay unrecognized (agent fallback) until a real body
	// verifies an adapter for them — see the plan's §N3 table for the
	// metric names pinned for that moment.
	for _, prefix := range []string{"tensorfold:", "llamacpp:"} {
		if strings.Contains(body, prefix) {
			a.dialect = strings.TrimSuffix(prefix, ":")
			break
		}
	}
	return a.dialect
}

// isOllama reports whether the backend answers Ollama's /api/ps: a JSON
// object carrying a "models" list (empty when nothing is loaded).
func (a *adapterState) isOllama(ctx context.Context) bool {
	body, err := a.fetch(ctx, "/api/ps")
	if err != nil {
		return false
	}
	var ps map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &ps) != nil {
		return false
	}
	m, ok := ps["models"]
	return ok && len(m) > 0 && m[0] == '['
}

func (a *adapterState) fetch(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", path, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, metricsBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// rate returns the per-second delta of a counter since the last read.
func (a *adapterState) rate(now time.Time, cur, last float64) (float64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastAt.IsZero() || !now.After(a.lastAt) {
		return 0, false
	}
	secs := now.Sub(a.lastAt).Seconds()
	if cur < last || secs <= 0 {
		return 0, false // counter reset (backend restart): skip one round
	}
	return (cur - last) / secs, true
}

// readTensorFold is the verified GB10/TensorFold 1.1.4 reader:
//
//	tensorfold:requests_running / requests_waiting — running, waiting
//	tensorfold:kv_cache_usage_ratio{pool="…"} — kv: the SUM over pools
//	  divided by the slot count. Each pool is one ACTIVE stream's tokens
//	  over that stream's own context window (an idle engine lists one
//	  pool at 0, ten running list ten), and TensorFold admits by slot, not
//	  by a shared cache. The max over pools, read until 21 Nov, made one
//	  long prompt (0.6 of its window) read as a 60%-full node with nine of
//	  ten slots free. Measured on GB10 21 Nov under 10 direct requests.
//	tensorfold:generation_tokens_total delta — decode_tps
//	tensorfold:prompt_tokens_cached_total / prompt_tokens_total deltas — prefix_hit
//	/health memory.active (BYTES) — mem_used_mb
func (a *adapterState) readTensorFold(ctx context.Context) (running, waiting *int, kv *float64, decodeTPS, prefixHit *float64, memUsedMB *int64) {
	now := time.Now()
	body, err := a.fetch(ctx, "/metrics")
	if err != nil {
		return nil, nil, nil, nil, nil, nil
	}
	var gen, pmt, cache float64
	sawGen := false
	kvSum, kvStreams := 0.0, 0
	for _, s := range parsePromText(body) {
		switch s.Name {
		case "tensorfold:requests_running":
			v := int(s.Value)
			running = &v
		case "tensorfold:requests_waiting":
			v := int(s.Value)
			waiting = &v
		case "tensorfold:kv_cache_usage_ratio":
			kvSum += s.Value
			kvStreams++
		case "tensorfold:generation_tokens_total":
			gen, sawGen = s.Value, true
		case "tensorfold:prompt_tokens_total":
			pmt = s.Value
		case "tensorfold:prompt_tokens_cached_total":
			cache = s.Value
		}
	}
	if kvStreams > 0 {
		// Unknown slots: the listed streams are the only denominator there
		// is (an over-statement while some slots are idle, never an under-).
		v := clamp01(kvSum / float64(max(a.slots, kvStreams)))
		kv = &v
	}
	if sawGen {
		if r, ok := a.rate(now, gen, a.lastGen); ok && r >= 0 {
			v := r
			decodeTPS = &v
		}
	}
	if pmtLast := a.lastPmt; pmt > pmtLast && cache >= a.lastCache {
		if d := cache - a.lastCache; d > 0 {
			v := clamp01(d / (pmt - pmtLast))
			prefixHit = &v
		}
	}
	a.mu.Lock()
	a.lastGen, a.lastPmt, a.lastCache, a.lastAt = gen, pmt, cache, now
	a.mu.Unlock()

	// /health's memory.active is BYTES (GB10: 86813219726) — MB for the
	// stat object.
	if health, err := a.fetch(ctx, "/health"); err == nil {
		var h struct {
			Memory struct {
				Active int64 `json:"active"`
			} `json:"memory"`
		}
		if json.Unmarshal([]byte(health), &h) == nil && h.Memory.Active > 0 {
			v := h.Memory.Active / 1048576
			memUsedMB = &v
		}
	}
	return running, waiting, kv, decodeTPS, prefixHit, memUsedMB
}

// readLlamaCpp is the verified llama-server 0.8.0 --metrics reader:
//
//	llamacpp:requests_processing / requests_deferred — running, waiting
//	llamacpp:tokens_predicted_total delta — decode_tps
//	llamacpp:prompt_tokens_cached_total / prompt_tokens_total deltas — prefix_hit
//
// This build exposes no KV usage ratio (absent in some builds — the plan
// noted it), so kv stays nil.
func (a *adapterState) readLlamaCpp(ctx context.Context) (running, waiting *int, kv *float64, decodeTPS, prefixHit *float64, memUsedMB *int64) {
	now := time.Now()
	body, err := a.fetch(ctx, "/metrics")
	if err != nil {
		return nil, nil, nil, nil, nil, nil
	}
	var pred, pmt, cache float64
	for _, s := range parsePromText(body) {
		switch s.Name {
		case "llamacpp:requests_processing":
			v := int(s.Value)
			running = &v
		case "llamacpp:requests_deferred":
			v := int(s.Value)
			waiting = &v
		case "llamacpp:tokens_predicted_total":
			pred = s.Value
		case "llamacpp:prompt_tokens_total":
			pmt = s.Value
		case "llamacpp:prompt_tokens_cached_total":
			cache = s.Value
		}
	}
	if r, ok := a.rate(now, pred, a.lastGen); ok && r >= 0 {
		v := r
		decodeTPS = &v
	}
	if pmt > a.lastPmt && cache >= a.lastCache {
		if d := cache - a.lastCache; d > 0 {
			v := clamp01(d / (pmt - a.lastPmt))
			prefixHit = &v
		}
	}
	a.mu.Lock()
	a.lastGen, a.lastPmt, a.lastCache, a.lastAt = pred, pmt, cache, now
	a.mu.Unlock()
	return running, waiting, nil, decodeTPS, prefixHit, nil
}

// readOllama is the verified Ollama 0.40.16 reader: /metrics is 404, so
// the facts come from /api/ps — loaded models with size_vram (BYTES),
// summed into mem_used_mb. Running/waiting are the agent's counts (the
// sampler fills them); there is no KV or rate exposure.
func (a *adapterState) readOllama(ctx context.Context) (running, waiting *int, kv *float64, decodeTPS, prefixHit *float64, memUsedMB *int64) {
	body, err := a.fetch(ctx, "/api/ps")
	if err != nil {
		return nil, nil, nil, nil, nil, nil
	}
	var ps struct {
		Models []struct {
			SizeVRAM int64 `json:"size_vram"`
		} `json:"models"`
	}
	if json.Unmarshal([]byte(body), &ps) != nil {
		return nil, nil, nil, nil, nil, nil
	}
	var vram int64
	for _, m := range ps.Models {
		vram += m.SizeVRAM
	}
	if vram > 0 {
		v := vram / 1048576
		memUsedMB = &v
	}
	return nil, nil, nil, nil, nil, memUsedMB
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// backendURLWithoutV1 trims a /v1-style suffix for the metrics probe.
// Kept as a package function for tests of odd spellings.
func backendURLWithoutV1(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		u.Path = strings.TrimSuffix(u.Path, "/v1")
		return u.String()
	}
	return newAdapter("t", "k", raw).base
}
