//go:build darwin

package hardware

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// platformDetails probes CPU model, memory and GPUs on macOS. On Apple
// Silicon the SoC is the GPU: name = chip type, memory is unified, and the
// Metal family comes from SPDisplaysDataType (verified against Mac14,9 /
// M2 Pro output).
func platformDetails() (cpuModel string, totalMB, availMB int64, gpus []GPU, unified bool) {
	cpuModel = sysctl("machdep.cpu.brand_string")
	if cpuModel == "" || cpuModel == "unknown" {
		cpuModel = sysctl("hw.model")
	}

	if v := sysctlBytes("hw.memsize"); v > 0 {
		totalMB = v / (1024 * 1024)
	}
	availMB = availableMemoryMB()
	unified = isAppleSilicon(cpuModel)

	if chip, gpuName, metal := probeSPHardware(); chip != "" || gpuName != "" {
		if chip != "" && (cpuModel == "" || cpuModel == "unknown") {
			cpuModel = chip
		}
		g := GPU{
			Index:       0,
			Name:        firstNonEmpty(gpuName, chip, cpuModel),
			Vendor:      "apple",
			MetalFamily: metal,
		}
		if unified {
			// Unified memory: the GPU can address (most of) system RAM.
			g.MemoryMB = totalMB
		}
		gpus = []GPU{g}
	} else if unified {
		// system_profiler unavailable (headless CI); fall back to sysctl only.
		gpus = []GPU{{Index: 0, Name: cpuModel, Vendor: "apple", MemoryMB: totalMB}}
	}
	return cpuModel, totalMB, availMB, gpus, unified
}

func isAppleSilicon(cpuModel string) bool {
	if cpuModel == "" {
		// arm64 macOS is Apple Silicon in practice.
		return strings.Contains(sysctl("hw.optional.arm64"), "1")
	}
	return strings.Contains(strings.ToLower(cpuModel), "apple")
}

func sysctl(name string) string {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func sysctlBytes(name string) int64 {
	s := sysctl(name)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// availableMemoryMB approximates free memory from vm_stat page counts
// (free + speculative + inactive + purgeable). Format on macOS 26:
// "Pages free:                               13869."
func availableMemoryMB() int64 {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0
	}
	pageSize := int64(4096)
	if ps := sysctlBytes("hw.pagesize"); ps > 0 {
		pageSize = ps
	}
	re := regexp.MustCompile(`Pages (free|speculative|inactive|purgeable):\s+(\d+)\.`)
	pages := int64(0)
	for _, m := range re.FindAllSubmatch(out, -1) {
		if v, err := strconv.ParseInt(string(m[2]), 10, 64); err == nil {
			pages += v
		}
	}
	return pages * pageSize / (1024 * 1024)
}

// probeSPHardware parses `system_profiler -json SPHardwareDataType
// SPDisplaysDataType`; returns (chipType, gpuName, metalFamily).
func probeSPHardware() (chip, gpu, metal string) {
	out, err := exec.Command("system_profiler", "-json", "SPHardwareDataType", "SPDisplaysDataType").Output()
	if err != nil {
		return "", "", ""
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", "", ""
	}

	var hw []map[string]any
	if b, ok := raw["SPHardwareDataType"]; ok && json.Unmarshal(b, &hw) == nil && len(hw) > 0 {
		if s, ok := hw[0]["chip_type"].(string); ok {
			chip = s
		}
	}

	var disp []map[string]any
	if b, ok := raw["SPDisplaysDataType"]; ok && json.Unmarshal(b, &disp) == nil && len(disp) > 0 {
		// On Apple Silicon the GPU is the top-level _name (== chip).
		if s, ok := disp[0]["_name"].(string); ok && !strings.HasPrefix(s, "spdisplays") {
			gpu = s
		}
		if s, ok := disp[0]["spdisplays_mtlgpufamilysupport"].(string); ok {
			metal = strings.TrimPrefix(s, "spdisplays_metal")
		}
	}
	return chip, gpu, metal
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
