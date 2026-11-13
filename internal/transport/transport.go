// Package transport defines the boundary between the sync layer and the
// device-to-hub network transport. Synchronization code depends only on the
// interfaces here; every transport implementation, starting with the embedded
// Tailcat node (ADR 0002), stays behind them.
//
// The interfaces return ordinary net values, so the sync layer never sees a
// transport-specific type. A hub process accepts Device connections; a device
// process dials the hub at the address the hub published while pairing.
package transport

import (
	"context"
	"net"
)

// Hub is the hub side of a transport. One hub process holds one transport
// node; Listen can be called once per node.
type Hub interface {
	// Listen brings the node up, if needed, and starts accepting device
	// connections. The returned listener and its connections are ordinary
	// net values owned by the caller, which must Close them.
	Listen(ctx context.Context) (net.Listener, error)

	// Addr reports the address devices dial, in host:port form inside the
	// transport's network. It is valid after Listen returns.
	Addr() string

	// Close stops the node and releases its resources. Persisted node
	// state, if any, survives for reuse by a later process.
	Close() error
}

// Device is the device side of a transport.
type Device interface {
	// Dial connects to the hub at the address reported by Hub.Addr and
	// returns an established connection. Dial may block while the node
	// comes up; ctx bounds that wait.
	Dial(ctx context.Context, addr string) (net.Conn, error)

	// Close stops the node and releases its resources. Persisted node
	// state, if any, survives for reuse by a later process.
	Close() error
}
