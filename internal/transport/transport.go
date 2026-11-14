package transport

import (
	"context"
	"net"
)

type Hub interface {
	Listen(ctx context.Context) (net.Listener, error)

	Addr() string

	Close() error
}

type Device interface {
	Dial(ctx context.Context, addr string) (net.Conn, error)

	Close() error
}
