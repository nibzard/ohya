// Operational-limit probes for the spike: resident memory of this process
// and the size of the node state directory.
package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// readRSS returns the resident set size of this process in bytes, or 0 when
// it cannot be read (non-Linux).
func readRSS() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kib * 1024
	}
	return 0
}

// dirSize sums the byte size of every regular file under dir. A missing
// directory is an empty state, not an error.
func dirSize(dir string) int64 {
	if dir == "" {
		return 0
	}
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return total
	}
	return total
}
