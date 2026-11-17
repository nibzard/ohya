package address

import (
	"context"
	"testing"
	"time"
)

func TestDeviceDrainNeverDialed(t *testing.T) {
	device := NewDevice(DeviceOptions{})
	defer device.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := device.Drain(ctx); err != nil {
		t.Fatalf("Drain on a never-dialed device = %v, want nil", err)
	}
}

func TestDeviceDrainAfterFailedDial(t *testing.T) {
	region := startLocalDERP(t)
	hub, err := NewHub(HubOptions{StateDir: t.TempDir(), Region: region})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveEcho(t, ln)
	addr := hub.Addr()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	device := NewDevice(DeviceOptions{})
	defer device.Close()
	conn, err := device.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	exchange(t, conn, "before the hub goes away")
	if err := conn.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := device.Drain(drainCtx); err != nil {
		t.Fatalf("Drain after a completed exchange = %v, want nil", err)
	}

	if err := hub.Close(); err != nil {
		t.Fatalf("close hub: %v", err)
	}
	failCtx, failCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer failCancel()
	if _, err := device.Dial(failCtx, addr); err == nil {
		t.Fatalf("dial after the hub closed must fail")
	}
	resetCtx, resetCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer resetCancel()
	if err := device.Drain(resetCtx); err != nil {
		t.Fatalf("Drain after a failed dial = %v, want nil", err)
	}
}
