package observe

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func ProbePID(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return "unknown"
	}
	err = proc.Signal(syscall.Signal(0))
	switch {
	case err == nil:
		return "running"
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return "dead"
	case errors.Is(err, syscall.EPERM):
		return "running"
	default:
		return "unknown"
	}
}

const procUserHz = 100

func selfProcStat() ([]string, bool) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 24 {
		return nil, false
	}
	return fields, true
}

func SelfCPUSeconds() (float64, bool) {
	fields, ok := selfProcStat()
	if !ok {
		return 0, false
	}
	user, err := strconv.ParseFloat(fields[13], 64)
	if err != nil {
		return 0, false
	}
	system, err := strconv.ParseFloat(fields[14], 64)
	if err != nil {
		return 0, false
	}
	return (user + system) / procUserHz, true
}

func SelfRSSBytes() (float64, bool) {
	fields, ok := selfProcStat()
	if !ok {
		return 0, false
	}
	pages, err := strconv.ParseFloat(fields[23], 64)
	if err != nil {
		return 0, false
	}
	return pages * float64(os.Getpagesize()), true
}
