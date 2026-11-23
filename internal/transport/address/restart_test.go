package address

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDeviceRedialAfterHubRestart(t *testing.T) {
	t.Parallel()
	region := startLocalDERP(t)
	state := t.TempDir()

	hubLog := &logSink{}
	hub, err := NewHub(HubOptions{StateDir: state, Region: region, Logf: hubLog.Logf})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	ln, err := hub.Listen(context.Background())
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveEcho(t, ln)
	addr := hub.Addr()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	deviceLog := &logSink{}
	device := NewDevice(DeviceOptions{Logf: deviceLog.Logf})
	conn, err := device.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	exchange(t, conn, "before restart")
	conn.Close()

	hub.Close()
	ln.Close()
	t.Logf("hub closed; logs so far:\n%s", hubLog.all())

	hub2, err := NewHub(HubOptions{StateDir: state, Region: region, Logf: hubLog.Logf})
	if err != nil {
		t.Fatalf("NewHub after restart: %v", err)
	}
	ln2, err := hub2.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen after restart: %v", err)
	}
	serveEcho(t, ln2)
	defer hub2.Close()

	dialCtx, dialCancel := context.WithTimeout(ctx, 90*time.Second)
	defer dialCancel()
	var conn2 net.Conn
	var dialErr error
	for range 3 {
		conn2, dialErr = device.Dial(dialCtx, addr)
		if dialErr == nil {
			break
		}
		t.Logf("dial after restart failed, retrying: %v", dialErr)
		time.Sleep(time.Second)
	}
	if dialErr != nil {
		t.Fatalf("Dial after restart: %v\nhub logs:\n%s\ndevice logs:\n%s", err, hubLog.all(), deviceLog.all())
	}
	exchange(t, conn2, "after restart")
	conn2.Close()
	device.Close()
	if strings.Contains(deviceLog.all(), addr) {
		t.Fatalf("address leaked in device logs")
	}
}
