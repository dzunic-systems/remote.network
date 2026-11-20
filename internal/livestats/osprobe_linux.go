// osprobe_linux.go: /proc readings and nvidia-smi. Formats are the
// kernel's documented ones; the parsers are tested against representative
// captures including GB10's unified-memory `[N/A]` GPU line.
package livestats

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cpuDelta keeps the last /proc/stat snapshot for the util delta.
var (
	cpuMu     sync.Mutex
	cpuLast   cpuTimes
	cpuLastAt time.Time
)

type cpuTimes struct{ total, idle float64 }

// probeCPU: loadavg for load1, /proc/stat delta for util (0..1).
func probeCPU(ctx context.Context) (load1 float64, cores int, util *float64) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 1 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				load1 = v
			}
		}
	}
	cores = numCPU()

	if b, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			fields := strings.Fields(line)[1:]
			if len(fields) < 5 {
				break
			}
			var total, idle float64
			for i, f := range fields {
				v, err := strconv.ParseFloat(f, 64)
				if err != nil {
					continue
				}
				total += v
				if i == 3 { // idle
					idle += v
				}
				if i == 4 { // iowait counts as not-busy too
					idle += v
				}
			}
			cpuMu.Lock()
			defer cpuMu.Unlock()
			if cpuLastAt.IsZero() || total <= cpuLast.total {
				cpuLast, cpuLastAt = cpuTimes{total, idle}, time.Now()
				break
			}
			dTotal := total - cpuLast.total
			dIdle := idle - cpuLast.idle
			cpuLast, cpuLastAt = cpuTimes{total, idle}, time.Now()
			if dTotal > 0 {
				v := clamp01(1 - dIdle/dTotal)
				util = &v
			}
			break
		}
	}
	return load1, cores, util
}

// probeMem: /proc/meminfo MemTotal/MemAvailable (kB).
func probeMem(ctx context.Context) (totalMB, usedMB, availableMB int64, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0, false
	}
	kb := func(name string) int64 {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, name) {
				fields := strings.Fields(strings.TrimPrefix(line, name))
				if len(fields) >= 1 {
					if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
						return v
					}
				}
			}
		}
		return 0
	}
	totalKB := kb("MemTotal:")
	availKB := kb("MemAvailable:")
	if totalKB <= 0 {
		return 0, 0, 0, false
	}
	totalMB = totalKB / 1024
	if availKB > 0 {
		availableMB = availKB / 1024
		usedMB = totalMB - availableMB
	}
	return totalMB, usedMB, availableMB, true
}

// probeGPUs: nvidia-smi; a machine without it (CPU-only node) reports no
// GPUs at all.
func probeGPUs(ctx context.Context) []gpuReading { return nvidiaGPUs(ctx) }
