package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/transport/tailcat"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/install"
	"github.com/nibzard/ohya/internal/setup"
)

var setupSpec = commandSpec{
	name: "ohya setup", synopsis: "configure local memory and installed agents", usageLine: "ohya setup [flags]",
	description: "Configures a user service for local ingestion and registers ohya with installed agents. Preserves unrelated settings and reports each result. Rerun to repair owned configuration. Use --dry-run to inspect changes without writing files. Pi 0.84.2 uses an owned compatibility extension when native MCP is unavailable. Remote setup accepts --join-code-env and saves the hub settings for reruns.",
	argsTitle:   "Arguments", args: "(none)", examples: []string{"ohya setup", "ohya setup --dry-run --json"},
}

var uninstallSpec = commandSpec{
	name: "ohya uninstall", synopsis: "remove owned services and agent integrations", usageLine: "ohya uninstall [flags]",
	description: "Stops owned services and removes owned agent entries. Preserves databases, credentials, sessions, and entries edited after setup. An unchanged executable owned by the standalone installer is removed unless --keep-binary is set. Package manager and manual executables are preserved.",
	argsTitle:   "Arguments", args: "(none)", examples: []string{"ohya uninstall --json"},
}

var errSetupIncomplete = errors.New("setup is incomplete; review the component results and rerun ohya setup")

func setupPaths() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	binary, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	binary = stableBinaryPath(binary, os.Args[0])
	return home, binary, nil
}

func stableBinaryPath(actual, invoked string) string {
	candidate, err := exec.LookPath(invoked)
	if err != nil {
		return actual
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return actual
	}
	current, err := os.Stat(actual)
	if err != nil {
		return actual
	}
	target, err := os.Stat(candidate)
	if err == nil && os.SameFile(current, target) {
		return candidate
	}
	return actual
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
	remote := c.fs.Bool("remote", false, "configure this device to use an existing hub")
	local := c.fs.Bool("local", false, "configure local memory instead of a saved remote connection")
	codeEnv := c.fs.String("join-code-env", "", "environment variable holding the one-time join code")
	controlURL := c.fs.String("control", control.DefaultURL(), "hub coordination URL for remote setup")
	hubAddr := c.fs.String("hub-addr", tailcat.DefaultHubAddr(), "hub address inside the tailnet")
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
	connection, err := setup.LoadConnection(absState)
	if err != nil {
		return err
	}
	if *local && (*remote || *codeEnv != "") {
		return c.usageErrorf("--local cannot be combined with remote setup")
	}
	if *local {
		connection = setup.Connection{Mode: "local"}
	}
	if (*remote || *codeEnv != "") && connection.Mode != "remote" {
		connection = setup.Connection{Mode: "remote", ControlURL: *controlURL, HubAddress: *hubAddr}
	}
	c.fs.Visit(func(f *flag.Flag) {
		if f.Name == "control" {
			connection.ControlURL = *controlURL
		}
		if f.Name == "hub-addr" {
			connection.HubAddress = *hubAddr
		}
	})
	if err := connection.Validate(); err != nil {
		return c.usageErrorf("%s", err)
	}
	code := ""
	if *codeEnv != "" {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(*codeEnv) {
			return c.usageErrorf("invalid join code environment variable name")
		}
		if !*dry {
			code = os.Getenv(*codeEnv)
			if code == "" {
				return c.usageErrorf("join code environment variable is empty")
			}
			if _, _, err := control.SplitJoinCode(code); err != nil {
				return c.usageErrorf("join code environment variable does not contain a valid invitation")
			}
			if err := os.Unsetenv(*codeEnv); err != nil {
				return err
			}
		}
	}
	dataState := absState
	db := filepath.Join(absState, "hub.db")
	clientArgs := []string{"mcp", "--db", db, "--state", dataState}
	serviceArgs := []string{"ingest", "--allow-empty", "--db", db, "--state", dataState}
	if connection.Mode == "remote" {
		dataState = filepath.Join(absState, "join")
		db = ""
		clientArgs = []string{"mcp", "--remote", "--state", dataState, "--control", connection.ControlURL, "--hub-addr", connection.HubAddress}
		serviceArgs = []string{"join", "--allow-empty", "--state", dataState, "--control", connection.ControlURL, "--hub-addr", connection.HubAddress}
	}
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
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	options := setup.Options{WorkingDirectory: cwd, NoModifyPath: *noModifyPath, Shell: os.Getenv("SHELL"), Database: db, Clients: setup.ClientOptions{Home: home, State: absState, Binary: binary, Args: clientArgs, Agents: *agents}, ServiceArgs: serviceArgs, NoService: *noService, NoAgents: *noAgents, DryRun: *dry}
	options.Prepare = func(ctx context.Context) error {
		if err := setup.SaveConnection(absState, connection); err != nil {
			return err
		}
		if connection.Mode == "remote" {
			if err := setup.BindRemote(dataState, connection); err != nil {
				return err
			}
			pairCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			return runJoin(pairCtx, joinOptions{pairOnly: true, stateDir: dataState, controlURL: connection.ControlURL, hubAddr: connection.HubAddress, code: code, stderr: stderr})
		}
		return nil
	}
	report, err := setup.Run(ctx, options)
	return printSetupReport(stdout, setupSpec.name, *asJSON, report, err)
}

func runUninstallCommand(args []string, stdout, stderr io.Writer) error {
	home, binary, err := setupPaths()
	if err != nil {
		return err
	}
	c := newCommandFlags(uninstallSpec)
	state := c.fs.String("state", filepath.Join(home, ".ohya"), "memory state `directory`")
	keepBinary := c.fs.Bool("keep-binary", false, "preserve an executable owned by the standalone installer")
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
	if err == nil && report.Complete() && !*keepBinary {
		changed, removeErr := install.Remove(ctx, filepath.Dir(binary))
		result := setup.Result{Component: "binary", Status: "unmanaged", Changed: changed}
		if changed {
			result.Status = "removed"
		}
		if removeErr != nil {
			result.Status = "failed"
			result.Detail = removeErr.Error()
		}
		report.Results = append(report.Results, result)
	}
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
