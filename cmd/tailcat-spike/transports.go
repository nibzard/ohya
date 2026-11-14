package main

import (
	"context"
	"fmt"
	"net"

	"github.com/nibzard/ohya/internal/transport"
	"github.com/nibzard/ohya/internal/transport/tailcat"
)

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
