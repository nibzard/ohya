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

func parseProcStat(raw string) (cpuSeconds float64, rssPages float64, ok bool) {
	rparen := strings.LastIndexByte(raw, ')')
	if rparen < 0 || rparen+1 >= len(raw) {
		return 0, 0, false
	}
	after := strings.Fields(raw[rparen+1:])
	if len(after) < 22 {
		return 0, 0, false
	}
	user, err := strconv.ParseFloat(after[11], 64)
	if err != nil {
		return 0, 0, false
	}
	system, err := strconv.ParseFloat(after[12], 64)
	if err != nil {
		return 0, 0, false
	}
	pages, err := strconv.ParseFloat(after[21], 64)
	if err != nil {
		return 0, 0, false
	}
	return (user + system) / procUserHz, pages, true
}
