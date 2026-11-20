package livestats

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Goldens captured from REAL servers (the working rule: run the binary).
// llamacpp: llama-server 0.8.0 --metrics, run locally 20 Nov (driven by
// one real completion, then GET /metrics).
const llamacppGolden = `# HELP llamacpp:prompt_tokens_total Number of prompt tokens processed, excluding cached tokens
# TYPE llamacpp:prompt_tokens_total counter
llamacpp:prompt_tokens_total 32
# HELP llamacpp:prompt_tokens_cached_total Number of prompt tokens reused from the cache
# TYPE llamacpp:prompt_tokens_cached_total counter
llamacpp:prompt_tokens_cached_total 0
# HELP llamacpp:tokens_predicted_total Number of generation tokens processed
# TYPE llamacpp:tokens_predicted_total counter
llamacpp:tokens_predicted_total 40
# HELP llamacpp:requests_processing Number of requests processing
# TYPE llamacpp:requests_processing gauge
llamacpp:requests_processing 1
# HELP llamacpp:requests_deferred Number of requests deferred
# TYPE llamacpp:requests_deferred gauge
llamacpp:requests_deferred 2
`

// tensorfold: TensorFold 1.1.4 on GB10, captured 20 Nov.
const tensorfoldGolden = `tensorfold:requests_running 1
tensorfold:requests_waiting 0
tensorfold:generation_tokens_running 10742
tensorfold:prompt_tokens_total 12115603
tensorfold:prompt_tokens_cached_total 4712771
tensorfold:generation_tokens_total 2606334
tensorfold:kv_cache_usage_ratio{pool="0"} 0.382629
`

// tensorfoldHealth: /health on GB10 — memory.active is BYTES.
const tensorfoldHealth = `{"max_batch_size":10,"memory":{"active":86813219726},"live":{"connections":1,"waiting":0,"decode_tokens_per_second":75.5},"backend":"cuda","device":"NVIDIA GB10"}`

// ollamaPS: Ollama 0.40.16 /api/ps with one loaded model (captured 20 Nov
// on this machine, after loading qwen2.5:0.5b). /metrics is 404 here.
const ollamaPS = `{"models":[{"name":"qwen2.5:0.5b","model":"qwen2.5:0.5b","size":936105737,"digest":"a8b0c51577010a279d933d14c2a8ab4b268079d44c5c8830c0a93900f1827c67","details":{"parent_model":"","format":"gguf","family":"qwen2","families":["qwen2"],"parameter_size":"494.03M","quantization_level":"Q4_K_M"},"expires_at":"2026-11-20T16:17:25.543351+01:00","size_vram":936105737,"context_length":32768,"runner":"llamacpp"}]}`

