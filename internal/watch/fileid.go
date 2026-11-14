package watch

import (
	"fmt"
	"io/fs"
	"syscall"
)

func platformFileIdentity(info fs.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("dev:%d/ino:%d", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}
