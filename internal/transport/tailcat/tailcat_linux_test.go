package tailcat

import (
	"context"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/types/logger"
)

func TestNodeSocketsFollowHostRouting(t *testing.T) {
	netns.SetEnabled(true)
	t.Cleanup(func() { netns.SetEnabled(true) })
	if _, err := NewDevice(Options{Hostname: "device", ControlURL: "https://100.64.0.1:9417", StateDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	conn, err := netns.Listener(logger.Discard, netmon.NewStatic()).ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := conn.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var device string
	var mark int
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		device, sockErr = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		if sockErr != nil {
			return
		}
		mark, sockErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
	}); err != nil {
		t.Fatal(err)
	}
	if sockErr != nil && sockErr != syscall.ENOPROTOOPT {
		t.Fatal(sockErr)
	}
	if device != "" || mark != 0 {
		t.Fatalf("node socket escapes host routing: bound to device %q, mark %#x", device, mark)
	}
}
