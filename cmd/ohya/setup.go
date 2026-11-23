package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/setup"
)

var setupSpec = commandSpec{
	name: "ohya setup", synopsis: "configure local memory and installed agents", usageLine: "ohya setup [flags]",
	description: "Configures a user service for local ingestion and registers ohya with installed agents. Preserves unrelated settings and reports each result. Rerun to repair owned configuration. Use --dry-run to inspect changes without writing files. Pi 0.84.2 uses an owned compatibility extension when native MCP is unavailable. Remote setup accepts --join-code-env and saves the hub settings for reruns.\nUse --hub to also run this machine as the hub for other devices: it adds an owned ohya serve user service, creates the Tailcat identity, checks that a relay answers, and prints a one-time join code with the device command.",
	examples:    []string{"ohya setup", "ohya setup --hub", "ohya setup --dry-run --json"},
}

var uninstallSpec = commandSpec{
	name: "ohya uninstall", synopsis: "remove owned services and agent integrations", usageLine: "ohya uninstall [flags]",
	description: "Stops owned services and removes owned agent entries. Preserves databases, credentials, sessions, and entries edited after setup. An unchanged executable owned by the standalone installer is removed unless --keep-binary is set. Package manager and manual executables are preserved.",
	examples:    []string{"ohya uninstall --json"},
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
	agents := c.fs.String("agents", "auto", "agents to configure: auto or comma-separated claude,codex,pi,agy")
	remote := c.fs.Bool("remote", false, "configure this device to use an existing hub")
	local := c.fs.Bool("local", false, "configure local memory instead of a saved remote connection")
	codeEnv := c.fs.String("join-code-env", "", "environment variable holding the one-time join code")
	hubMode := c.fs.Bool("hub", false, "also run this machine as the hub: an owned ohya serve service, a relay check, and a one-time join code with the device command")
	ttl := c.fs.Duration("ttl", registry.DefaultInvitationTTL, "how long the join code printed by --hub stays consumable")
	noAgents := c.fs.Bool("no-agents", false, "skip agent configuration")
	noModifyPath := c.fs.Bool("no-modify-path", false, "leave shell profiles unchanged")
	noService := c.fs.Bool("no-service", false, "skip the background service")
	dry := c.fs.Bool("dry-run", false, "report planned changes without writing files")
	asJSON := c.fs.Bool("json", false, "print one versioned JSON result")
	roots := newAgentRootFlags(c.fs, home)
	excludes := newExcludeFlag(c.fs)
	if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" {
		*roots.claude = filepath.Join(config, "projects")
	}
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *ttl <= 0 {
		return c.usageErrorf("--ttl must be positive")
	}
	if *hubMode && (*remote || *codeEnv != "") {
		return c.usageErrorf("--hub cannot be combined with remote setup; the hub machine runs local memory")
	}
	if *hubMode && *noService {
		return c.usageErrorf("--hub installs the ohya serve service; omit --no-service")
	}
	absState, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	preferences, err := setupPreferences(c.fs, absState)
	if err != nil {
		return err
	}
	connection, err := setup.LoadConnection(absState)
	if err != nil {
		return err
	}
	if *hubMode && connection.Mode == "remote" {
		return c.usageErrorf("this setup state is bound to a remote hub; a hub machine needs its own state directory with local memory")
	}
	if *local && (*remote || *codeEnv != "") {
		return c.usageErrorf("--local cannot be combined with remote setup")
	}
	if *local {
		connection = setup.Connection{Mode: "local"}
	}
	if (*remote || *codeEnv != "") && connection.Mode != "remote" {
		connection = setup.Connection{Mode: "remote"}
	}
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
			if _, _, err := parseJoinCode(code); err != nil {
				return c.usageErrorf("join code environment variable does not hold one: %v", err)
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
		clientArgs = []string{"mcp", "--remote", "--state", dataState}
		serviceArgs = []string{"join", "--allow-empty", "--state", dataState}
	}
	for _, root := range []struct {
		name string
		dir  *string
	}{{"claude-root", roots.claude}, {"codex-root", roots.codex}, {"pi-root", roots.pi}, {"antigravity-root", roots.antigravity}} {
		dir := *root.dir
		if dir != "" {
			dir, err = filepath.Abs(dir)
			if err != nil {
				return err
			}
		}
		preferences.Flags[root.name] = dir
		serviceArgs = append(serviceArgs, "--"+root.name, dir)
	}
	for _, glob := range *excludes {
		serviceArgs = append(serviceArgs, "--exclude", glob)
	}
	if err := preferences.Validate(); err != nil {
		return err
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
	options := setup.Options{WorkingDirectory: cwd, NoModifyPath: *noModifyPath, Shell: os.Getenv("SHELL"), Database: db, Clients: setup.ClientOptions{Home: home, State: absState, Binary: binary, Args: clientArgs, Agents: *agents, Getenv: func(name string) string { return preferences.Homes[name] }}, ServiceArgs: serviceArgs, NoService: *noService, NoAgents: *noAgents, DryRun: *dry}
	if run, loaded := serviceRunners(); run != nil {
		options.RunService, options.LoadedService = run, loaded
	}
	if *hubMode {
		options.HubServiceArgs = hubServiceArgs(absState, filepath.Join(absState, "hub.db"))
		options.CheckHub = func(ctx context.Context) error {
			return checkHubSetup(ctx, absState)
		}
	}
	options.Prepare = func(ctx context.Context) error {
		if err := setup.SavePreferences(absState, preferences); err != nil {
			return err
		}
		if connection.Mode == "remote" {
			if err := setup.CheckRemoteBinding(dataState, connection); err != nil {
				return err
			}
			pairCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if err := runJoin(pairCtx, joinOptions{pairOnly: true, stateDir: dataState, code: code, stderr: stderr}); err != nil {
				return err
			}
			if err := pairCtx.Err(); err != nil {
				return fmt.Errorf("pair with the hub: %w", err)
			}
			if err := setup.BindRemote(dataState, connection); err != nil {
				return err
			}
		}
		return setup.SaveConnection(absState, connection)
	}
	options.Progress = func(line string) { fmt.Fprintf(stderr, "ohya: %s\n", line) }
	printed := 0
	if !*asJSON {
		options.Notify = func(r setup.Result) {
			printSetupResult(stdout, r)
			printed++
		}
	}
	report, err := setup.Run(ctx, options)
	if conflict := setupPathConflict(binary); conflict != nil {
		report.Results = append(report.Results, *conflict)
	}
	if err == nil && report.Complete() && *hubMode && !*dry {
		invite, hubErr := finishHubSetup(absState, *ttl)
		if hubErr != nil {
			return printSetupReport(stdout, setupSpec.name, *asJSON, report, printed, hubErr)
		}
		report.Hub = invite
	}
	return printSetupReport(stdout, setupSpec.name, *asJSON, report, printed, err)
}

