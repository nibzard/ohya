// The mcp subcommand: serve the read-only sql(query) MCP tool over stdio.
// The tool is one of two surfaces into the hub database — ingestion writes
// through the single writer, agents read through restricted reader
// connections guarded by a SQLite authorizer (internal/mcpserver).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
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
		"hub database `path`")
	state := fs.String("state", "",
		"state `directory` for the device identity (default: the database directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runMcp(ctx, *db, stateDir)
}

// runMcp opens the hub and serves the sql tool until ctx is done. The writer
// stays open alongside the readers: it applies pending migrations, and a
// read-only connection cannot create the database or recover the write-ahead
// log. The tool itself never writes.
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
	server := mcpserver.New(readers, mcpserver.EngineOptions{WriterID: deviceID})
	return mcpserver.Run(ctx, server)
}
