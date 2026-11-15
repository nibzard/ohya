package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/hubsync"
)

type pairTestDevice struct{ conn net.Conn }

func (d pairTestDevice) Dial(context.Context, string) (net.Conn, error) { return d.conn, nil }
func (d pairTestDevice) Close() error                                   { return nil }

func TestPairRejectsWrongDevice(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		_, _, err := hubsync.ReadAuthFrame(server)
		if err == nil {
			err = hubsync.WriteCredentialReply(server, hubsync.CredentialReply{DeviceID: "other", Credential: "secret"})
		}
		done <- err
	}()
	_, err := pair(context.Background(), pairTestDevice{client}, "hub", "invitation", "expected")
	if err == nil || !strings.Contains(err.Error(), "not expected") {
		t.Fatalf("pair error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