var serviceRunnersHook func() (run func(context.Context, []string) error, loaded func(context.Context, []string) error)

func serviceRunners() (func(context.Context, []string) error, func(context.Context, []string) error) {
	if serviceRunnersHook != nil {
		return serviceRunnersHook()
	}
	return nil, nil
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
	return printSetupReport(stdout, uninstallSpec.name, *asJSON, report, 0, err)
}

const (
	setupComponentWidth = 14
	setupStatusWidth    = 11
)

func printSetupResult(stdout io.Writer, r setup.Result) {
	s := styleFor(stdout)
	mark, status := s.dim("·"), r.Status
	switch r.Status {
	case "configured", "created", "started", "restarted", "removed", "updated", "running", "ready", "saved":
		mark, status = s.boldGreen("✓"), s.green(r.Status)
	case "unchanged", "skipped", "unmanaged", "not needed", "absent":
		status = s.dim(r.Status)
	case "failed":
		mark, status = s.boldRed("✗"), s.boldRed(r.Status)
	case "planned":
		mark, status = s.cyan("○"), s.cyan(r.Status)
	default:
		mark, status = s.boldYellow("▲"), s.yellow(r.Status)
	}
	line := fmt.Sprintf("  %s %s %s", mark, padRight(s.bold(r.Component), setupComponentWidth), padRight(status, setupStatusWidth))
	if r.Detail != "" {
		line += " " + strings.ReplaceAll(r.Detail, "\n", " ")
	}
	fmt.Fprintln(stdout, strings.TrimRight(line, " "))
}

func printSetupReport(stdout io.Writer, command string, asJSON bool, report setup.Report, printed int, err error) error {
	if err != nil {
		return jsonFailurePrint(asJSON, stdout, command, err)
	}
	if asJSON {
		if err := printJSON(stdout, command, report); err != nil {
			return err
		}
	} else {
		for _, r := range report.Results[printed:] {
			printSetupResult(stdout, r)
		}
		if report.Hub != nil {
			printHubJoin(stdout, *report.Hub)
		}
	}
	if !report.Complete() {
		return errSetupIncomplete
	}
	return nil
}
