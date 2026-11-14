package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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

type peerPath struct {
	Relay   string `json:"relay,omitempty"`
	CurAddr string `json:"cur_addr,omitempty"`
	Error   string `json:"error,omitempty"`
}

type pathProber interface {
	PeerPath(ctx context.Context, conn net.Conn) (relay, curAddr string, err error)
}

func probePath(ctx context.Context, t any, conn net.Conn) *peerPath {
	p, ok := t.(pathProber)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	relay, curAddr, err := p.PeerPath(ctx, conn)
	if err != nil {
		return &peerPath{Error: err.Error()}
	}
	return &peerPath{Relay: relay, CurAddr: curAddr}
}