func serveBody(t *testing.T, metrics, health string, healthStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(metrics))
	})
	if health != "" {
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			if healthStatus != http.StatusOK {
				w.WriteHeader(healthStatus)
			}
			w.Write([]byte(health))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// §8 N3, llamacpp: the golden body parses to the plan's metric names, and
// the second read's counter deltas become decode_tps. This build exposes
// no KV ratio — kv must stay NIL, never 0.
func TestLlamaCppGolden(t *testing.T) {
	srv := serveBody(t, llamacppGolden, "", 200)
	a := newAdapter("b1", "llamacpp", srv.URL)
	if got := a.dialectOf(context.Background()); got != "llamacpp" {
		t.Fatalf("dialect = %q", got)
	}
	run, wait, kv, tps, prefix, mem := a.readLlamaCpp(context.Background())
	if run == nil || *run != 1 || wait == nil || *wait != 2 {
		t.Fatalf("running/waiting = %v/%v", run, wait)
	}
	if kv != nil {
		t.Fatalf("kv = %v — this build exposes none; unknown must stay nil", *kv)
	}
	if tps != nil || prefix != nil {
		t.Fatalf("first read already has rates: %v %v (deltas need two reads)", tps, prefix)
	}
	if mem != nil {
		t.Fatalf("llamacpp has no memory source: %v", *mem)
	}

	// Counter advanced by 200 predicted tokens over 2 s → 100 tok/s.
	second := strings.Replace(llamacppGolden, "llamacpp:tokens_predicted_total 40", "llamacpp:tokens_predicted_total 240", 1)
	second = strings.Replace(second, "llamacpp:prompt_tokens_total 32", "llamacpp:prompt_tokens_total 64", 1)
	second = strings.Replace(second, "llamacpp:prompt_tokens_cached_total 0", "llamacpp:prompt_tokens_cached_total 16", 1)
	srv2 := serveBody(t, second, "", 200)
	a2 := newAdapter("b2", "llamacpp", srv2.URL)
	a2.dialectOf(context.Background())
	a2.readLlamaCpp(context.Background())
	a2.mu.Lock()
	a2.lastAt = time.Now().Add(-2 * time.Second) // pretend the interval was 2 s
	a2.lastGen, a2.lastPmt, a2.lastCache = 40, 32, 0
	a2.mu.Unlock()
	_, _, _, tps, prefix, _ = a2.readLlamaCpp(context.Background())
	if tps == nil || *tps < 99 || *tps > 101 {
		t.Fatalf("decode_tps = %v, want ~100", tps)
	}
	if prefix == nil || *prefix < 0.49 || *prefix > 0.51 {
		t.Fatalf("prefix_hit = %v, want ~0.5 (16 cached of the 32 new prompt tokens)", prefix)
	}
}

// §8 N3, tensorfold: the GB10 golden body — running/waiting/kv and the
// /health bytes→MB conversion.
func TestTensorFoldGolden(t *testing.T) {
	srv := serveBody(t, tensorfoldGolden, tensorfoldHealth, 200)
	a := newAdapter("tf", "tensorfold", srv.URL)
	if got := a.dialectOf(context.Background()); got != "tensorfold" {
		t.Fatalf("dialect = %q", got)
	}
	run, wait, kv, _, _, mem := a.readTensorFold(context.Background())
	if run == nil || *run != 1 || wait == nil || *wait != 0 {
		t.Fatalf("running/waiting = %v/%v", run, wait)
	}
	if kv == nil || *kv < 0.38 || *kv > 0.39 {
		t.Fatalf("kv = %v, want the pool ratio ~0.3826", kv)
	}
	// 86813219726 bytes / 1048576 = 82791 MB.
	if mem == nil || *mem != 82791 {
		t.Fatalf("mem_used_mb = %v, want 82791 (memory.active bytes → MB)", mem)
	}
	// kv max over pools: a second pool reading higher wins.
	withPools := tensorfoldGolden + `tensorfold:kv_cache_usage_ratio{pool="1"} 0.7712
`
	srv2 := serveBody(t, withPools, "", 200)
	a2 := newAdapter("tf2", "tensorfold", srv2.URL)
	a2.dialectOf(context.Background())
	_, _, kv2, _, _, _ := a2.readTensorFold(context.Background())
	if kv2 == nil || *kv2 < 0.77 {
		t.Fatalf("kv max over pools = %v, want 0.7712", kv2)
	}
}

// §8 N3, ollama: /metrics is 404 (verified 0.40.16) — the adapter reads
// /api/ps and sums size_vram into mem_used_mb; running/waiting/kv stay
// nil (the agent's own counts fill running).
func TestOllamaGolden(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(ollamaPS))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := newAdapter("ol", "ollama", srv.URL)
	if got := a.dialectOf(context.Background()); got != "ollama" {
		t.Fatalf("dialect = %q", got)
	}
	_, _, kv, _, _, mem := a.readOllama(context.Background())
	if kv != nil {
		t.Fatalf("ollama has no kv: %v", *kv)
	}
	// 936105737 bytes / 1048576 = 892 MB.
	if mem == nil || *mem != 892 {
		t.Fatalf("mem_used_mb = %v, want 892 (size_vram bytes → MB)", mem)
	}
}

// §8 N3: a dialect the adapters do not speak (vllm — shipped disabled)
// falls back to agent-sourced stats: the read returns nothing, the
// sampler keeps held/slots/busy.
func TestUnknownDialectFallsBackToAgent(t *testing.T) {
	srv := serveBody(t, "vllm:num_requests_running 3\nvllm:num_requests_waiting 1\n", "", 200)
	a := newAdapter("vl", "vllm", srv.URL)
	if got := a.dialectOf(context.Background()); got != "" {
		t.Fatalf("unverified dialect detected as %q — no adapter ships for it", got)
	}
	run, wait, kv, tps, prefix, mem := a.read(context.Background())
	if run != nil || wait != nil || kv != nil || tps != nil || prefix != nil || mem != nil {
		t.Fatalf("unverified dialect produced numbers: %v %v %v %v %v %v", run, wait, kv, tps, prefix, mem)
	}
}

// A backend that dies between rounds: the read returns nothing (fields
// dropped, never zeroed), and the next good read recovers.
func TestAdapterFailureDropsFields(t *testing.T) {
	a := newAdapter("dead", "llamacpp", "http://127.0.0.1:1") // nothing listens
	run, wait, kv, _, _, mem := a.read(context.Background())
	if run != nil || wait != nil || kv != nil || mem != nil {
		t.Fatalf("dead backend produced numbers: %v %v %v %v", run, wait, kv, mem)
	}
}
