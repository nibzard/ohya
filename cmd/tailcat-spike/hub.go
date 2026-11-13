// Hub mode: bring up the transport, publish the dialable address, and serve
// batch/acknowledgment exchanges until stopped.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/nibzard/ohya/internal/transport"
)

// hubResult is the hub-side record of a spike run.
type hubResult struct {
	Transport      string `json:"transport"`
	ListenMs       int64  `json:"listen_ms"`
	Addr           string `json:"addr"`
	Connections    int    `json:"connections"`
	LastPeer       string `json:"last_peer,omitempty"`
	CommittedBytes uint64 `json:"committed_bytes"`
	FinalCursor    uint64 `json:"final_cursor"`
	RSSBytes       uint64 `json:"rss_bytes"`
	StateDirBytes  int64  `json:"state_dir_bytes"`
	Errors         int    `json:"errors"`
	LastError      string `json:"last_error,omitempty"`
}

// identify reports the authenticated peer of a connection, or a placeholder
// when the transport has no peer identity (the TCP baseline).
func identify(ctx context.Context, conn net.Conn, hub transport.Hub) string {
	type whoiser interface {
		WhoIs(ctx context.Context, conn net.Conn) (login, hostname string, err error)
	}
	if w, ok := hub.(whoiser); ok {
		login, hostname, err := w.WhoIs(ctx, conn)
		if err != nil {
			return fmt.Sprintf("unknown peer (%v)", err)
		}
		return fmt.Sprintf("%s (%s)", login, hostname)
	}
	return "unauthenticated (transport has no peer identity)"
}

func runHub(f flags) {
	ctx, stop := stopOnSignal()
	defer stop()

	hub, err := newHubTransport(f)
	if err != nil {
		log.Fatalf("hub: %v", err)
	}
	defer hub.Close()

	start := time.Now()
	listenCtx, cancel := context.WithTimeout(ctx, f.Timeout)
	var ln net.Listener
	ln, err = hub.Listen(listenCtx)
	cancel()
	if err != nil {
		log.Fatalf("hub listen: %v", err)
	}
	res := hubResult{
		Transport: f.Transport,
		ListenMs:  time.Since(start).Milliseconds(),
		Addr:      hub.Addr(),
	}
	log.Printf("hub listening on %s (up in %d ms)", res.Addr, res.ListenMs)

	// Accept does not observe ctx cancellation; close the listener to
	// unblock it and let the results be written on shutdown.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for ctx.Err() == nil {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			res.Errors++
			res.LastError = err.Error()
			log.Printf("accept: %v", err)
			continue
		}
		res.Connections++
		peer := identify(ctx, conn, hub)
		res.LastPeer = peer
		log.Printf("connection %d from %s", res.Connections, peer)
		c := &hubConn{r: conn, w: conn, digest: sha256.New()}
		cursor, err := c.serve()
		res.CommittedBytes += cursor
		res.FinalCursor = cursor
		if err != nil && !errors.Is(err, errClosed) {
			res.Errors++
			res.LastError = err.Error()
			log.Printf("connection ended with error: %v", err)
		}
		conn.Close()
		log.Printf("connection committed %d bytes, cursor %d", cursor, cursor)
	}

	res.RSSBytes = readRSS()
	res.StateDirBytes = dirSize(f.StateDir)
	writeResults(f.Out, res)
	log.Printf("hub done: %d connections, %d bytes committed, rss %d MiB, state %d KiB",
		res.Connections, res.CommittedBytes, res.RSSBytes>>20, res.StateDirBytes>>10)
}
