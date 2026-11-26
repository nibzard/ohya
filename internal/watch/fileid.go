package watch

import (
	"io/fs"
	"strconv"
	"syscall"
	"time"
)

type DirStamp struct {
	Dev       uint64
	Ino       uint64
	Mode      fs.FileMode
	Size      int64
	ModTimeNS int64
	CTimeNS   int64
}

func StatDirStamp(info fs.FileInfo) (DirStamp, bool) {
	dev, ino, ok := rawFileIdentity(info)
	if !ok {
		return DirStamp{}, false
	}
	return DirStamp{
		Dev:       dev,
		Ino:       ino,
		Mode:      info.Mode(),
		Size:      info.Size(),
		ModTimeNS: info.ModTime().UnixNano(),
		CTimeNS:   platformFileChangeTime(info),
	}, true
}

func (s DirStamp) StableAt(now time.Time) bool {
	cutoff := now.Add(-2 * time.Second).UnixNano()
	if s.ModTimeNS > cutoff {
		return false
	}
	if s.CTimeNS > 0 && s.CTimeNS > cutoff {
		return false
	}
	return true
}

func rawFileIdentity(info fs.FileInfo) (uint64, uint64, bool) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino), true
	}
	return 0, 0, false
}

func formatFileIdentity(dev, ino uint64) string {
	var buf [48]byte
	b := append(buf[:0], "dev:"...)
	b = strconv.AppendUint(b, dev, 10)
	b = append(b, "/ino:"...)
	b = strconv.AppendUint(b, ino, 10)
	return string(b)
}

func platformFileIdentity(info fs.FileInfo) string {
	dev, ino, ok := rawFileIdentity(info)
	if !ok {
		return ""
	}
	return formatFileIdentity(dev, ino)
}
