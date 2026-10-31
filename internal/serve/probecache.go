// probecache.go: persistent probe/bench results per backend model. A model
// characterized once (metadata + measured tok/s) is never characterized
// again — not on restart, not on reconnect, not by the periodic re-list —
// unless the operator forces it (--reprobe / --probe-model). New models
// (never seen) get the full treatment. Facts live in ~/.remote so they
// survive the agent process entirely.
package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CachedModel is one backend model's characterization as the cache stores it.
type CachedModel struct {
	RawID    string `json:"raw_id"`
	Modality string `json:"modality,omitempty"`
	State    string `json:"state,omitempty"` // ready | ready-unbenched

	// Probe facts (what ProbeMeta/heuristics produced).
	Family       string   `json:"family,omitempty"`
	Quant        string   `json:"quant,omitempty"`
	ParamsB      float64  `json:"params_b,omitempty"`
	Ctx          int      `json:"ctx,omitempty"`
	SizeMB       int      `json:"size_mb,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"` // needed on the cache path too — tools/vision flags otherwise vanish on restart (pre-2026-10 cache files just lack it)

	// Bench facts (zero when skipped/failed).
	TPS       float64 `json:"tps,omitempty"`
	TTFTms    int     `json:"ttft_ms,omitempty"`
	BenchedAt string  `json:"benched_at,omitempty"`

	ProbedAt string `json:"probed_at"`
}

// ProbeCache is the on-disk characterization store, keyed
// "<backend_id>|<raw_id>" (the same model on two backends probes twice).
// A missing or corrupt file is an empty cache — never an error: the worst
// case is one extra characterization pass. mu guards Entries + the save
// path: the admit loop and a periodic Refresh can characterize
// concurrently.
type ProbeCache struct {
	mu      sync.Mutex
	path    string
	Entries map[string]CachedModel `json:"entries"`
}

// LoadProbeCache reads the cache at path (typically ~/.remote/probe-cache.json).
func LoadProbeCache(path string) *ProbeCache {
	c := &ProbeCache{path: path, Entries: map[string]CachedModel{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return c // first run
	}
	_ = json.Unmarshal(b, c) // corrupt file → empty entries, overwritten on save
	return c
}

func (c *ProbeCache) Get(key string) (CachedModel, bool) {
	if c == nil {
		return CachedModel{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.Entries[key]
	return m, ok
}

// Put records a characterization and persists immediately — the cache must
// survive a Ctrl+C right after a long bench, and at ≤256 small entries the
// write-per-put cost is nothing.
func (c *ProbeCache) Put(key string, m CachedModel) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Entries[key] = m
	c.saveLocked()
}

// Save flushes (Put already persists; this covers bulk edits).
func (c *ProbeCache) Save() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveLocked()
}

// saveLocked persists under mu (callers hold it).
func (c *ProbeCache) saveLocked() {
	if c.path == "" {
		return
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(c.path, b, 0o600); err != nil {
		return
	}
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }
