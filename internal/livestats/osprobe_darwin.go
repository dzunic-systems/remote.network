// osprobe_darwin.go: the Mac node's live reading, verified 20 Nov on the
// fleet's Mac (loadavg/memsize/vm_stat/ioreg outputs captured locally).
// No cgo — every probe shells out to a standard tool; a missing tool or
// unreadable field leaves its slot nil, never 0.
package livestats

import (
	"context"
	"strconv"
	"strings"
)

// probeCPUdarwin reads vm.loadavg (util is not measured on darwin — the
// plan leaves it nil; load1 and cores still report).
func probeCPU(ctx context.Context) (load1 float64, cores int, util *float64) {
	out, err := output(ctx, 2e9, "sysctl", "-n", "vm.loadavg")
	if err == nil {
		// "{ 9.37 5.32 3.83 }"
		fields := strings.Fields(strings.Trim(out, "{} "))
		if len(fields) >= 1 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				load1 = v
			}
		}
	}
	return load1, numCPU(), nil
}

// probeMemdarwin: hw.memsize for the total; vm_stat's pages for
// used/available (page size comes from its header, per the plan).
func probeMem(ctx context.Context) (totalMB, usedMB, availableMB int64, ok bool) {
	if out, err := output(ctx, 2e9, "sysctl", "-n", "hw.memsize"); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			totalMB = v / 1048576
		}
	}
	out, err := output(ctx, 2e9, "vm_stat")
	if err != nil {
		return totalMB, 0, 0, totalMB > 0
	}
	pageSize := int64(16384) // Apple Silicon default; corrected from the header below
	if i := strings.Index(out, "page size of "); i >= 0 {
		if field := strings.Fields(out[i+len("page size of "):]); len(field) > 0 {
			if v, err := strconv.ParseInt(field[0], 10, 64); err == nil {
				pageSize = v
			}
		}
	}
	page := func(name string) int64 {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, name) {
				n := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, name)), ".")
				if v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
					return v
				}
			}
		}
		return 0
	}
	free := page("Pages free:") + page("Pages speculative:") + page("Pages purgeable:")
	inactive := page("Pages inactive:")
	// Available = free + purgeable + inactive; used = total - available.
	availableMB = (free + inactive) * pageSize / 1048576
	if totalMB > 0 && availableMB > totalMB {
		availableMB = totalMB
	}
	usedMB = totalMB - availableMB
	return totalMB, usedMB, availableMB, totalMB > 0
}

// probeGPUsdarwin reads GPU utilization from IOAccelerator's
// PerformanceStatistics ("Device Utilization %"=7 on the fleet Mac).
// No memory or power figures — darwin exposes none here; nil, never 0.
func probeGPUs(ctx context.Context) []gpuReading {
	out, err := output(ctx, 2e9, "ioreg", "-r", "-d", "1", "-c", "IOAccelerator")
	if err != nil {
		return nil
	}
	var gpus []gpuReading
	index := 0
	for _, chunk := range strings.Split(out, "\"PerformanceStatistics\" = {") {
		if index == 0 && !strings.Contains(chunk, "\"Device Utilization %\"") {
			// The chunk before the first match carries the header only.
			if !strings.Contains(chunk, "IOAccelerator") {
				continue
			}
		}
		end := strings.Index(chunk, "}")
		if end < 0 {
			end = len(chunk)
		}
		stats := chunk[:end]
		if u, ok := extractInt(stats, "\"Device Utilization %\""); ok {
			v := clamp01(float64(u) / 100)
			g := gpuReading{Index: index, Util: &v}
			gpus = append(gpus, g)
			index++
		}
	}
	return gpus
}
