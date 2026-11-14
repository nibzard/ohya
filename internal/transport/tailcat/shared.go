package tailcat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/proxy"

	"github.com/nibzard/ohya/internal/durablefile"
	"github.com/nibzard/ohya/internal/transport"
)

type sharedNode interface {
	transport.Device
	proxy() (string, string, error)
}

type SharedDevice struct {
	options Options
	lock    *os.File
	path    string
	newNode func(Options) (sharedNode, error)

	mu        sync.Mutex
	owned     bool
	closed    bool
	published bool
	node      sharedNode
}

type proxyEndpoint struct {
	Version    int    `json:"version"`
	Hostname   string `json:"hostname"`
	ControlURL string `json:"control_url"`
	Address    string `json:"address"`
	Password   string `json:"password"`
}

var _ transport.Device = (*SharedDevice)(nil)

func NewSharedDevice(options Options) (*SharedDevice, error) {
	if err := options.validate(false); err != nil {
		return nil, err
	}
	if options.Ephemeral {
		return nil, fmt.Errorf("tailcat: a shared device must use a durable identity")
	}
	if err := os.MkdirAll(options.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tailcat: create shared state directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(options.StateDir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("tailcat: open device ownership lock: %w", err)
	}
	return &SharedDevice{
		options: options,
		lock:    lock,
		path:    filepath.Join(options.StateDir, "proxy.json"),
		newNode: func(o Options) (sharedNode, error) { return NewDevice(o) },
	}, nil
}

func (n *Node) proxy() (string, string, error) {
	address, password, _, err := n.srv.Loopback()
	return address, password, err
}

func (s *SharedDevice) Dial(ctx context.Context, addr string) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, net.ErrClosed
	}
	retried := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !s.owned {
			err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			switch {
			case err == nil:
				s.owned = true
				if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("tailcat: remove stale proxy endpoint: %w", err)
				}
			case errors.Is(err, syscall.EWOULDBLOCK):
			default:
				return nil, fmt.Errorf("tailcat: lock device transport: %w", err)
			}
		}
		if s.owned {
			return s.dialOwned(ctx, addr)
		}
		endpoint, err := s.readEndpoint()
		if err == nil {
			if endpoint.ControlURL != s.options.ControlURL || endpoint.Hostname != s.options.Hostname {
				return nil, fmt.Errorf("tailcat: the device state is in use with another coordination URL or hostname; use the same settings or a separate state directory")
			}
			dialer, err := proxy.SOCKS5("tcp", endpoint.Address,
				&proxy.Auth{User: "tsnet", Password: endpoint.Password}, &net.Dialer{})
			if err != nil {
				return nil, fmt.Errorf("tailcat: configure shared proxy: %w", err)
			}
			conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
			if err != nil && !retried {
				retried = true
				continue
			}
			return conn, err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("tailcat: read shared proxy endpoint: %w", err)
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *SharedDevice) dialOwned(ctx context.Context, addr string) (net.Conn, error) {
	if s.node == nil {
		node, err := s.newNode(s.options)
		if err != nil {
			return nil, err
		}
		s.node = node
	}
	conn, err := s.node.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	if s.published {
		return conn, nil
	}
	address, password, err := s.node.proxy()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tailcat: start shared proxy: %w", err)
	}
	data, err := json.Marshal(proxyEndpoint{
		Version: 1, Hostname: s.options.Hostname, ControlURL: s.options.ControlURL,
		Address: address, Password: password,
	})
	if err == nil {
		err = durablefile.Write(s.path, data)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("tailcat: publish shared proxy endpoint: %w", err)
	}
	s.published = true
	return conn, nil
}

func (s *SharedDevice) readEndpoint() (proxyEndpoint, error) {
	var endpoint proxyEndpoint
	file, err := os.Open(s.path)
	if err != nil {
		return endpoint, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return endpoint, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return endpoint, fmt.Errorf("proxy endpoint must be an owner-only regular file of at most 4096 bytes")
	}
	if err := json.NewDecoder(io.LimitReader(file, 4097)).Decode(&endpoint); err != nil {
		return endpoint, err
	}
	address, err := netip.ParseAddrPort(endpoint.Address)
	if err != nil || !address.Addr().IsLoopback() || address.Port() == 0 || endpoint.Version != 1 || len(endpoint.Password) != 32 {
		return endpoint, fmt.Errorf("invalid shared proxy endpoint")
	}
	return endpoint, nil
}

func (s *SharedDevice) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.owned {
		if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = removeErr
		}
		if s.node != nil {
			err = errors.Join(err, s.node.Close())
		}
	}
	return errors.Join(err, s.lock.Close())
}
