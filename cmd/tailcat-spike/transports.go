// Transport construction for the spike: the embedded Tailcat adapter behind
// the interfaces, and a plain TCP baseline for comparison on the same path.
package main

import (
	"context"
	"fmt"
	"net"

	"github.com/nibzard/ohya/internal/transport"
	"github.com/nibzard/ohya/internal/transport/tailcat"
)

// tailcatOptions validates and maps spike flags to adapter options for the
// given role. The hostname defaults to the role's spike name. Devices are
// durable by default: --ephemeral opts into server-side expiry at
// disconnect, which orphans the local state (recorded in ADR 0002).
func tailcatOptions(f flags, hub bool) (tailcat.Options, error) {
	hostname := f.Hostname
	if hostname == "" {
		if hub {
			hostname = "spike-hub"
		} else {
			hostname = "spike-device"
		}
	}
	if f.StateDir == "" || f.Control == "" {
		return tailcat.Options{}, fmt.Errorf("--state and --control are required for the tailcat transport")
	}
	return tailcat.Options{
		Hostname:   hostname,
		AuthKey:    f.AuthKey,
		ControlURL: f.Control,
		StateDir:   f.StateDir,
		Port:       f.Port,
		Ephemeral:  f.Ephemeral,
	}, nil
}

// newHubTransport builds the hub side of the selected transport.
func newHubTransport(f flags) (transport.Hub, error) {
	switch f.Transport {
	case "tailcat":
		opts, err := tailcatOptions(f, true)
		if err != nil {
			return nil, err
		}
		return tailcat.NewHub(opts)
	case "tcp":
		return newTCPHub()
	default:
		return nil, fmt.Errorf("unknown transport %q", f.Transport)
	}
}

// newDeviceTransport builds the device side of the selected transport.
func newDeviceTransport(f flags) (transport.Device, error) {
	switch f.Transport {
	case "tailcat":
		opts, err := tailcatOptions(f, false)
		if err != nil {
			return nil, err
		}
		return tailcat.NewDevice(opts)
	case "tcp":
		return tcpDevice{}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q", f.Transport)
	}
}

// tcpHub is the baseline hub: a loopback TCP listener.
type tcpHub struct {
	ln net.Listener
}

func newTCPHub() (*tcpHub, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	return &tcpHub{ln: ln}, nil
}

func (h *tcpHub) Listen(context.Context) (net.Listener, error) { return h.ln, nil }

func (h *tcpHub) Addr() string { return h.ln.Addr().String() }

func (h *tcpHub) Close() error { return h.ln.Close() }

// tcpDevice is the baseline device dialer.
type tcpDevice struct{}

func (tcpDevice) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp dial %s: %w", addr, err)
	}
	return conn, nil
}

func (tcpDevice) Close() error { return nil }
