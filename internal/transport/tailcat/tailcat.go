// Package tailcat implements the transport.Hub and transport.Device
// interfaces with an embedded Tailcat node (tailscale.com/tsnet, the client
// library pinned in ADR 0002).
//
// One process holds one node. The hub node listens on a fixed tailnet port;
// device nodes dial the hub's address, which the hub publishes while pairing.
// Both roles bring the node up before use, so a rejected identity or pairing
// key fails the call with the coordination server's reason instead of
// blocking until the context deadline.
package tailcat

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"tailscale.com/tsnet"

	"github.com/nibzard/ohya/internal/transport"
)

// Options configures one embedded node. Every field is required for both
// roles unless its comment says otherwise.
type Options struct {
	// Hostname is the node's name inside the tailnet. It must be stable
	// across runs: the hub's dialed address refers to it.
	Hostname string

	// AuthKey pairs the node with the coordination server exactly once. A
	// node whose StateDir already holds an enrolled identity ignores it,
	// so restarts consume no new key.
	AuthKey string

	// ControlURL is the coordination server, for example a self-hosted
	// Headscale instance or the vendor's control plane.
	ControlURL string

	// StateDir persists the node identity and connection state. Losing it
	// orphans the node on the coordination server, like a lost device
	// identity (see internal/device).
	StateDir string

	// Port is the tailnet port the hub listens on; devices dial it.
	// Required for hubs, ignored by devices.
	Port int

	// Ephemeral marks the node disposable: the coordination server expires
	// it soon after disconnect, orphaning its state so the next start needs
	// a new pairing key. An ohya hub or device is durable — restarts must
	// reuse the enrolled identity — so leave this unset. Optional.
	Ephemeral bool
}

// validate reports whether the options are usable for the given role.
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

// Node is one embedded Tailcat node. A Node built by NewHub serves
// transport.Hub; one built by NewDevice serves transport.Device. The sync
// layer consumes the interfaces; WhoIs is available to callers that hold
// the concrete type and need the authenticated peer of a connection.
type Node struct {
	srv  *tsnet.Server
	port int
	addr string
}

// Compile-time checks that Node serves both transport roles.
var (
	_ transport.Hub    = (*Node)(nil)
	_ transport.Device = (*Node)(nil)
)

// NewHub builds a hub node. Call Listen before devices dial; Addr reports
// the dialable address afterwards.
func NewHub(opts Options) (*Node, error) {
	if err := opts.validate(true); err != nil {
		return nil, err
	}
	return &Node{srv: server(opts), port: opts.Port}, nil
}

// NewDevice builds a device node.
func NewDevice(opts Options) (*Node, error) {
	if err := opts.validate(false); err != nil {
		return nil, err
	}
	return &Node{srv: server(opts)}, nil
}

// server builds the underlying tsnet server from validated options.
func server(opts Options) *tsnet.Server {
	return &tsnet.Server{
		Hostname:   opts.Hostname,
		AuthKey:    opts.AuthKey,
		ControlURL: opts.ControlURL,
		Dir:        opts.StateDir,
		Ephemeral:  opts.Ephemeral,
	}
}

// Listen brings the node up and listens on the configured tailnet port.
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

// Addr reports the hub's dialable address after Listen.
func (n *Node) Addr() string {
	return n.addr
}

// Dial brings the node up and connects to the hub at addr.
func (n *Node) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if _, err := n.srv.Up(ctx); err != nil {
		return nil, fmt.Errorf("tailcat: bring node up: %w", err)
	}
	conn, err := n.srv.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tailcat: dial %s: %w", addr, err)
	}
	return conn, nil
}

// WhoIs reports the authenticated tailnet identity of the peer on a
// connection accepted by this node. The tailnet's cryptographic peer
// authentication backs it: only paired devices hold keys the coordination
// server accepted.
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

// Close stops the node.
func (n *Node) Close() error {
	if err := n.srv.Close(); err != nil {
		return fmt.Errorf("tailcat: close node: %w", err)
	}
	return nil
}
