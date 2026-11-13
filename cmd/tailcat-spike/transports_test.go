package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

// TestTCPBaselineExchange runs the whole spike protocol end to end through
// the adapter interfaces on the TCP baseline: a hub accepts one device
// connection, and verified batch/acknowledgment exchanges advance the
// cursor. The Tailcat transport runs the same protocol; its end-to-end run
// needs a coordination server and is driven by the recorded spike instead
// (ADR 0002).
func TestTCPBaselineExchange(t *testing.T) {
	hub, err := newTCPHub()
	if err != nil {
		t.Fatalf("newTCPHub: %v", err)
	}
	defer hub.Close()

	type connResult struct {
		cursor uint64
		err    error
	}
	served := make(chan connResult, 1)
	go func() {
		ln, err := hub.Listen(context.Background())
		if err != nil {
			served <- connResult{err: err}
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			served <- connResult{err: err}
			return
		}
		c := &hubConn{r: conn, w: conn, digest: sha256.New()}
		cursor, err := c.serve()
		conn.Close()
		served <- connResult{cursor: cursor, err: err}
	}()

	device := tcpDevice{}
	conn, err := device.Dial(context.Background(), hub.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	b := newBatcher()
	for _, size := range []int{1, 4096, 65536} {
		if _, _, err := b.exchange(conn, size); err != nil {
			t.Fatalf("exchange %d: %v", size, err)
		}
	}
	conn.Close()

	res := <-served
	if res.err != nil && !errors.Is(res.err, errClosed) {
		t.Fatalf("hub serve: %v", res.err)
	}
	if res.cursor != 1+4096+65536 {
		t.Fatalf("hub cursor = %d, want %d", res.cursor, 1+4096+65536)
	}
	if addr := hub.Addr(); addr == "" {
		t.Fatal("hub Addr is empty after Listen")
	}
}
