package observe

import "os"

func SelfCPUSeconds() (float64, bool) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	cpu, _, ok := parseProcStat(string(raw))
	return cpu, ok
}

func SelfRSSBytes() (float64, bool) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	_, pages, ok := parseProcStat(string(raw))
	if !ok {
		return 0, false
	}
	return pages * float64(os.Getpagesize()), true
}
