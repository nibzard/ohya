package address

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestPathInfoReportsMeasuredPath(t *testing.T) {
	t.Parallel()
	region := startLocalDERP(t)
	hub, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	remoteConns := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			remoteConns <- conn
		}
	}()
	defer hub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	device := NewDevice(DeviceOptions{})
	defer device.Close()
	report, err := device.PathInfo(ctx, hub.Addr())
	if err != nil {
		t.Fatalf("PathInfo: %v", err)
	}
	if report.Path == PathUnknown {
		t.Fatalf("PathInfo reported an unknown path over a live connection: %+v", report)
	}
	if report.Path == PathRelay && report.RegionCode == "" {
		t.Fatalf("relay path without a region code: %+v", report)
	}
	if report.RoundTripMS < 0 {
		t.Fatalf("negative round trip: %+v", report)
	}

	conn, err := device.Dial(ctx, hub.Addr())
	if err != nil {
		t.Fatalf("Dial after PathInfo: %v", err)
	}
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case hubConn := <-remoteConns:
		peer := hub.PeerPath(hubConn.RemoteAddr())
		if peer.Path == PathUnknown {
			t.Fatalf("PeerPath reported an unknown path for a connected peer: %+v", peer)
		}
		if peer.Path == PathRelay && peer.RegionCode == "" {
			t.Fatalf("PeerPath relay without a region code: %+v", peer)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the hub never accepted the dialed connection")
	}
	conn.Close()
}

func TestPathInfoRejectsInvalidAddress(t *testing.T) {
	device := NewDevice(DeviceOptions{})
	defer device.Close()
	if _, err := device.PathInfo(context.Background(), "tcnot-a-tailcat-address"); err == nil {
		t.Fatal("PathInfo must reject an invalid address")
	}
	if _, err := device.PathInfo(context.Background(), "tcnot-a-tailcat-address"); err == nil || strings.Contains(err.Error(), "tcnot-a-tailcat-address") {
		t.Fatalf("PathInfo echoed the address text: %v", err)
	}
}
