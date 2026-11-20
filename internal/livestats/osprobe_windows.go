// osprobe_windows.go: memory via GlobalMemoryStatusEx, CPU util from a
// GetSystemTimes delta — both through x/sys/windows' lazy kernel32
// bindings (no cgo, the cross-built releases stay honest), NVIDIA via
// nvidia-smi.exe when on PATH. Untested against a live Windows box by
// this implementer — the fleet's Windows node is the box that will
// verify it; a failing probe drops its field, never zeroes it.
package livestats

import (
	"context"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMem   = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSysTimes = kernel32.NewProc("GetSystemTimes")
)

// memoryStatusEx is MEMORY_STATUS_EX (the struct kernel32 fills).
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

// filetime is the 100ns-tick pair GetSystemTimes writes.
type filetime struct{ Lo, Hi uint32 }

func (f filetime) ticks() uint64 { return uint64(f.Hi)<<32 | uint64(f.Lo) }

var (
	winMu     sync.Mutex
	winLast   cpuTimes64
	winLastAt time.Time
)

type cpuTimes64 struct{ total, idle uint64 }

// probeCPU: GetSystemTimes kernel+user (busy) vs idle, delta over the
// interval; the first call primes the delta.
func probeCPU(ctx context.Context) (load1 float64, cores int, util *float64) {
	cores = numCPU()
	if err := procGetSysTimes.Find(); err != nil {
		return 0, cores, nil
	}
	var idle, kernel, user filetime
	r1, _, _ := procGetSysTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r1 == 0 {
		return 0, cores, nil
	}
	busy := kernel.ticks() + user.ticks()
	idleTicks := idle.ticks()

	winMu.Lock()
	defer winMu.Unlock()
	now := time.Now()
	if winLastAt.IsZero() || busy <= winLast.total {
		winLast, winLastAt = cpuTimes64{busy, idleTicks}, now
		return 0, cores, nil
	}
	dBusy := busy - winLast.total
	dIdle := idleTicks - winLast.idle
	winLast, winLastAt = cpuTimes64{busy, idleTicks}, now
	if dBusy+dIdle > 0 {
		v := clamp01(float64(dBusy) / float64(dBusy+dIdle))
		util = &v
	}
	return 0, cores, util
}

// probeMem: GlobalMemoryStatusEx (the plan's shape).
func probeMem(ctx context.Context) (totalMB, usedMB, availableMB int64, ok bool) {
	if err := procGlobalMem.Find(); err != nil {
		return 0, 0, 0, false
	}
	var stat memoryStatusEx
	stat.Length = uint32(unsafe.Sizeof(stat))
	r1, _, _ := procGlobalMem.Call(uintptr(unsafe.Pointer(&stat)))
	if r1 == 0 {
		return 0, 0, 0, false
	}
	totalMB = int64(stat.TotalPhys) / 1048576
	availableMB = int64(stat.AvailPhys) / 1048576
	usedMB = totalMB - availableMB
	return totalMB, usedMB, availableMB, totalMB > 0
}

// probeGPUs: nvidia-smi.exe when on PATH; otherwise nil.
func probeGPUs(ctx context.Context) []gpuReading { return nvidiaGPUs(ctx) }
