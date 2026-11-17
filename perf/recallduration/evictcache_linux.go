package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func evictCache(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
		f.Close()
	}
}
