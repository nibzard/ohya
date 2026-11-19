package watch

import (
	"io/fs"
	"syscall"
)

func platformFileChangeTime(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}
