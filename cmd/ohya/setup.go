package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nibzard/ohya/internal/setup"
)

var setupSpec = commandSpec{
	name: "ohya setup", synopsis: "configure local memory and installed agents", usageLine: "ohya setup [flags]",
	description: "Configures a user service for local ingestion and registers ohya with installed agents. Preserves unrelated settings and reports each result. Rerun to repair owned configuration. Use --dry-run to inspect changes without writing files. Older Pi compatibility and remote setup are still under development.",
	argsTitle:   "Arguments", args: "(none)", examples: []string{"ohya setup", "ohya setup --dry-run --json"},
}

var uninstallSpec = commandSpec{
	name: "ohya uninstall", synopsis: "remove owned services and agent integrations", usageLine: "ohya uninstall [flags]",
	description: "Stops owned services and removes owned agent entries. Preserves databases, credentials, sessions, and entries edited after setup. Executable removal is handled by the installer or package manager.",
	argsTitle:   "Arguments", args: "(none)", examples: []string{"ohya uninstall --json"},
}

var errSetupIncomplete = errors.New("setup is incomplete; review the component results and rerun ohya setup")

func setupPaths() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	binary, err := os.Executable()
	return home, binary, err
}

func runSetupCommand(args []string, stdout, stderr io.Writer) error {
	home, binary, err := setupPaths()
	if err != nil {
		return err
	}
	c := newCommandFlags(setupSpec)
	binaryPath := c.fs.String("binary", binary, "absolute executable `path` registered with services and agents")
	state := c.fs.String("state", filepath.Join(home, ".ohya"), "memory state `directory`")
	agents := c.fs.String("agents", "auto", "agents to configure: auto or comma-separated claude,codex,pi")
	noAgents := c.fs.Bool("no-agents", false, "skip agent configuration")
	noModifyPath := c.fs.Bool("no-modify-path", false, "leave shell profiles unchanged")
	noService := c.fs.Bool("no-service", false, "skip the background service")
	dry := c.fs.Bool("dry-run", false, "report planned changes without writing files")
	asJSON := c.fs.Bool("json", false, "print one versioned JSON result")
	roots := newAgentRootFlags(c.fs, home)
	if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" {
		*roots.claude = filepath.Join(config, "projects")
	}
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	absState, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	db := filepath.Join(absState, "hub.db")
	serviceArgs := []string{"ingest", "--allow-empty", "--db", db, "--state", absState}
	for _, root := range []struct {
		name string
		dir  *string
	}{{"claude-root", roots.claude}, {"codex-root", roots.codex}, {"pi-root", roots.pi}} {
		dir := *root.dir
		if dir != "" {
			dir, err = filepath.Abs(dir)
			if err != nil {
				return err
			}
		}
		serviceArgs = append(serviceArgs, "--"+root.name, dir)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	binary, err = filepath.Abs(*binaryPath)
	if err != nil {
		return err
	}
	options := setup.Options{NoModifyPath: *noModifyPath, Shell: os.Getenv("SHELL"), Database: db, Clients: setup.ClientOptions{Home: home, State: absState, Binary: binary, Args: []string{"mcp", "--db", db, "--state", absState}, Agents: *agents}, ServiceArgs: serviceArgs, NoService: *noService, NoAgents: *noAgents, DryRun: *dry}
	report, err := setup.Run(ctx, options)
	return printSetupReport(stdout, setupSpec.name, *asJSON, report, err)
}

func runUninstallCommand(args []string, stdout, stderr io.Writer) error {
	home, _, err := setupPaths()
	if err != nil {
		return err
	}
	c := newCommandFlags(uninstallSpec)
	state := c.fs.String("state", filepath.Join(home, ".ohya"), "memory state `directory`")
	asJSON := c.fs.Bool("json", false, "print one versioned JSON result")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	absState, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := setup.Remove(ctx, absState, nil)
	return printSetupReport(stdout, uninstallSpec.name, *asJSON, report, err)
}

func printSetupReport(stdout io.Writer, command string, asJSON bool, report setup.Report, err error) error {
	if err != nil {
		return jsonFailurePrint(asJSON, stdout, command, err)
	}
	if asJSON {
		if err := printJSON(stdout, command, report); err != nil {
			return err
		}
	} else {
		for _, r := range report.Results {
			fmt.Fprintf(stdout, "ohya: %s: %s", r.Component, r.Status)
			if r.Detail != "" {
				fmt.Fprintf(stdout, " — %s", strings.ReplaceAll(r.Detail, "\n", " "))
			}
			fmt.Fprintln(stdout)
		}
	}
	if !report.Complete() {
		return errSetupIncomplete
	}
	return nil
}
