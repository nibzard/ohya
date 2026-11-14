// Operational-limit probes for the spike: resident memory of this process,
// the size of the node state directory, and how a peer connection is routed.
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

// peerPath records how a peer connection is routed at one moment: relay is
// the DERP region carrying the traffic, curAddr the direct endpoint in use.
// An empty curAddr with a non-empty relay is a relay-only path; a probe
// failure is recorded instead of hidden so results stay truthful.
type peerPath struct {
	Relay   string `json:"relay,omitempty"`
	CurAddr string `json:"cur_addr,omitempty"`
	Error   string `json:"error,omitempty"`
}

// pathProber is the optional transport capability behind peerPath, mirroring
// the whoiser pattern in hub.go. The Tailcat node implements it; the TCP
// baseline does not (there is no relay concept to report).
type pathProber interface {
	PeerPath(ctx context.Context, conn net.Conn) (relay, curAddr string, err error)
}

// probePath reports how conn's peer is currently routed when the transport
// implements pathProber, and nil when it does not. The caller holds the
// transport value through the transport.Hub or transport.Device interface,
// so the capability is discovered by type assertion.
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
