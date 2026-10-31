//go:build unix

package hardware

import (
	"os/exec"
	"strings"
	"syscall"
)

// kernelVersion is a best-effort `uname -r`; Windows has its own.
func kernelVersion() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// diskInfo reports totals for the filesystem holding dataDir (where models
// will live), not the system volume.
func diskInfo(dataDir string) (totalMB, availMB int64) {
	if dataDir == "" {
		dataDir = "."
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dataDir, &fs); err != nil {
		return 0, 0
	}
	const mb = 1024 * 1024
	return int64(fs.Blocks) * int64(fs.Bsize) / mb, int64(fs.Bavail) * int64(fs.Bsize) / mb
}
