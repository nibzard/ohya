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
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/address"
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
		"serve the tools from the joined hub over the tailcat transport instead of a local database")
	observeFlag := c.fs.String("observe", "on",
		"publish local snapshots and metrics under the state directory's observe/ tree: on or off")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	observeOn, err := parseObserveFlag(*observeFlag)
	if err != nil {
		return c.usageErrorf("%s", err.Error())
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
		return runMcpRemote(ctx, stateDir, observeOn)
	}
	return runMcp(ctx, *db, stateDir, observeOn)
}

func runMcp(ctx context.Context, dbPath, stateDir string, observeOn bool) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	telem := startTelemetry(stateDir, observeOn, sqliteSampleFiles(dbPath), observe.RoleMCP)
	defer telem.close()
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
	server := mcpserver.New(mcpserver.NewEngine(readers, hub.Writer(), mcpserver.EngineOptions{WriterID: deviceID, Usage: telem.usageSink(), Observer: telem.observer()}),
		mcpserver.EngineOptions{WriterID: deviceID})
	return mcpserver.Run(ctx, server)
}

func runMcpRemote(ctx context.Context, stateDir string, observeOn bool) error {
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
	hubAddr, err := device.LoadHubAddress(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("this state directory holds no hub address; run ohya join with an ohya1 code once to save it")
	}
	if err != nil {
		return err
	}
	telem := startTelemetry(stateDir, observeOn, nil, observe.RoleMCP)
	defer telem.close()
	node := address.NewDevice(address.DeviceOptions{})
	session := hubsync.NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) { return node.Dial(ctx, hubAddr) },
		cred.Secret,
		hubsync.RemoteSessionOptions{DeviceID: deviceID, Usage: telem.usageSink(), Observer: telem.observer()})
	defer session.Close()
	server := mcpserver.New(session, mcpserver.EngineOptions{WriterID: deviceID})
	err = mcpserver.Run(ctx, server)
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = node.Drain(drainCtx)
	cancel()
	if cerr := node.Close(); err == nil {
		err = cerr
	}
	return err
}
