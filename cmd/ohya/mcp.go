package main

import (
	"context"
	"errors"
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

var mcpSpec = commandSpec{
	name:      "ohya mcp",
	synopsis:  "serve the sql and annotate MCP tools on stdio",
	usageLine: "ohya mcp [flags]",
	description: "Serves the read-only sql(query) and bounded annotate(operations) MCP tools to any MCP client over stdio. " +
		"Without --remote the tools run against the local hub database; with --remote they run on the joined hub through the authenticated sync connection, and the hub derives the annotation writer from the device identity. " +
		"Register the command with the client, for example args [\"mcp\"] for Claude Code.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya mcp", "ohya mcp --remote"},
}

func runMcpCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(mcpSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` (ignored with --remote)")
	state := c.fs.String("state", "",
		"state `directory` for the device identity; defaults to the database directory, with --remote to the join state directory")
	remote := c.fs.Bool("remote", false,
		"serve the tools from the joined hub over the tailnet instead of a local database")
	hubAddr := c.fs.String("hub-addr", tailcat.DefaultHubAddr(),
		"hub `address` inside the tailnet (--remote only)")
	controlURL := c.fs.String("control", control.DefaultURL(),
		"coordination `URL` of the hub's tailnet (--remote only)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
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
