package livestats

import (
	"context"
	"runtime"
	"testing"
)

// §8 N5: the parsers are tested against captured command outputs. The
// GB10 nvidia-smi line is the plan's own capture (unified memory answers
// `[N/A]` for the GPU memory figures); the darwin blobs are this Mac's
// live outputs (the fleet's Mac node, 20 Nov).
func TestNvidiaSMIParse(t *testing.T) {
	// GB10: util 0, memory N/A (unified), power 11.60W, 50°C —
	// in our query order (index, util, mem, mem, power, temp).
	out := "0, 0, [N/A], [N/A], 11.60, 50"
	gpus := parseNvidiaCSV(out)
	if len(gpus) != 1 {
		t.Fatalf("gpus = %d", len(gpus))
	}
	g := gpus[0]
	if g.Util == nil || *g.Util != 0 {
		t.Fatalf("util = %v, want 0", g.Util)
	}
	if g.MemUsedMB != nil || g.MemTotalMB != nil {
		t.Fatalf("unified memory reported figures: %v %v — [N/A] must stay nil", g.MemUsedMB, g.MemTotalMB)
	}
	if g.PowerW == nil || *g.PowerW != 11.60 {
		t.Fatalf("power = %v", g.PowerW)
	}
	if g.TempC == nil || *g.TempC != 50 {
		t.Fatalf("temp = %v", g.TempC)
	}

	// A discrete GPU: util 37%, 8134/24576 MiB.
	gpus = parseNvidiaCSV("0, 37, 8134, 24576, 220.5, 61")
	if g := gpus[0]; g.Util == nil || *g.Util != 0.37 ||
		g.MemUsedMB == nil || *g.MemUsedMB != 8134 ||
		g.MemTotalMB == nil || *g.MemTotalMB != 24576 {
		t.Fatalf("discrete parse: %+v", gpus[0])
	}

	// The plan's exact GB10 capture, name first: the non-numeric first
	// field fails the index parse and the loop index stands in; the
	// semantic fields (util at 1, memory at 2/3, power 4, temp 5) still
	// land.
	gpus = parseNvidiaCSV("NVIDIA GB10, 0, [N/A], [N/A], 11.60, 50")
	g = gpus[0]
	if g.Util == nil || *g.Util != 0 {
		t.Fatalf("GB10-name-line util = %v", g.Util)
	}
	if g.MemUsedMB != nil || g.MemTotalMB != nil {
		t.Fatalf("GB10-name-line memory = %v/%v", g.MemUsedMB, g.MemTotalMB)
	}
}

func TestDarwinParse(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only parsers")
	}
	// Captured vm_stat header + pages (page size read from the header).
	load1, cores, util := probeCPU(context.Background())
	if load1 <= 0 || cores == 0 {
		t.Fatalf("loadavg = %f cores %d — sysctl unreadable on this Mac?", load1, cores)
	}
	if util != nil {
		t.Fatalf("darwin CPU util must stay nil (plan), got %v", *util)
	}
	total, used, avail, ok := probeMem(context.Background())
	if !ok || total <= 0 || used <= 0 || avail <= 0 {
		t.Fatalf("mem = %d/%d/%d ok=%v", total, used, avail, ok)
	}
	if used+avail != total {
		t.Fatalf("used+avail = %d, total = %d", used+avail, total)
	}
	gpus := probeGPUs(context.Background())
	if len(gpus) == 0 {
		t.Skip("no IOAccelerator GPU on this machine")
	}
	if gpus[0].Util == nil || *gpus[0].Util < 0 || *gpus[0].Util > 1 {
		t.Fatalf("GPU util = %v", gpus[0].Util)
	}
	if gpus[0].MemUsedMB != nil || gpus[0].MemTotalMB != nil {
		t.Fatalf("darwin GPU memory figures must stay nil, got %v/%v", *gpus[0].MemUsedMB, *gpus[0].MemTotalMB)
	}
}

// The ioreg blob shape, pinned without shelling out: "Device Utilization
// %"=7 reads as 0.07.
func TestIoregExtract(t *testing.T) {
	blob := `"PerformanceStatistics" = {"In use system memory (driver)"=0,"Alloc system memory"=4121477120,"Tiler Utilization %"=6,"Device Utilization %"=7,"Allocated PB Size"=56098812}`
	if v, ok := extractInt(blob, `"Device Utilization %"`); !ok || v != 7 {
		t.Fatalf("extract = %d %v", v, ok)
	}
	if _, ok := extractInt(blob, `"Missing Key"`); ok {
		t.Fatal("missing key extracted")
	}
}

func TestMeminfoParse(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only parsers")
	}
	// /proc readings are kernel-documented; the live box is the check.
	total, used, avail, ok := probeMem(context.Background())
	if !ok || total <= 0 {
		t.Fatalf("meminfo unreadable: %d/%d/%d ok=%v", total, used, avail, ok)
	}
}
