// Package livestats is the node agent's live sampler: ONE goroutine per
// agent — never per router connection — that
// every 5 s (GPU every 10 s: nvidia-smi costs a fork) assembles the
// newest wire.NodeStats and publishes it behind an atomic pointer. Every
// client's heartbeat reads the same pointer; `remote node top` (N6)
// reads the same object from ~/.remote/live.json. Each probe runs under
// a 2 s timeout; a failing probe drops its field and logs at most once
// per 10 minutes. The sampler starts once the first backend probe has
// succeeded (the CLI starts it after serve.Prepare) and stops when its
// context dies (drain).
package livestats

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"remote.network/node/wire"
)

// SampleEvery is the machine+backend cadence; GPUEvery exists because
// nvidia-smi is a process spawn, and half the cadence is still fresh
// enough for a load view.
const (
	SampleEvery = 5 * time.Second
	GPUEvery    = 10 * time.Second
	probeWait   = 2 * time.Second
	logEvery    = 10 * time.Minute
)

// gpuReading is a platform probe's GPU slot (platform files fill it; the
// sampler converts it to wire.GPUStats).
type gpuReading struct {
	Index      int
	Util       *float64
	MemUsedMB  *int64
	MemTotalMB *int64
	PowerW     *float64
	TempC      *float64
}

// BackendLive is one backend as the serving runtime exposes it — the
// agent's own truth that every backend reports regardless of dialect.
type BackendLive struct {
	ID     string
	Kind   string
	URL    string
	Models []string
	Slots  int
	Held   int
	Busy1m int
}

// Backends is what the sampler asks the runtime for each round.
type Backends func() []BackendLive

// Sampler publishes the newest NodeStats. Zero value is not usable; New
// it and Run it.
type Sampler struct {
	backends  Backends
	log       *slog.Logger
	livePath  string // ~/.remote/live.json (N6); empty = don't write
	connected func() []ConnState

	cur      atomic.Pointer[wire.NodeStats]
	byID     map[string]*adapterState
	fails    map[string]time.Time // per probe: last warned
	lastGPUs []gpuReading
}

// ConnState is one router connection's line in live.json (N6).
type ConnState struct {
	Name         string `json:"name"`
	Connected    bool   `json:"connected"`
	InFlightConn int    `json:"in_flight_conn"`
}

// New builds a sampler. backends is the runtime's per-backend view; live
// is the HomeDir for live.json ("" = skip the file); conns is the fleet's
// connection snapshot (may be nil).
func New(backends Backends, live string, conns func() []ConnState, log *slog.Logger) *Sampler {
	if log == nil {
		log = slog.Default()
	}
	return &Sampler{
		backends:  backends,
		log:       log,
		livePath:  live,
		connected: conns,
		byID:      map[string]*adapterState{},
		fails:     map[string]time.Time{},
	}
}

// Latest returns the newest stats object (nil before the first round).
func (s *Sampler) Latest() *wire.NodeStats { return s.cur.Load() }

// Run samples until ctx is done. One immediate round first — the first
// heartbeat after startup should already carry the machine.
func (s *Sampler) Run(ctx context.Context) {
	s.round(ctx)
	t := time.NewTicker(SampleEvery)
	defer t.Stop()
	gpu := time.NewTicker(GPUEvery)
	defer gpu.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.round(ctx)
		case <-gpu.C:
			s.lastGPUs = probeGPUs(ctx)
			s.round(ctx)
		}
	}
}

// round assembles one stats object from the platform probes and every
// backend adapter, publishes it, and (N6) writes live.json.
func (s *Sampler) round(ctx context.Context) {
	stats := &wire.NodeStats{At: time.Now().UTC().Format(time.RFC3339)}

	if load1, cores, util := probeCPU(ctx); cores > 0 || load1 > 0 {
		stats.CPU = &wire.CPUStats{Load1: load1, Cores: cores, Util: util}
	}
	if total, used, avail, ok := probeMem(ctx); ok {
		stats.Mem = &wire.MemStats{TotalMB: total, UsedMB: used, AvailableMB: avail}
	}
	if s.lastGPUs == nil {
		s.lastGPUs = probeGPUs(ctx)
	}
	for _, g := range s.lastGPUs {
		stats.GPUs = append(stats.GPUs, wire.GPUStats{
			Index: g.Index, Util: g.Util, MemUsedMB: g.MemUsedMB,
			MemTotalMB: g.MemTotalMB, PowerW: g.PowerW, TempC: g.TempC,
		})
	}

	for _, b := range s.backends() {
		agent := wire.BackendStats{
			ID: b.ID, Kind: b.Kind, Source: "agent",
			Models: b.Models, Slots: b.Slots, Held: b.Held, Busy1m: b.Busy1m,
		}
		if adapter := s.adapterFor(b); adapter != nil {
			adapter.slots = b.Slots
			runCtx, cancel := context.WithTimeout(ctx, probeWait)
			running, waiting, kv, tps, prefix, mem := adapter.read(runCtx)
			cancel()
			if running != nil || waiting != nil || kv != nil || mem != nil {
				agent.Source = "metrics"
				if b.Kind == "ollama" {
					agent.Source = "api"
				}
			}
			agent.Running, agent.Waiting, agent.KV = running, waiting, kv
			agent.DecodeTPS, agent.PrefixHit, agent.MemUsedMB = tps, prefix, mem
		}
		stats.Backends = append(stats.Backends, agent)
	}

	s.cur.Store(stats)
	s.writeLive(ctx, stats)
}

