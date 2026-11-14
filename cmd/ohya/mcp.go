// The mcp subcommand: serve the sql(query) and annotate(operations) MCP
// tools over stdio. Locally, the tools are the agent-facing surface of the
// hub database — queries run through restricted reader connections guarded
// by a SQLite authorizer, annotation batches commit through the single
// writer, and both stay bounded (internal/mcpserver). With --remote, a
// joined device serves the same tools from its hub over the authenticated
// sync connection (ADR 0003): the hub runs the same bounded engine, derives
// the annotation writer from the device's credential, and checks revocation
// on every frame.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/tailcat"
)

// runMcpCommand parses the mcp flags and serves the MCP tools on stdio until
// the process receives SIGINT or SIGTERM or stdin closes. stdout carries only
// protocol frames; every diagnostic goes to stderr.
func runMcpCommand(args []string, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	fs := flag.NewFlagSet("ohya mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` (ignored with --remote)")
	state := fs.String("state", "",
		"state `directory` for the device identity (default: the database directory; with --remote: the join state directory)")
	remote := fs.Bool("remote", false,
		"serve the tools from the joined hub over the tailnet instead of a local database")
	hubAddr := fs.String("hub-addr", tailcat.DefaultHubAddr(),
		"hub `address` inside the tailnet (--remote only)")
	controlURL := fs.String("control", control.DefaultURL(),
		"coordination `URL` of the hub's tailnet (--remote only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	stateDir := *state
	if *remote {
		if stateDir == "" {
			stateDir = filepath.Join(home, ".ohya", "join")
		}
	} else if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *remote {
		return runMcpRemote(ctx, *hubAddr, *controlURL, stateDir)
	}
	return runMcp(ctx, *db, stateDir)
}

// runMcp opens the hub and serves the tools until ctx is done. The writer
// stays open alongside the readers: it applies pending migrations, serves
// annotation batches, and a read-only connection cannot create the database
// or recover the write-ahead log. The sql tool itself never writes.
func runMcp(ctx context.Context, dbPath, stateDir string) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer hub.Close()
	readers, err := hub.Reader()
	if err != nil {
		return err
	}
	defer readers.Close()
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		return err
	}
	server := mcpserver.New(mcpserver.NewEngine(readers, hub.Writer(), mcpserver.EngineOptions{WriterID: deviceID}),
		mcpserver.EngineOptions{WriterID: deviceID})
	return mcpserver.Run(ctx, server)
}

// runMcpRemote serves the tools from the joined hub: the device's transport
// node dials the hub, the pairing credential authenticates one connection,
// and every tool call rides it as a bounded query or annotate exchange. The
// hub derives the annotation writer from the credential, and a revocation
// closes the connection at its next frame.
func runMcpRemote(ctx context.Context, hubAddr, controlURL, stateDir string) error {
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		return err
	}
	cred, err := device.LoadCredential(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("this state directory holds no pairing credential; run ohya join first")
	}
	if err != nil {
		return err
	}
	node, err := tailcat.NewDevice(tailcat.Options{
		Hostname:   deviceHostname(deviceID),
		ControlURL: controlURL,
		StateDir:   filepath.Join(stateDir, "tailcat"),
	})
	if err != nil {
		return err
	}
	defer node.Close()
	session := hubsync.NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, hubAddr) },
		cred.Secret)
	server := mcpserver.New(session, mcpserver.EngineOptions{WriterID: deviceID})
	return mcpserver.Run(ctx, server)
}
