// Package hardware detects the machine's capability for advertisement to
// the router. Field shape follows an earlier prototype's
// SystemCapabilities/GPUInfo (proven in production there), minus Docker/Python/wallet fields this LLM
// node agent does not need, plus Apple-Silicon specifics.
package hardware

import (
	"os"
	"runtime"
	"time"
)

// GPU describes one detected accelerator.
type GPU struct {
	Index         int    `json:"index"`
	Name          string `json:"name"`
	Vendor        string `json:"vendor"` // apple | nvidia | amd | intel
	MemoryMB      int64  `json:"memory_mb"`
	UUID          string `json:"uuid,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
	MetalFamily   string `json:"metal_family,omitempty"` // darwin: "3", "4"
}

// Capability is the full advertisement blob stored as JSONB on the router.
type Capability struct {
	Platform          string `json:"platform"`
	Arch              string `json:"arch"`
	KernelVersion     string `json:"kernel_version"`
	Hostname          string `json:"hostname"`
	CPUModel          string `json:"cpu_model"`
	CPUCores          int    `json:"cpu_cores"` // logical; cores==threads is a documented simplification
	CPUThreads        int    `json:"cpu_threads"`
	TotalMemoryMB     int64  `json:"total_memory_mb"`
	AvailableMemoryMB int64  `json:"available_memory_mb"`
	TotalDiskMB       int64  `json:"total_disk_mb"`
	AvailableDiskMB   int64  `json:"available_disk_mb"`
	UnifiedMemory     bool   `json:"unified_memory"` // Apple Silicon: GPU shares RAM
	GPUs              []GPU  `json:"gpus,omitempty"`
	DetectedAt        string `json:"detected_at"`
}

// Detect gathers everything, best-effort per field: a failed probe yields
// "unknown"/0, never an error — a node with unreadable GPU info is still
// schedulable for CPU inference.
func Detect(dataDir string) *Capability {
	c := &Capability{
		Platform:      runtime.GOOS,
		Arch:          runtime.GOARCH,
		CPUCores:      runtime.NumCPU(),
		CPUThreads:    runtime.NumCPU(),
		KernelVersion: kernelVersion(),
		DetectedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if h, err := os.Hostname(); err == nil {
		c.Hostname = h
	}
	c.CPUModel, c.TotalMemoryMB, c.AvailableMemoryMB, c.GPUs, c.UnifiedMemory = platformDetails()
	c.TotalDiskMB, c.AvailableDiskMB = diskInfo(dataDir)
	if c.GPUs == nil {
		c.GPUs = []GPU{} // stable JSON shape: "gpus":[] not null
	}
	return c
}