func (s *Sampler) adapterFor(b BackendLive) *adapterState {
	if b.URL == "" {
		return nil
	}
	a := s.byID[b.ID]
	if a == nil {
		a = newAdapter(b.ID, b.Kind, b.URL)
		s.byID[b.ID] = a
	}
	return a
}

// writeLive is N6's file: the stats object plus per-router connection
// state, 0600, atomic rename. A failure here is silent — the CLI reads
// whatever the last good write left.
func (s *Sampler) writeLive(ctx context.Context, stats *wire.NodeStats) {
	if s.livePath == "" {
		return
	}
	doc := struct {
		*wire.NodeStats
		Conns []ConnState `json:"conns,omitempty"`
	}{NodeStats: stats}
	if s.connected != nil {
		doc.Conns = s.connected()
	}
	b, err := marshalIndent(doc)
	if err != nil {
		return
	}
	tmp := s.livePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		s.noteFailure("live.json", err)
		return
	}
	if err := os.Rename(tmp, s.livePath); err != nil {
		s.noteFailure("live.json", err)
	}
}

// noteFailure rate-limits per-probe warnings to once per 10 minutes.
func (s *Sampler) noteFailure(probe string, err error) {
	if last, ok := s.fails[probe]; ok && time.Since(last) < logEvery {
		return
	}
	s.fails[probe] = time.Now()
	s.log.Warn("live probe failed — field dropped until it answers", "probe", probe, "err", err)
}

// --- shared helpers the platform files use ---

// output runs a command with a timeout and returns trimmed stdout.
func output(ctx context.Context, wait time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func numCPU() int { return runtime.NumCPU() }

// nvidiaGPUs is the NVIDIA reader every non-darwin platform shares:
// nvidia-smi --query-gpu=index,utilization.gpu,memory.used,memory.total,
// power.draw,temperature.gpu --format=csv,noheader,nounits — with `[N/A]`
// (GB10's unified memory answers no GPU memory figure) becoming nil.
func nvidiaGPUs(ctx context.Context) []gpuReading {
	bin := "nvidia-smi"
	if runtime.GOOS == "windows" {
		bin = "nvidia-smi.exe" // when on PATH; otherwise GPU util stays nil
	}
	out, err := output(ctx, 3e9, bin,
		"--query-gpu=index,utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil
	}
	return parseNvidiaCSV(out)
}

// parseNvidiaCSV reads one line per GPU. Position 1 is util, 2/3 memory,
// 4 power, 5 temp — true for our query order AND for captures that lead
// with the GPU name (the plan's GB10 line), where the non-numeric first
// field just fails the index parse and the loop index stands in.
func parseNvidiaCSV(out string) []gpuReading {
	var gpus []gpuReading
	for i, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 6 {
			continue
		}
		g := gpuReading{Index: i}
		if idx, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil {
			g.Index = idx
		}
		if v, ok := csvFloat(fields[1]); ok {
			v := clamp01(v / 100)
			g.Util = &v
		}
		if v, ok := csvFloat(fields[2]); ok {
			v := int64(v)
			g.MemUsedMB = &v
		}
		if v, ok := csvFloat(fields[3]); ok {
			v := int64(v)
			g.MemTotalMB = &v
		}
		if v, ok := csvFloat(fields[4]); ok {
			g.PowerW = &v
		}
		if v, ok := csvFloat(fields[5]); ok {
			g.TempC = &v
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// csvFloat parses one CSV field; "[N/A]" and friends are not numbers.
func csvFloat(field string) (float64, bool) {
	field = strings.TrimSpace(field)
	if field == "" || strings.HasPrefix(field, "[") {
		return 0, false
	}
	v, err := strconv.ParseFloat(field, 64)
	return v, err == nil
}

func marshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

var _ = filepath.Join

// extractInt finds `"key"=value` in an ioreg-style stats blob (darwin's
// PerformanceStatistics; kept platform-free so the parser test compiles
// everywhere).
func extractInt(s, key string) (int, bool) {
	i := strings.Index(s, key)
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimLeft(s[i+len(key):], " =")
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	v, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return v, true
}
