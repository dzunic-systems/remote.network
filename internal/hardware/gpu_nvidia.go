package hardware

import (
	"os/exec"
	"strconv"
	"strings"
)

// detectGPUsNvidia lists NVIDIA GPUs via nvidia-smi's CSV output. The CLI
// ships with the driver on both Linux and Windows, so this one parser
// serves both platforms.
func detectGPUsNvidia() []GPU {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,uuid,memory.total,driver_version",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []GPU
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, ", ")
		if len(f) < 5 {
			continue
		}
		g := GPU{Index: atoi(f[0]), Name: f[1], UUID: f[2], Vendor: "nvidia", DriverVersion: f[4]}
		if mb, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil {
			g.MemoryMB = mb
		}
		gpus = append(gpus, g)
	}
	return gpus
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}
