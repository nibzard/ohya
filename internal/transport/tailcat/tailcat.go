package tailcat

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"

	"github.com/nibzard/ohya/internal/transport"
)

const (
	DefaultHubPort    = 9027
	DefaultBaseDomain = "tail.ohya"
	HubHostname       = "ohya-hub"
)

const dialTimeout = 20 * time.Second

func DefaultHubAddr() string {
	return net.JoinHostPort(HubHostname+"."+DefaultBaseDomain, fmt.Sprintf("%d", DefaultHubPort))
}

type Options struct {
	Hostname string

	AuthKey string

	ControlURL string

	StateDir string

	Port int

	Ephemeral bool
}

func (o Options) validate(hub bool) error {
	if strings.TrimSpace(o.Hostname) == "" {
		return fmt.Errorf("tailcat: Hostname is required")
	}
	if o.ControlURL == "" {
		return fmt.Errorf("tailcat: ControlURL is required")
	}
	u, err := url.Parse(o.ControlURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("tailcat: ControlURL %q is not an absolute URL", o.ControlURL)
	}
	if o.StateDir == "" {
		return fmt.Errorf("tailcat: StateDir is required")
	}
	if hub && o.Port <= 0 || o.Port > 65535 {
		return fmt.Errorf("tailcat: Port must be 1-65535 for a hub, got %d", o.Port)
	}
	return nil
}

type Node struct {
	srv  *tsnet.Server
	port int
	addr string
}

var (
	_ transport.Hub    = (*Node)(nil)
	_ transport.Device = (*Node)(nil)
)

func NewHub(opts Options) (*Node, error) {
	if err := opts.validate(true); err != nil {
		return nil, err
	}
	return &Node{srv: server(opts), port: opts.Port}, nil
}

func NewDevice(opts Options) (*Node, error) {
	if err := opts.validate(false); err != nil {
		return nil, err
	}
	return &Node{srv: server(opts)}, nil
}

func server(opts Options) *tsnet.Server {
	netns.SetEnabled(false)
	return &tsnet.Server{
		Hostname:   opts.Hostname,
		AuthKey:    opts.AuthKey,
		ControlURL: opts.ControlURL,
		Dir:        opts.StateDir,
		Ephemeral:  opts.Ephemeral,
	}
}

func (n *Node) Listen(ctx context.Context) (net.Listener, error) {
	status, err := n.srv.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailcat: bring node up: %w", err)
	}
	if status.Self == nil || status.Self.DNSName == "" {
		return nil, fmt.Errorf("tailcat: node is up but reports no DNS name")
	}
	ln, err := n.srv.Listen("tcp", fmt.Sprintf(":%d", n.port))
	if err != nil {
		return nil, fmt.Errorf("tailcat: listen on tailnet port %d: %w", n.port, err)
	}
	n.addr = net.JoinHostPort(strings.TrimSuffix(status.Self.DNSName, "."), fmt.Sprintf("%d", n.port))
	return ln, nil
}

func (n *Node) Addr() string {
	return n.addr
}

func (n *Node) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if _, err := n.srv.Up(ctx); err != nil {
		return nil, fmt.Errorf("tailcat: bring node up: %w", err)
	}
	if err := n.waitRunning(ctx); err != nil {
		return nil, fmt.Errorf("tailcat: %w", err)
	}
	dialCtx := ctx
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > dialTimeout {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	conn, err := n.srv.Dial(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tailcat: dial %s: %w", addr, err)
	}
	return conn, nil
}

func (n *Node) waitRunning(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lc, err := n.srv.LocalClient()
	if err != nil {
		return fmt.Errorf("wait for the tailnet: local client: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		st, err := lc.Status(waitCtx)
		if err == nil && st.BackendState == "Running" && st.MagicDNSSuffix != "" && len(st.Peer) > 0 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			state := "unavailable"
			suffix := ""
			peers := 0
			if err == nil && st != nil {
				state = st.BackendState
				suffix = st.MagicDNSSuffix
				peers = len(st.Peer)
			}
			return fmt.Errorf("wait for the tailnet: state %s, MagicDNS suffix %q, %d peers: %w", state, suffix, peers, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (n *Node) WhoIs(ctx context.Context, conn net.Conn) (login, hostname string, err error) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return "", "", fmt.Errorf("tailcat: local client: %w", err)
	}
	who, err := lc.WhoIs(ctx, conn.RemoteAddr().String())
	if err != nil {
		return "", "", fmt.Errorf("tailcat: whois %s: %w", conn.RemoteAddr(), err)
	}
	login = who.UserProfile.LoginName
	hostname = who.Node.Name
	return login, hostname, nil
}

func (n *Node) PeerPath(ctx context.Context, conn net.Conn) (relay, curAddr string, err error) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return "", "", fmt.Errorf("tailcat: local client: %w", err)
	}
	who, err := lc.WhoIs(ctx, conn.RemoteAddr().String())
	if err != nil {
		return "", "", fmt.Errorf("tailcat: whois %s: %w", conn.RemoteAddr(), err)
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return "", "", fmt.Errorf("tailcat: status: %w", err)
	}
	ps, ok := st.Peer[key.NodePublic(who.Node.Key)]
	if !ok {
		return "", "", fmt.Errorf("tailcat: peer %s missing from status", who.Node.Key)
	}
	return ps.Relay, ps.CurAddr, nil
}

func (n *Node) Close() error {
	if err := n.srv.Close(); err != nil {
		return fmt.Errorf("tailcat: close node: %w", err)
	}
	return nil
}
