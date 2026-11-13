package watch

import (
	"fmt"
	"io/fs"
	"syscall"
)

// platformFileIdentity returns a string that identifies the file
// incarnation on this platform: device and inode numbers on Unix. A rename
// keeps the identity; replacement or re-creation changes it. The empty
// string means the platform provides no identity. Then only the file size
// and the content fingerprint detect changes.
func platformFileIdentity(info fs.FileInfo) string {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("dev:%d/ino:%d", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}
