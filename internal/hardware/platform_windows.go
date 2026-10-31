//go:build windows

package hardware

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// memoryStatusEx mirrors MEMORYSTATUSEX; x/sys/windows does not wrap
// GlobalMemoryStatusEx, so it goes through kernel32 directly.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

// platformDetails probes CPU model, memory and GPUs on Windows: CPU name
// and Windows build from the registry (kernel-written, no subprocess),
// memory via GlobalMemoryStatusEx, NVIDIA GPUs via the shared nvidia-smi
// parser. AMD/Intel discrete GPU detection is a post-MVP nicety; such
// nodes still schedule fine for CPU inference or an Ollama backend.
func platformDetails() (cpuModel string, totalMB, availMB int64, gpus []GPU, unified bool) {
	cpuModel = "unknown"
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("ProcessorNameString"); err == nil && strings.TrimSpace(s) != "" {
			cpuModel = strings.TrimSpace(s)
		}
		k.Close()
	}

	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r1, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r1 != 0 {
		const mb = 1024 * 1024
		totalMB = int64(ms.TotalPhys) / mb
		availMB = int64(ms.AvailPhys) / mb
	}

	gpus = detectGPUsNvidia()
	return cpuModel, totalMB, availMB, gpus, false
}

// kernelVersion reports the Windows build ("10.0.26100 (24H2)-style") from
// the CurrentVersion key; a missing key degrades to "unknown".
func kernelVersion() string {
	build, display := "unknown", ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("CurrentBuildNumber"); err == nil && s != "" {
			build = s
		}
		if s, _, err := k.GetStringValue("DisplayVersion"); err == nil {
			display = s
		}
		k.Close()
	}
	if build == "unknown" {
		return build
	}
	if display != "" {
		return "win build " + build + " (" + display + ")"
	}
	return "win build " + build
}

// diskInfo reports free space on the volume holding dataDir (where models
// will live) via GetDiskFreeSpaceEx.
func diskInfo(dataDir string) (totalMB, availMB int64) {
	if dataDir == "" {
		dataDir = "."
	}
	var freeCaller, total, free uint64
	p, err := windows.UTF16PtrFromString(dataDir)
	if err != nil {
		return 0, 0
	}
	if err := windows.GetDiskFreeSpaceEx(p, &freeCaller, &total, &free); err != nil {
		return 0, 0
	}
	const mb = 1024 * 1024
	return int64(total) / mb, int64(free) / mb
}
