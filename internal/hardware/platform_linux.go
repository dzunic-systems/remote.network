//go:build linux

package hardware

import (
	"os"
	"strconv"
	"strings"
)

// platformDetails probes CPU model, memory and GPUs on Linux. GPU detection
// is nvidia-smi first (CUDA fleet), rocminfo best-effort after.
func platformDetails() (cpuModel string, totalMB, availMB int64, gpus []GPU, unified bool) {
	cpuModel = "unknown"
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
					cpuModel = strings.TrimSpace(parts[1])
					break
				}
			}
		}
	}

	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				continue
			}
			switch {
			case strings.HasPrefix(line, "MemTotal:"):
				totalMB = v / 1024
			case strings.HasPrefix(line, "MemAvailable:"):
				availMB = v / 1024
			}
		}
	}

	gpus = detectGPUsNvidia()
	return cpuModel, totalMB, availMB, gpus, false
}
