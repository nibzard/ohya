package address

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/tailscale/tailcat"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

const (
	PathDirect  = "direct"
	PathRelay   = "relay"
	PathUnknown = "unknown"
)

const pathPingAttempts = 3

var ErrInvalidAddress = errors.New("address: not a valid hub address")

func ValidHubAddr(addr string) bool {
	_, err := tailcat.ParseAddr(tailcat.Addr(addr))
	return err == nil
}

type PathReport struct {
	Path        string
	RegionCode  string
	RegionID    int64
	RegionName  string
	RoundTripMS float64
}

func (d *Device) PathInfo(ctx context.Context, addr string) (PathReport, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return PathReport{}, net.ErrClosed
	}
	if d.addr != "" && d.addr != tailcat.Addr(addr) {
		d.mu.Unlock()
		return PathReport{}, errors.New("address: this device already dials another hub")
	}
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		d.mu.Unlock()
		return PathReport{}, fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}
	if d.addr == "" {
		d.addr = tailcat.Addr(addr)
		d.slot.client.Server = d.addr
	}
	slot := d.slot
	slot.dialing++
	d.mu.Unlock()

	ctx, done := d.operationContext(ctx)
	defer done()
	report := PathReport{Path: PathUnknown}
	var err error
	for range pathPingAttempts {
		var res *ipnstate.PingResult
		res, err = slot.client.DiscoPing(ctx)
		if err != nil {
			break
		}
		report = reportFromDisco(res)
		if report.Path == PathDirect {
			break
		}
	}
	d.mu.Lock()
	slot.dialing--
	if d.closed {
		toClose := d.releaseSlotLocked(slot)
		d.mu.Unlock()
		if toClose != nil {
			_ = toClose.Close()
		}
		return PathReport{Path: PathUnknown}, net.ErrClosed
	}
	if err == nil && d.slot == slot {
		d.started = true
	}
	toClose := d.releaseSlotLocked(slot)
	d.mu.Unlock()
	if toClose != nil {
		_ = toClose.Close()
	}
	if err != nil {
		return PathReport{Path: PathUnknown}, fmt.Errorf("address: measure the path to the hub: %w", err)
	}
	return report, nil
}

func reportFromDisco(res *ipnstate.PingResult) PathReport {
	report := PathReport{Path: PathUnknown, RoundTripMS: res.LatencySeconds * 1000}
	switch {
	case res.Endpoint != "":
		report.Path = PathDirect
	case res.DERPRegionID != 0:
		report.Path = PathRelay
		report.RegionID = int64(res.DERPRegionID)
		report.RegionCode = res.DERPRegionCode
	}
	return report
}

func (h *Hub) PeerPath(remote net.Addr) PathReport {
	if remote == nil {
		return PathReport{Path: PathUnknown}
	}
	h.mu.Lock()
	ready := h.started && !h.closed
	h.mu.Unlock()
	if !ready {
		return PathReport{Path: PathUnknown}
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return PathReport{Path: PathUnknown}
	}
	ip, ipErr := netip.ParseAddr(host)
	if ipErr != nil || !ip.Is6() || !strings.HasPrefix(ip.String(), tailcatULAPrefix) {
		return PathReport{Path: PathUnknown}
	}
	ip16 := ip.As16()
	for peerKey, peer := range h.srv.Status().Peer {
		want := peerTunnelAddr(peerKey)
		got := ip16
		if want != got {
			continue
		}
		if peer.CurAddr != "" {
			return PathReport{Path: PathDirect}
		}
		if peer.Relay != "" {
			return PathReport{Path: PathRelay, RegionCode: peer.Relay}
		}
		return PathReport{Path: PathUnknown}
	}
	return PathReport{Path: PathUnknown}
}

const tailcatULAPrefix = "fd7a:115c:a1e0:"

func peerTunnelAddr(k key.NodePublic) [16]byte {
	var a [16]byte
	a[0] = 0xfd
	a[1] = 0x7a
	a[2] = 0x11
	a[3] = 0x5c
	a[4] = 0xa1
	a[5] = 0xe0
	raw := k.Raw32()
	copy(a[6:], raw[:10])
	return a
}
